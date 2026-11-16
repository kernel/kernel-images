package camera

import (
	"bufio"
	"context"
	"encoding/json"
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
)

// Config configures a Manager. Zero fields fall back to the package defaults.
type Config struct {
	OutputDevice  string // browser-facing device, e.g. /dev/video0
	IngestDevice  string // neko writes the viewer camera here, e.g. /dev/video1
	Width, Height int
	FPS           int
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
	if c.Width == 0 {
		c.Width = DefaultWidth
	}
	if c.Height == 0 {
		c.Height = DefaultHeight
	}
	if c.FPS == 0 {
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
	if m.cfg.StateFile != "" {
		go m.runStateFile(ctx)
	}

	return m.runMixer(ctx, ph)
}

func (m *Manager) mux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/state", stateHandler(m.snapshot))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })
	return mux
}

func (m *Manager) snapshot() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return State{
		Wanted:    m.wanted,
		Streaming: time.Since(m.lastAt) < time.Second && m.lastFrame != nil,
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
		cmd := m.ffmpeg(ctx, "-f", "v4l2", "-i", m.cfg.IngestDevice, "-vf", vf, "-f", "rawvideo", "pipe:1")
		stdout, err := cmd.StdoutPipe()
		if err == nil && cmd.Start() == nil {
			r := bufio.NewReaderSize(stdout, m.frameSize()*2)
			for ctx.Err() == nil {
				buf := make([]byte, m.frameSize())
				if _, err := io.ReadFull(r, buf); err != nil {
					break
				}
				m.mu.Lock()
				m.lastFrame, m.lastAt = buf, time.Now()
				m.mu.Unlock()
			}
			_ = cmd.Wait()
		}
		sleep(ctx, 300*time.Millisecond)
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
			m.mu.Lock()
			frame, fresh := m.lastFrame, time.Since(m.lastAt) < time.Second
			m.mu.Unlock()
			next := "placeholder"
			if fresh && frame != nil {
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

// runDetector polls /proc for a chromium process holding the browser-facing
// device open, which is the on-demand "a page wants the camera" signal.
//
// A page-driven CDP signal would be cleaner than scanning /proc; see the PR
// notes. This is deliberately simple for the first cut.
func (m *Manager) runDetector(ctx context.Context) {
	for ctx.Err() == nil {
		want := chromiumHoldsDevice(m.cfg.OutputDevice)
		m.mu.Lock()
		m.wanted = want
		m.mu.Unlock()
		sleep(ctx, 300*time.Millisecond)
	}
}

// runStateFile rewrites the state file whenever the snapshot changes so the
// live-view client can poll it as a static asset under the live-view origin.
func (m *Manager) runStateFile(ctx context.Context) {
	last := ""
	tmp := m.cfg.StateFile + ".tmp"
	for ctx.Err() == nil {
		b, _ := json.Marshal(m.snapshot())
		if s := string(b); s != last {
			if err := os.WriteFile(tmp, b, 0o644); err == nil {
				if err := os.Rename(tmp, m.cfg.StateFile); err == nil {
					last = s
				}
			}
		}
		sleep(ctx, 300*time.Millisecond)
	}
}

// ensureDevices creates the loopback pair when the browser-facing device is
// absent. Loading needs root; the daemon runs in the browser VM where it has it.
func (m *Manager) ensureDevices(ctx context.Context) error {
	if _, err := os.Stat(m.cfg.OutputDevice); err == nil {
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
		if _, err := os.Stat(m.cfg.OutputDevice); err == nil {
			return nil
		}
		sleep(ctx, 100*time.Millisecond)
	}
	return fmt.Errorf("%s did not appear after modprobe", m.cfg.OutputDevice)
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
