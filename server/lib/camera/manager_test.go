package camera

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestLoopbackModuleArgs(t *testing.T) {
	got := loopbackModuleArgs(0, 1)
	want := []string{
		"v4l2loopback", "devices=2", "video_nr=0,1",
		"card_label=Integrated Camera,kernel-ingest", "exclusive_caps=1,1",
		"max_width=1920", "max_height=1080", "max_buffers=4",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
	if got, want := loopbackModuleParams(0, 1), "devices=2 video_nr=0,1 card_label=Integrated Camera,kernel-ingest exclusive_caps=1,1 max_width=1920 max_height=1080 max_buffers=4"; got != want {
		t.Fatalf("params = %q, want %q", got, want)
	}
}

func TestDevNum(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
	}{
		{"/dev/video0", 0},
		{"/dev/video12", 12},
		{"/dev/null", -1},
	} {
		if got := devNum(tc.in); got != tc.want {
			t.Errorf("devNum(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestConfigDefaults(t *testing.T) {
	var c Config
	c.withDefaults()
	if c.OutputDevice != DefaultOutputDevice || c.IngestDevice != DefaultIngestDevice {
		t.Errorf("device defaults not applied: %+v", c)
	}
	if c.Width != DefaultWidth || c.Height != DefaultHeight || c.FPS != DefaultFPS {
		t.Errorf("mode defaults not applied: %+v", c)
	}

	bad := Config{Width: -1, Height: 1 << 20, FPS: -5}
	bad.withDefaults()
	if bad.Width != DefaultWidth || bad.Height != DefaultHeight || bad.FPS != DefaultFPS {
		t.Errorf("out-of-range mode not clamped: %+v", bad)
	}
}

func TestStateHandler(t *testing.T) {
	want := State{Wanted: true, Streaming: false, Width: 1280, Height: 720}
	rr := httptest.NewRecorder()
	stateHandler(func() State { return want }).ServeHTTP(rr, httptest.NewRequest("GET", "/state", nil))
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q", ct)
	}
	var got State
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("state = %+v, want %+v", got, want)
	}
}

func TestFrameSize(t *testing.T) {
	m := New(Config{Width: 1280, Height: 720}, nil)
	if got := m.frameSize(); got != 1280*720*2 {
		t.Errorf("frameSize = %d", got)
	}
}

func TestNegotiatedModeTiming(t *testing.T) {
	m := negotiatedMode{640, 480, 1, 30}
	if got := m.interval(); got != time.Second/30 {
		t.Errorf("interval = %v", got)
	}
	m = negotiatedMode{1920, 1080, 1001, 30000}
	if got := m.interval(); got != time.Duration(1001)*time.Second/30000 {
		t.Errorf("interval = %v", got)
	}
	var zero negotiatedMode
	if got := zero.interval(); got != time.Second/DefaultFPS {
		t.Errorf("zero interval = %v", got)
	}
}

func TestReadIngestStallReleasesDevice(t *testing.T) {
	// A reader whose producer sends nothing must release the device (ffmpeg
	// exits and readIngest returns) within roughly the stall timeout, rather
	// than blocking forever. Point it at /dev/null, which ffmpeg can open as a
	// v4l2 input but never yields a frame from.
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg unavailable:", err)
	}
	m := New(Config{IngestDevice: "/dev/null", Width: 64, Height: 48}, nil)
	// Shrink the stall timeout for the test via a short parent context deadline
	// as an upper bound, and rely on the real timeout being short enough.
	ctx, cancel := context.WithTimeout(context.Background(), ingestStallTimeout+5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { m.readIngest(ctx, "format=yuyv422"); close(done) }()
	select {
	case <-done:
	case <-time.After(ingestStallTimeout + 4*time.Second):
		t.Fatal("readIngest did not return after a stall")
	}
}

// frameSink records each frame the mixer writes.
type frameSink struct {
	mu     sync.Mutex
	frames [][]byte
}

func (s *frameSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frames = append(s.frames, append([]byte(nil), p...))
	return len(p), nil
}

func (s *frameSink) snapshot() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]byte(nil), s.frames...)
}

func TestMixerForwardsLiveFramePromptly(t *testing.T) {
	sink := &frameSink{}
	m := New(Config{Width: 4, Height: 2, FPS: 100, DisableNoise: true}, nil)
	m.negotiated = negotiatedMode{4, 2, 1, 100} // 10 ms ticks
	placeholder := bytes.Repeat([]byte{'P'}, m.frameSize())
	live := bytes.Repeat([]byte{'L'}, m.frameSize())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = m.runMixer(ctx, placeholder, sink) }()

	m.mu.Lock()
	m.lastFrame, m.lastAt = live, time.Now()
	m.mu.Unlock()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, f := range sink.snapshot() {
			if bytes.Equal(f, live) {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("live frame was not written promptly")
}

func TestMixerWritesPlaceholderWithoutLiveFrames(t *testing.T) {
	sink := &frameSink{}
	m := New(Config{Width: 4, Height: 2, FPS: 100, DisableNoise: true}, nil)
	m.negotiated = negotiatedMode{4, 2, 1, 100}
	placeholder := bytes.Repeat([]byte{'P'}, m.frameSize())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = m.runMixer(ctx, placeholder, sink) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(sink.snapshot()) > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("placeholder was not written")
}

func TestMixerFollowsModeChangeWithoutStopping(t *testing.T) {
	sink := &frameSink{}
	m := New(Config{Width: 4, Height: 2, FPS: 100, DisableNoise: true}, nil)
	m.negotiated = negotiatedMode{4, 2, 1, 100}
	placeholder := bytes.Repeat([]byte{'P'}, m.frameSize())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = m.runMixer(ctx, placeholder, sink) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(sink.snapshot()) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// A page negotiating a different size changes the frames the mixer
	// writes on the next tick — the producer never stops.
	m.mu.Lock()
	m.negotiated = negotiatedMode{2, 1, 1, 100}
	m.mu.Unlock()

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, f := range sink.snapshot() {
			if len(f) == 2*1*2 {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("mixer did not follow the negotiated mode")
}

func TestIngestProducing(t *testing.T) {
	old := sysfsVideo
	t.Cleanup(func() { sysfsVideo = old })
	sysfsVideo = t.TempDir()
	m := New(Config{}, nil)
	if err := os.MkdirAll(filepath.Join(sysfsVideo, "video1"), 0o755); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(sysfsVideo, "video1", "state")
	for _, tc := range []struct {
		state string
		want  bool
	}{{"capture\n", true}, {"output\n", false}} {
		if err := os.WriteFile(state, []byte(tc.state), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := m.ingestProducing(); got != tc.want {
			t.Errorf("state %q: got %v, want %v", tc.state, got, tc.want)
		}
	}
	// No sysfs attribute (a device number with no state file): fall back to
	// whether the device node exists.
	m.cfg.IngestVideoNr = 7
	m.cfg.IngestDevice = "/dev/null"
	if !m.ingestProducing() {
		t.Error("existing device without sysfs state should count as producing")
	}
	m.cfg.IngestDevice = "/dev/does-not-exist"
	if m.ingestProducing() {
		t.Error("missing device should not count as producing")
	}
}

func TestScalerSameSize(t *testing.T) {
	s := newScaler(4, 2, 4, 2)
	if !s.sameSize() {
		t.Fatal("4x2->4x2 should be a passthrough")
	}
	src := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	dst := make([]byte, s.frameSize())
	s.scale(dst, src)
	if !bytes.Equal(dst, src) {
		t.Errorf("passthrough changed the frame: %v", dst)
	}
}

func TestScalerDownscaleCentered(t *testing.T) {
	// 4x2 -> 2x1 (same 2:1 aspect, no crop): every output sample is the
	// bilinear midpoint of the source window.
	s := newScaler(4, 2, 2, 1)
	if s.cw != 4 || s.ch != 2 || s.ox != 0 || s.oy != 0 {
		t.Fatalf("crop = %dx%d+%d+%d, want full frame", s.cw, s.ch, s.ox, s.oy)
	}
	src := make([]byte, 4*2*2)
	for y := 0; y < 2; y++ {
		for x := 0; x < 4; x++ {
			src[(y*4+x)*2] = uint8(40 * x) // luma gradient 0,40,80,120
		}
	}
	dst := make([]byte, s.frameSize())
	s.scale(dst, src)
	if got := dst[0]; got < 15 || got > 25 {
		t.Errorf("dst luma[0] = %d, want ~20", got)
	}
	if got := dst[2]; got < 95 || got > 105 {
		t.Errorf("dst luma[1] = %d, want ~100", got)
	}
}

func TestScalerCropsToAspect(t *testing.T) {
	// 16:9 source into a 4:3 target: sides are cropped, not squashed.
	s := newScaler(1280, 720, 640, 480)
	wantCW := 720 * 4 / 3 // 960
	if s.cw != wantCW || s.ch != 720 {
		t.Errorf("crop window = %dx%d, want %dx720", s.cw, s.ch, wantCW)
	}
	if s.ox != (1280-wantCW)/2 || s.oy != 0 {
		t.Errorf("crop origin = %d,%d, want %d,0", s.ox, s.oy, (1280-wantCW)/2)
	}
}

func TestScalerChromaAlignment(t *testing.T) {
	// A solid-color frame must come through unchanged at any size: U and V
	// stay aligned to their pixel pairs.
	for _, size := range [][2]int{{640, 480}, {1280, 720}, {1920, 1080}} {
		s := newScaler(1280, 720, size[0], size[1])
		src := bytes.Repeat([]byte{0x22, 0xF0}, 1280*720) // dark + blue chroma
		dst := make([]byte, s.frameSize())
		s.scale(dst, src)
		for i := 0; i < len(dst); i += 4 {
			if dst[i+1] != 0xF0 || dst[i+3] != 0xF0 {
				t.Fatalf("%v: chroma misaligned at byte %d: U=%02x V=%02x", size, i, dst[i+1], dst[i+3])
			}
			if dst[i] < 0x1F || dst[i] > 0x25 {
				t.Fatalf("%v: luma drifted at byte %d: %02x", size, i, dst[i])
			}
		}
	}
}

func BenchmarkScaler1080(b *testing.B) {
	s := newScaler(1280, 720, 1920, 1080)
	src := make([]byte, 1280*720*2)
	dst := make([]byte, s.frameSize())
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.scale(dst, src)
	}
}

func TestLoadBundledModulesNoManifest(t *testing.T) {
	loaded, err := loadBundledModules(t.TempDir(), 0, 1)
	if err != nil || loaded {
		t.Errorf("loadBundledModules without manifest = %v, %v; want false, nil", loaded, err)
	}
}

func TestNoiseFilter(t *testing.T) {
	const w, h = 16, 8
	n := newNoiseFilter(w, h)
	base := make([]byte, w*h*2)
	for i := range base {
		base[i] = 128
	}
	a := append([]byte(nil), base...)
	b := append([]byte(nil), base...)
	n.apply(a)
	n.apply(b)

	if bytes.Equal(a, base) {
		t.Error("one pass changed nothing")
	}
	if bytes.Equal(a, b) {
		t.Error("two passes produced identical frames: frames must never repeat")
	}
	var sum, maxDev int
	for i := range a {
		d := int(a[i]) - 128
		if d < 0 {
			d = -d
		}
		sum += d
		if d > maxDev {
			maxDev = d
		}
	}
	mean := float64(sum) / float64(len(a))
	if mean > 2.5 || maxDev > 8 {
		t.Errorf("noise too strong: mean |dev| = %.2f, max %d", mean, maxDev)
	}
	if mean == 0 {
		t.Error("noise had no effect")
	}
}

func TestNoiseFilterFlatPlaceholderDiffers(t *testing.T) {
	const w, h = 1280, 720
	n := newNoiseFilter(w, h)
	ph := bytes.Repeat([]byte{0x22, 0x80}, w*h) // dark gray, neutral chroma
	f1 := append([]byte(nil), ph...)
	f2 := append([]byte(nil), ph...)
	n.apply(f1)
	n.apply(f2)
	if bytes.Equal(f1, f2) {
		t.Error("placeholder frames identical across ticks: looks frozen")
	}
}

func TestJitterOfBounds(t *testing.T) {
	rng := newNoiseFilter(1, 1).rng
	for i := 0; i < 1000; i++ {
		j := jitterOf(rng, time.Millisecond)
		if j < -time.Millisecond || j >= time.Millisecond {
			t.Fatalf("jitter %v out of bounds", j)
		}
	}
	if got := jitterOf(rng, 0); got != 0 {
		t.Errorf("jitterOf(_, 0) = %v", got)
	}
}
