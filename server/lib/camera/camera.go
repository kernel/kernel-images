// Package camera passes a live-view viewer's real webcam into the browser as a
// local camera device, on demand.
//
// Topology (all inside the browser VM):
//
//	viewer's camera ──WebRTC──▶ neko ──▶ /dev/video1 (ingest, v4l2sink)
//	                                         │
//	                                      Manager mixer (this package)
//	                                         ▼
//	chromium getUserMedia ◀── /dev/video0 (the camera pages see)
//
// The Manager is the single writer of the browser-facing device so chromium
// never loses its video source: it streams a placeholder frame until the
// viewer shares their camera, then switches to the live ingest frames, then
// back to the placeholder when sharing stops. It also reports, over a small
// HTTP endpoint, whether a page in the browser currently has the camera open,
// so the live-view client can start and stop the viewer's camera to match
// (camera on demand).
//
// Scope: this moves a real person's real camera into the browser. It is not a
// way to feed synthetic or recorded video to an identity check. The device and
// the feed are made to look like what they genuinely are to the person sharing
// — a real camera, attached locally — so that camera checks meant to catch
// *injected* video do not false-positive on a *relayed* live person.
package camera

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Defaults for the capture pipeline. Width/Height are the fixed native mode of
// the ingest side (neko's webcam pipeline is hardcoded to 1280x720 and the
// client always sends an upright landscape frame); the browser-facing device
// negotiates its own mode with chromium, which the mixer follows.
const (
	DefaultOutputDevice = "/dev/video0"
	// DefaultIngestDevice is the node neko and the ingest reader use. It is
	// deliberately NOT under /dev/video*: chromium enumerates cameras by
	// scanning that pattern, and everything in the image runs as root, so
	// permissions alone cannot hide the ingest device from a page. The
	// kernel registers it as video1 (DefaultIngestVideoNr, for sysfs lookups
	// and module args); camerad moves the node here at startup.
	DefaultIngestDevice  = "/dev/kernel-camera/ingest"
	DefaultIngestVideoNr = 1
	DefaultWidth         = 1280
	DefaultHeight        = 720
	DefaultFPS           = 30

	// MaxWidth/MaxHeight bound what the loopback device accepts; the mixer
	// opens the device at this size at boot so any size a page later
	// negotiates fits the already-allocated buffers (a capture-side format
	// change cannot reallocate them while we stream).
	MaxWidth  = 1920
	MaxHeight = 1080

	// ModuleDir is where the image ships the camera kernel modules (V4L2
	// core + the camera-persona v4l2loopback, built for the guest kernel by
	// images/chromium-headful/camera). When present, they are loaded with
	// finit_module(2) directly — modprobe/kmod need not exist in the image.
	ModuleDir = "/usr/lib/kernel-camera"
)

// State is the camera-on-demand signal the live-view client polls.
type State struct {
	// Wanted is true while a page inside the browser holds the camera open.
	Wanted bool `json:"wanted"`
	// Streaming is true while the viewer's camera is reaching the ingest
	// device with fresh frames (as opposed to the placeholder).
	Streaming bool `json:"streaming"`
	// Width and Height are the browser-facing device's native mode, so the
	// client can request getUserMedia at a matching aspect ratio.
	Width  int `json:"width"`
	Height int `json:"height"`
}

// stateHandler serves the current State as JSON. snapshot is supplied by the
// Manager so the handler holds no state of its own.
func stateHandler(snapshot func() State) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(snapshot())
	}
}

// loopbackModuleArgs returns the modprobe arguments that create the ingest and
// browser-facing loopback devices as a pair, with exclusive capture caps so a
// device only advertises a camera to chromium while a producer is attached.
// outNr and ingestNr are the /dev/videoN numbers for the two devices.
func loopbackModuleArgs(outNr, ingestNr int) []string {
	return []string{
		"v4l2loopback",
		"devices=2",
		fmt.Sprintf("video_nr=%d,%d", outNr, ingestNr),
		// A real integrated webcam label on the browser-facing device; an
		// internal label on the ingest device the page never sees.
		`card_label=Integrated Camera,kernel-ingest`,
		"exclusive_caps=1,1",
		fmt.Sprintf("max_width=%d", MaxWidth),
		fmt.Sprintf("max_height=%d", MaxHeight),
		"max_buffers=4",
	}
}

// loopbackModuleParams is the finit_module(2) parameter string equivalent of
// loopbackModuleArgs (insmod joins its argv the same way).
func loopbackModuleParams(outNr, ingestNr int) string {
	args := loopbackModuleArgs(outNr, ingestNr)
	return strings.Join(args[1:], " ")
}

// loadBundledModules loads the camera kernel modules from ModuleDir in
// load-order using finit_module(2), so the image needs no kmod tools. Modules
// already loaded are skipped. It is a no-op when ModuleDir has no manifest.
func loadBundledModules(dir string, outNr, ingestNr int) (bool, error) {
	order, err := os.ReadFile(filepath.Join(dir, "load-order"))
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return true, err
	}
	for _, name := range strings.Fields(string(order)) {
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			return true, err
		}
		params := ""
		if name == "v4l2loopback.ko" {
			params = loopbackModuleParams(outNr, ingestNr)
		}
		err = unix.FinitModule(int(f.Fd()), params, 0)
		_ = f.Close()
		if err != nil && err != unix.EEXIST {
			return true, fmt.Errorf("finit_module %s: %w", name, err)
		}
	}
	return true, nil
}

// --- V4L2 ioctls -----------------------------------------------------------
//
// The daemon reads the negotiated mode off the device (the capture client,
// not camerad, owns it) and pins the boot format at startup. These wrap just
// what camerad needs, without shelling out to v4l2-ctl.

const (
	v4l2BufTypeVideoCapture = 1
	v4l2BufTypeVideoOutput  = 2
	v4l2PixFmtYUYV          = 0x56595559 // 'YUYV' little-endian

	// struct v4l2_format is 208 bytes on 64-bit (the union's 8-byte member
	// alignment pads the pix section to offset 8); struct v4l2_streamparm is
	// 204 (its members are u32-only, pix at offset 4).
	vidiocGFmt  = 0xc0d05604 // _IOWR('V',  4, struct v4l2_format)
	vidiocSFmt  = 0xc0d05605 // _IOWR('V',  5, struct v4l2_format)
	vidiocGParm = 0xc0cc5615 // _IOWR('V', 21, struct v4l2_streamparm)
)

// ioctl issues request on fd with arg as the data buffer.
func ioctl(fd int, request uintptr, arg unsafe.Pointer) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), request, uintptr(arg))
	if errno != 0 {
		return errno
	}
	return nil
}

// v4l2Format mirrors the fields of struct v4l2_format (pix) camerad reads and
// sets; the buffer carries the raw layout.
type v4l2Format struct {
	Width, Height uint32
	PixFmt        uint32
}

// v4l2GetFormat reads the current format for bufType (capture or output).
// The pix section starts at offset 8 in the 208-byte v4l2_format.
func v4l2GetFormat(fd int, bufType uint32) (v4l2Format, error) {
	buf := make([]byte, 208)
	binary.LittleEndian.PutUint32(buf[0:4], bufType)
	if err := ioctl(fd, vidiocGFmt, unsafe.Pointer(&buf[0])); err != nil {
		return v4l2Format{}, err
	}
	return v4l2Format{
		Width:  binary.LittleEndian.Uint32(buf[8:12]),
		Height: binary.LittleEndian.Uint32(buf[12:16]),
		PixFmt: binary.LittleEndian.Uint32(buf[16:20]),
	}, nil
}

// v4l2SetFormat sets the device format; only what camerad uses (packed
// single-plane, default colorspace).
func v4l2SetFormat(fd int, bufType uint32, f v4l2Format) error {
	buf := make([]byte, 208)
	binary.LittleEndian.PutUint32(buf[0:4], bufType)
	binary.LittleEndian.PutUint32(buf[8:12], f.Width)
	binary.LittleEndian.PutUint32(buf[12:16], f.Height)
	binary.LittleEndian.PutUint32(buf[16:20], f.PixFmt)
	// field: leave ANY(1) so the driver picks; bytesperline/sizeimage are
	// computed by the driver.
	binary.LittleEndian.PutUint32(buf[20:24], 1)
	return ioctl(fd, vidiocSFmt, unsafe.Pointer(&buf[0]))
}

// v4l2GetFPS reads the frame interval, returning it as an fps value
// (denominator/numerator). v4l2loopback reports one shared capture_param for
// either buffer type.
func v4l2GetFPS(fd int) (num, den uint32, err error) {
	buf := make([]byte, 204)
	binary.LittleEndian.PutUint32(buf[0:4], v4l2BufTypeVideoCapture)
	if err := ioctl(fd, vidiocGParm, unsafe.Pointer(&buf[0])); err != nil {
		binary.LittleEndian.PutUint32(buf[0:4], v4l2BufTypeVideoOutput)
		if err2 := ioctl(fd, vidiocGParm, unsafe.Pointer(&buf[0])); err2 != nil {
			return 0, 0, err
		}
	}
	// v4l2_captureparm.timeperframe at offset 12 (capability u32, capturemode
	// u32, then the fract).
	num = binary.LittleEndian.Uint32(buf[12:16])
	den = binary.LittleEndian.Uint32(buf[16:20])
	if den == 0 {
		return 0, 0, fmt.Errorf("device reported zero fps denominator")
	}
	return num, den, nil
}

// negotiatedMode is what the browser-facing device currently presents: the
// frame size and interval the capture client (chromium) last negotiated.
// tpfNum/tpfDen is the frame interval as a fraction of a second.
type negotiatedMode struct {
	width, height  int
	tpfNum, tpfDen int
}

// defaultMode is used until the device's format can be read: the mixer opens
// the device at the maximum size at boot so later negotiation downwards fits
// the allocated buffers.
func defaultMode() negotiatedMode {
	return negotiatedMode{width: MaxWidth, height: MaxHeight, tpfNum: 1, tpfDen: DefaultFPS}
}

// interval is the time between frames the mixer should write at.
func (m negotiatedMode) interval() time.Duration {
	if m.tpfDen <= 0 {
		return time.Second / DefaultFPS
	}
	return time.Duration(m.tpfNum) * time.Second / time.Duration(m.tpfDen)
}
