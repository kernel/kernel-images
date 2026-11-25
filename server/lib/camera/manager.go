package camera

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/kernel/kernel-images/server/lib/statefile"
)

// Config configures a Manager. Zero fields fall back to the package defaults.
type Config struct {
	OutputDevice   string // browser-facing device, e.g. /dev/video0
	IngestDevice   string // neko writes the viewer camera here, e.g. /dev/video1
	Width, Height  int    // ingest frame size (neko's webcam pipeline is fixed)
	FPS            int
	PlaceholderPNG string // shown while the viewer is not sharing
	FFmpegPath     string
	StateAddr      string // host:port for the on-demand state endpoint (in-VM debugging)
	// StateFile, when set, is a path the manager rewrites with the current
	// State JSON whenever it changes. It is served to the live-view client as a
	// static file under the authenticated live-view origin (neko's static dir),
	// which the viewer's browser can reach; the HTTP endpoint above cannot be
	// reached cross-origin from the viewer.
	StateFile string
	// LoadModule, when true, attempts to load the camera kernel modules at
	// startup: from the modules shipped in the image (ModuleDir), falling back
	// to modprobe. Device setup (waiting for the nodes, moving the ingest
	// node out of /dev/video*) happens regardless.
	LoadModule bool
	// DisableNoise turns off the sensor-noise pass (see noise.go).
	DisableNoise bool
	// IngestVideoNr is the ingest device's kernel video number (its sysfs
	// name under /sys/class/video4linux). The node itself is at IngestDevice.
	IngestVideoNr int
}

func (c *Config) withDefaults() {
	if c.OutputDevice == "" {
		c.OutputDevice = DefaultOutputDevice
	}
	if c.IngestDevice == "" {
		c.IngestDevice = DefaultIngestDevice
	}
	// Clamp the mode to sane bounds: a non-positive or absurd value would panic
	// make([]byte, …) for the frame buffer or time.NewTicker (non-positive
	// duration) downstream. These come from env (CAMERA_WIDTH etc.), so don't
	// trust them.
	if c.Width <= 0 || c.Width > 7680 {
		c.Width = DefaultWidth
	}
	if c.Height <= 0 || c.Height > 4320 {
		c.Height = DefaultHeight
	}
	if c.FPS <= 0 || c.FPS > 120 {
		c.FPS = DefaultFPS
	}
	if c.FFmpegPath == "" {
		c.FFmpegPath = "ffmpeg"
	}
	if c.StateAddr == "" {
		c.StateAddr = "127.0.0.1:10004"
	}
	if c.IngestVideoNr <= 0 {
		c.IngestVideoNr = DefaultIngestVideoNr
	}
}

// Manager owns the browser-facing camera device and the viewer-camera mixer.
type Manager struct {
	cfg Config
	log *slog.Logger

	mu         sync.Mutex
	lastFrame  []byte
	lastAt     time.Time
	wanted     bool
	negotiated negotiatedMode
}

// withOutputDevice runs fn with a short-lived handle on the output device.
// The handle is never kept: an S_FMT makes the opener hold the loopback's
// OUTPUT format token until close, and the mixer's fd is the one that must
// hold it long-term.
func (m *Manager) withOutputDevice(fn func(fd int) error) error {
	fd, err := unix.Open(m.cfg.OutputDevice, unix.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	return fn(fd)
}

// New builds a Manager. It does not touch any devices until Run is called.
func New(cfg Config, log *slog.Logger) *Manager {
	cfg.withDefaults()
	if log == nil {
		log = slog.Default()
	}
	return &Manager{cfg: cfg, log: log, negotiated: defaultMode()}
}

// noiseFor returns a noise filter for frames of w×h, or nil when the sensor
// noise pass is disabled.
func (m *Manager) noiseFor(w, h int) *noiseFilter {
	if m.cfg.DisableNoise {
		return nil
	}
	return newNoiseFilter(w, h)
}

func (m *Manager) frameSize() int { return m.cfg.Width * m.cfg.Height * 2 } // yuyv422

// Run sets up the devices and blocks, running the ingest reader, the mixer
// writer, the on-demand detector and the state endpoint until ctx is cancelled.
func (m *Manager) Run(ctx context.Context) error {
	if err := m.ensureDevices(ctx); err != nil {
		return fmt.Errorf("ensure devices: %w", err)
	}
	m.secureDevices()

	// The mixer holds the device open for the daemon's lifetime: the
	// producer never detaches, so the camera never disappears from a page's
	// point of view, even while a page renegotiates the capture mode.
	dev, err := unix.Open(m.cfg.OutputDevice, unix.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", m.cfg.OutputDevice, err)
	}
	defer func() { _ = unix.Close(dev) }()

	// Pin the boot mode before anything reads it: YUYV (the packed format
	// UVC cameras deliver) at the maximum size, so the allocated buffers
	// fit any mode a page may later negotiate. Without this, the watcher's
	// first read could publish the module's compile-time default.
	boot := defaultMode()
	if err := v4l2SetFormat(dev, v4l2BufTypeVideoOutput, v4l2Format{
		Width:  uint32(boot.width),
		Height: uint32(boot.height),
		PixFmt: v4l2PixFmtYUYV,
	}); err != nil {
		return fmt.Errorf("set boot format: %w", err)
	}

	ph, err := m.placeholderFrame(ctx)
	if err != nil {
		return fmt.Errorf("placeholder: %w", err)
	}

	srv := &http.Server{Addr: m.cfg.StateAddr, Handler: m.mux()}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			m.log.Error("state endpoint", "err", err)
		}
	}()

	go m.runIngest(ctx)
	go m.runDetector(ctx)
	go m.watchMode(ctx)

	return m.runMixer(ctx, ph, &fdWriter{fd: dev})
}

func (m *Manager) mux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/state", stateHandler(m.snapshot))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })
	return mux
}

// frameFreshFor bounds how old the latest ingest frame may be and still be fed
// to the browser instead of the placeholder. The mixer and snapshot share it so
// a page is never told streaming=true while it is seeing the placeholder.
const frameFreshFor = time.Second

// live returns the latest ingest frame and whether it is still fresh.
func (m *Manager) live() (frame []byte, fresh bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastFrame, m.lastFrame != nil && time.Since(m.lastAt) < frameFreshFor
}

func (m *Manager) snapshot() State {
	_, fresh := m.live()
	m.mu.Lock()
	defer m.mu.Unlock()
	return State{
		Wanted:    m.wanted,
		Streaming: fresh,
		Width:     m.cfg.Width,
		Height:    m.cfg.Height,
	}
}

func (m *Manager) ffmpeg(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, m.cfg.FFmpegPath, append([]string{"-hide_banner", "-loglevel", "error"}, args...)...)
	cmd.Stderr = os.Stderr
	return cmd
}

// placeholderFrame renders the placeholder PNG once into a single yuyv422 frame
// the mixer repeats while the viewer is not sharing. A synthesized dark frame
// is used when no placeholder image is configured (or it cannot be read: the
// camera must still come up).
func (m *Manager) placeholderFrame(ctx context.Context) ([]byte, error) {
	size := fmt.Sprintf("scale=%d:%d,format=yuyv422", m.cfg.Width, m.cfg.Height)
	in := []string{"-f", "lavfi", "-i", fmt.Sprintf("color=c=0x212225:s=%dx%d", m.cfg.Width, m.cfg.Height)}
	if m.cfg.PlaceholderPNG != "" {
		if _, err := os.Stat(m.cfg.PlaceholderPNG); err == nil {
			in = []string{"-i", m.cfg.PlaceholderPNG}
		} else {
			m.log.Warn("placeholder image unreadable, using synthesized frame", "path", m.cfg.PlaceholderPNG, "err", err)
		}
	}
	args := append(in, "-frames:v", "1", "-vf", size, "-f", "rawvideo", "pipe:1")
	out, err := m.ffmpeg(ctx, args...).Output()
	if err != nil {
		return nil, err
	}
	if len(out) < m.frameSize() {
		return nil, fmt.Errorf("short placeholder frame: %d < %d", len(out), m.frameSize())
	}
	return out[:m.frameSize()], nil
}

// ingestStallTimeout bounds how long the ingest reader may hold the device
// without receiving a frame. It covers neko's first keyframe after a share
// starts, and releases the device soon after a share stops.
const ingestStallTimeout = 3 * time.Second

// runIngest continuously decodes the ingest device into raw frames, keeping the
// latest frame for the mixer. It waits for the device to exist and reconnects
// whenever neko stops and restarts its webcam capture.
func (m *Manager) runIngest(ctx context.Context) {
	vf := fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2,format=yuyv422",
		m.cfg.Width, m.cfg.Height, m.cfg.Width, m.cfg.Height)
	for ctx.Err() == nil {
		if !m.ingestProducing() {
			sleep(ctx, 300*time.Millisecond)
			continue
		}
		m.readIngest(ctx, vf)
		sleep(ctx, 300*time.Millisecond)
	}
}

// sysfsVideo is where v4l2loopback publishes per-device state.
var sysfsVideo = "/sys/class/video4linux"

// ingestProducing reports whether neko is currently writing the ingest
// device (kernel name video<nr>). v4l2loopback reports "capture" only while a
// producer is streaming. Waiting for it, rather than retrying open, keeps the
// reader from opening the device while neko is still negotiating its format,
// which can race neko's pipeline start. Without the sysfs attribute it falls
// back to the device node existing.
func (m *Manager) ingestProducing() bool {
	b, err := os.ReadFile(filepath.Join(sysfsVideo, "video"+strconv.Itoa(m.ingestNr()), "state"))
	if err != nil {
		_, statErr := os.Stat(m.cfg.IngestDevice)
		return statErr == nil
	}
	return strings.TrimSpace(string(b)) == "capture"
}

// ingestNr is the ingest device's kernel video number.
func (m *Manager) ingestNr() int {
	return m.cfg.IngestVideoNr
}

// readIngest runs one ffmpeg reader until it stalls, fails, or ctx ends.
//
// A v4l2loopback read blocks forever once its producer goes away, and the
// blocked reader keeps the device open with the previous session's format,
// which stops the next neko session from streaming. A stall timer cancels this
// attempt's context if no frame arrives within ingestStallTimeout, so the same
// cancellation that kills ffmpeg is what lets Wait return — there is no
// separate watchdog that could retire before Wait and leave ffmpeg wedged.
func (m *Manager) readIngest(parent context.Context, vf string) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	// Low-latency input, and flush each frame to the pipe as soon as it is
	// muxed: otherwise the tail of every frame waits in ffmpeg's output buffer
	// until the next one arrives, adding a frame of delay.
	cmd := m.ffmpeg(ctx, "-fflags", "nobuffer", "-flags", "low_delay", "-probesize", "32", "-analyzeduration", "0",
		"-f", "v4l2", "-i", m.cfg.IngestDevice, "-vf", vf, "-flush_packets", "1", "-f", "rawvideo", "pipe:1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		m.log.Error("ingest pipe", "err", err)
		return
	}
	if err := cmd.Start(); err != nil {
		m.log.Error("ingest start", "err", err)
		return
	}
	defer func() { _ = cmd.Wait() }() // runs after cancel below (LIFO)
	stall := time.AfterFunc(ingestStallTimeout, cancel)
	defer stall.Stop()
	r := bufio.NewReaderSize(stdout, m.frameSize()*2)
	for {
		buf := make([]byte, m.frameSize())
		if _, err := io.ReadFull(r, buf); err != nil {
			return
		}
		stall.Reset(ingestStallTimeout)
		m.mu.Lock()
		m.lastFrame, m.lastAt = buf, time.Now()
		m.mu.Unlock()
	}
}

// watchMode tracks the mode the capture client (chromium) negotiates on the
// browser-facing device and publishes it for the mixer. The device, not this
// daemon, owns the negotiation: a page asking for 640x480 or 1920x1080 gets
// exactly that, like a camera with several sensor modes.
//
// The loopback answers CAPTURE-typed ioctls while a producer streams and
// OUTPUT-typed ones while idle (its exclusive_caps token rules), so both
// types are tried.
func (m *Manager) watchMode(ctx context.Context) {
	for ctx.Err() == nil {
		if mode, err := m.readMode(); err == nil {
			m.mu.Lock()
			if mode != m.negotiated {
				m.log.Info("camera mode", "size", fmt.Sprintf("%dx%d", mode.width, mode.height),
					"interval", fmt.Sprintf("%d/%ds", mode.tpfNum, mode.tpfDen))
				m.negotiated = mode
			}
			m.mu.Unlock()
		}
		sleep(ctx, 200*time.Millisecond)
	}
}

func (m *Manager) readMode() (mode negotiatedMode, err error) {
	err = m.withOutputDevice(func(fd int) error {
		f, err := v4l2GetFormat(fd, v4l2BufTypeVideoCapture)
		if err != nil {
			f, err = v4l2GetFormat(fd, v4l2BufTypeVideoOutput)
		}
		if err != nil {
			return err
		}
		mode = negotiatedMode{width: int(f.Width), height: int(f.Height), tpfNum: 1, tpfDen: DefaultFPS}
		if num, den, err := v4l2GetFPS(fd); err == nil {
			mode.tpfNum, mode.tpfDen = int(num), int(den)
		}
		if mode.width <= 0 || mode.height <= 0 {
			return fmt.Errorf("bad mode %+v", mode)
		}
		return nil
	})
	return mode, err
}

func (m *Manager) currentMode() negotiatedMode {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.negotiated
}

// fdWriter writes frames to a v4l2 device node, looping short writes.
type fdWriter struct{ fd int }

func (w *fdWriter) Write(p []byte) (int, error) {
	var n int
	for n < len(p) {
		m, err := unix.Write(w.fd, p[n:])
		n += m
		if err != nil {
			return n, err
		}
		if m == 0 {
			return n, io.ErrShortWrite
		}
	}
	return n, nil
}

// runMixer is the single writer of the browser-facing device. Frames are
// written on a steady paced cadence (the negotiated frame interval, with
// sub-millisecond jitter) rather than as the network delivers them, so the
// timestamps chromium and the page observe look like a local sensor's clock
// and not like a network stream. Each tick writes the newest live frame when
// one is fresh — repeating the previous frame when the next is merely late —
// and the placeholder otherwise, so chromium always sees a running camera.
//
// The negotiated mode is re-read every tick, so a page renegotiating the
// capture size or rate takes effect on the next frame without the producer
// ever detaching — a real camera switches sensor modes without unplugging.
func (m *Manager) runMixer(ctx context.Context, placeholder []byte, w io.Writer) error {
	rng := rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 0x9e3779b97f4a7c15))
	var sc *scaler
	var noise *noiseFilter
	var mode negotiatedMode
	var out []byte
	src := ""

	// Anchor ticks to an absolute schedule so jitter does not random-walk.
	start := time.Now()
	n := int64(0)
	for ctx.Err() == nil {
		n++
		if cur := m.currentMode(); cur != mode {
			mode = cur
			sc = newScaler(m.cfg.Width, m.cfg.Height, mode.width, mode.height)
			out = make([]byte, sc.frameSize())
			noise = m.noiseFor(mode.width, mode.height)
			start, n = time.Now(), 0 // re-anchor the schedule on a mode change
		}
		interval := mode.interval()
		if interval <= 0 {
			interval = time.Second / DefaultFPS
		}
		jitter := minDuration(paceJitter, interval/10)

		frame, fresh := m.live()
		next := "placeholder"
		if fresh {
			next = "live"
		} else {
			frame = placeholder
		}
		if next != src {
			m.log.Info("camera source", "source", next)
			src = next
		}
		sc.scale(out, frame)
		if noise != nil {
			noise.apply(out)
		}
		if _, err := w.Write(out); err != nil {
			m.log.Error("mixer write", "err", err)
			if !sleepC(ctx, time.Second) {
				break
			}
		}
		target := start.Add(time.Duration(n)*interval + jitterOf(rng, jitter))
		if delay := time.Until(target); delay > 0 {
			if !sleepC(ctx, delay) {
				break
			}
		} else if delay < -interval {
			start = time.Now() // fell badly behind; re-anchor
			n = 0
		}
	}
	return ctx.Err()
}

// paceJitter bounds the per-tick timing jitter. A real USB camera's frame
// interval wobbles by well under a millisecond around its nominal cadence.
const paceJitter = time.Millisecond

func jitterOf(rng *rand.Rand, bound time.Duration) time.Duration {
	if bound <= 0 {
		return 0
	}
	return time.Duration(rng.Int64N(int64(2*bound))) - bound
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// sleepC is sleep that reports whether it completed (false = ctx done).
func sleepC(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

const (
	detectInterval = 300 * time.Millisecond
	// wantedHolds is how many consecutive polls must see a chromium process
	// holding the device before `wanted` goes true. chromium opens the device
	// briefly to read capabilities during enumerateDevices(), so a single
	// latched sample is not consent to turn on the viewer's camera; requiring a
	// sustained hold filters those blips. The client applies the real
	// consent gate (auto-follow only after one manual share). A page-driven CDP
	// signal would be cleaner than scanning /proc; see the PR notes.
	wantedHolds = 3
)

// runDetector polls /proc for a chromium process holding the browser-facing
// device open — the on-demand "a page wants the camera" signal — debounces it,
// and republishes the state file whenever the snapshot changes.
func (m *Manager) runDetector(ctx context.Context) {
	var last State
	holds := 0
	first := true
	for ctx.Err() == nil {
		if chromiumHoldsDevice(m.cfg.OutputDevice) {
			if holds < wantedHolds {
				holds++
			}
		} else {
			holds = 0
		}
		m.mu.Lock()
		m.wanted = holds >= wantedHolds
		m.mu.Unlock()

		if s := m.snapshot(); m.cfg.StateFile != "" && (first || s != last) {
			if err := statefile.WriteJSON(m.cfg.StateFile, s); err != nil {
				m.log.Error("camera state file", "path", m.cfg.StateFile, "err", err)
			}
			last, first = s, false
		}
		sleep(ctx, detectInterval)
	}
}

// secureDevices restricts the device modes: the ingest node is root-only
// (0600), and it lives outside /dev/video* (see ensureDevices) so chromium
// never enumerates it — otherwise a page would see a second "kernel-ingest"
// camera and could select the raw, un-managed feed. The output device is
// world-accessible (0666) as the camera the browser is meant to use.
func (m *Manager) secureDevices() {
	if err := os.Chmod(m.cfg.IngestDevice, 0o600); err != nil {
		m.log.Warn("secure ingest device", "device", m.cfg.IngestDevice, "err", err)
	}
	if err := os.Chmod(m.cfg.OutputDevice, 0o666); err != nil {
		m.log.Warn("secure output device", "device", m.cfg.OutputDevice, "err", err)
	}
}

// ensureDevices creates the loopback pair when it is absent, then confirms both
// nodes exist. The modules built for the guest kernel and shipped in the image
// (ModuleDir) are preferred; modprobe is the fallback for a kernel that ships
// v4l2loopback itself. Loading needs root; the daemon runs in the browser VM
// where it has it. When the guest kernel already ships the devices this is a
// no-op.
func (m *Manager) ensureDevices(ctx context.Context) error {
	_, outErr := os.Stat(m.cfg.OutputDevice)
	_, ingestErr := os.Stat(m.cfg.IngestDevice)
	if outErr == nil && ingestErr == nil {
		return nil
	}
	outNr := devNum(m.cfg.OutputDevice)
	ingestNr := m.cfg.IngestVideoNr
	// The kernel registers the ingest node at /dev/videoN; it must be moved
	// out of the pattern chromium enumerates.
	kernelNode := fmt.Sprintf("/dev/video%d", ingestNr)

	if (outErr != nil || (ingestErr != nil && !fileExists(kernelNode))) && m.cfg.LoadModule {
		loaded, err := loadBundledModules(ModuleDir, outNr, ingestNr)
		if err != nil {
			return err
		}
		if !loaded {
			cmd := exec.CommandContext(ctx, "modprobe", loopbackModuleArgs(outNr, ingestNr)...)
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				return err
			}
		}
	}
	for i := 0; i < 50; i++ {
		_, outErr = os.Stat(m.cfg.OutputDevice)
		_, ingestErr = os.Stat(m.cfg.IngestDevice)
		if outErr == nil && ingestErr == nil {
			return nil
		}
		if outErr == nil && ingestErr != nil && fileExists(kernelNode) {
			if err := os.MkdirAll(filepath.Dir(m.cfg.IngestDevice), 0o755); err == nil {
				if err := os.Rename(kernelNode, m.cfg.IngestDevice); err != nil {
					m.log.Warn("move ingest node", "from", kernelNode, "to", m.cfg.IngestDevice, "err", err)
				}
			}
		}
		sleep(ctx, 100*time.Millisecond)
	}
	return fmt.Errorf("camera devices %s/%s did not appear after module load", m.cfg.OutputDevice, m.cfg.IngestDevice)
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// chromiumHoldsDevice reports whether any chromium process has dev open.
func chromiumHoldsDevice(dev string) bool {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		pid := e.Name()
		if _, err := strconv.Atoi(pid); err != nil {
			continue
		}
		comm, _ := os.ReadFile(filepath.Join("/proc", pid, "comm"))
		if !strings.Contains(string(comm), "chrom") {
			continue
		}
		fdDir := filepath.Join("/proc", pid, "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			if target, err := os.Readlink(filepath.Join(fdDir, fd.Name())); err == nil && target == dev {
				return true
			}
		}
	}
	return false
}

// devNum parses the trailing number of a /dev/videoN path, or -1.
func devNum(dev string) int {
	n, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(dev), "video"))
	if err != nil {
		return -1
	}
	return n
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
