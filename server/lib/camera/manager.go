package camera

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kernel/kernel-images/server/lib/statefile"
)

// Config configures a Manager. Zero fields fall back to the package defaults.
type Config struct {
	OutputDevice   string // browser-facing device, e.g. /dev/video0
	IngestDevice   string // neko writes the viewer camera here, e.g. /dev/video1
	Width, Height  int
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
	// LoadModule, when true, attempts to create the loopback devices via
	// modprobe at startup. When the guest kernel ships V4L2 and the devices
	// already exist this is a no-op.
	LoadModule bool
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
}

// Manager owns the browser-facing camera device and the viewer-camera mixer.
type Manager struct {
	cfg Config
	log *slog.Logger

	mu        sync.Mutex
	lastFrame []byte
	lastAt    time.Time
	wanted    bool
}

// New builds a Manager. It does not touch any devices until Run is called.
func New(cfg Config, log *slog.Logger) *Manager {
	cfg.withDefaults()
	if log == nil {
		log = slog.Default()
	}
	return &Manager{cfg: cfg, log: log}
}

func (m *Manager) frameSize() int { return m.cfg.Width * m.cfg.Height * 3 / 2 } // yuv420p

// Run sets up the devices and blocks, running the ingest reader, the mixer
// writer, the on-demand detector and the state endpoint until ctx is cancelled.
func (m *Manager) Run(ctx context.Context) error {
	if m.cfg.LoadModule {
		if err := m.ensureDevices(ctx); err != nil {
			return fmt.Errorf("ensure devices: %w", err)
		}
	}
	m.secureDevices()

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

	return m.runMixer(ctx, ph)
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

// placeholderFrame renders the placeholder PNG once into a single yuv420p frame
// the mixer repeats while the viewer is not sharing. A synthesized dark frame
// is used when no placeholder image is configured.
func (m *Manager) placeholderFrame(ctx context.Context) ([]byte, error) {
	size := fmt.Sprintf("scale=%d:%d,format=yuv420p", m.cfg.Width, m.cfg.Height)
	var in []string
	if m.cfg.PlaceholderPNG != "" {
		in = []string{"-i", m.cfg.PlaceholderPNG}
	} else {
		in = []string{"-f", "lavfi", "-i", fmt.Sprintf("color=c=0x212225:s=%dx%d", m.cfg.Width, m.cfg.Height)}
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
	vf := fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2,format=yuv420p",
		m.cfg.Width, m.cfg.Height, m.cfg.Width, m.cfg.Height)
	for ctx.Err() == nil {
		if _, err := os.Stat(m.cfg.IngestDevice); err != nil {
			sleep(ctx, 300*time.Millisecond)
			continue
		}
		m.readIngest(ctx, vf)
		sleep(ctx, 300*time.Millisecond)
	}
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
	cmd := m.ffmpeg(ctx, "-f", "v4l2", "-i", m.cfg.IngestDevice, "-vf", vf, "-f", "rawvideo", "pipe:1")
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

// runMixer is the single writer of the browser-facing device. It pushes the
// latest live frame when one is fresh, otherwise the placeholder, at a steady
// rate so chromium always sees a running camera.
func (m *Manager) runMixer(ctx context.Context, placeholder []byte) error {
	for ctx.Err() == nil {
		wr := m.ffmpeg(ctx, "-f", "rawvideo", "-pix_fmt", "yuv420p",
			"-s", fmt.Sprintf("%dx%d", m.cfg.Width, m.cfg.Height),
			"-r", strconv.Itoa(m.cfg.FPS),
			"-i", "pipe:0", "-f", "v4l2", m.cfg.OutputDevice)
		stdin, err := wr.StdinPipe()
		if err != nil {
			return err
		}
		if err := wr.Start(); err != nil {
			m.log.Error("mixer writer start", "err", err)
			sleep(ctx, time.Second)
			continue
		}
		ticker := time.NewTicker(time.Second / time.Duration(m.cfg.FPS))
		src := ""
		for range ticker.C {
			if ctx.Err() != nil {
				break
			}
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
			if _, err := stdin.Write(frame); err != nil {
				break
			}
		}
		ticker.Stop()
		_ = stdin.Close()
		_ = wr.Wait()
		sleep(ctx, 300*time.Millisecond)
	}
	return ctx.Err()
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

// secureDevices restricts the device modes so a page in the browser can only
// reach the mixer's output. The ingest device is root-only (0600) so chromium,
// which runs unprivileged, can neither open nor enumerate it — otherwise a page
// would see a second "kernel-ingest" camera and could select the raw,
// un-letterboxed feed. The output device is world-accessible (0666) as the
// camera the browser is meant to use.
func (m *Manager) secureDevices() {
	if err := os.Chmod(m.cfg.IngestDevice, 0o600); err != nil {
		m.log.Warn("secure ingest device", "device", m.cfg.IngestDevice, "err", err)
	}
	if err := os.Chmod(m.cfg.OutputDevice, 0o666); err != nil {
		m.log.Warn("secure output device", "device", m.cfg.OutputDevice, "err", err)
	}
}

// ensureDevices creates the loopback pair when it is absent, then confirms both
// nodes exist. Loading needs root; the daemon runs in the browser VM where it
// has it. When the guest kernel already ships the devices this is a no-op.
func (m *Manager) ensureDevices(ctx context.Context) error {
	_, outErr := os.Stat(m.cfg.OutputDevice)
	_, ingestErr := os.Stat(m.cfg.IngestDevice)
	if outErr == nil && ingestErr == nil {
		return nil
	}
	outNr := devNum(m.cfg.OutputDevice)
	ingestNr := devNum(m.cfg.IngestDevice)
	cmd := exec.CommandContext(ctx, "modprobe", loopbackModuleArgs(outNr, ingestNr)...)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return err
	}
	for i := 0; i < 50; i++ {
		_, outErr = os.Stat(m.cfg.OutputDevice)
		_, ingestErr = os.Stat(m.cfg.IngestDevice)
		if outErr == nil && ingestErr == nil {
			return nil
		}
		sleep(ctx, 100*time.Millisecond)
	}
	return fmt.Errorf("camera devices %s/%s did not appear after modprobe", m.cfg.OutputDevice, m.cfg.IngestDevice)
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
