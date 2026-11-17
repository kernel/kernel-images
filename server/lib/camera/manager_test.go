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
	"testing"
	"time"
)

func TestLoopbackModuleArgs(t *testing.T) {
	got := loopbackModuleArgs(0, 1)
	want := []string{
		"v4l2loopback", "devices=2", "video_nr=0,1",
		"card_label=Integrated Camera,kernel-ingest", "exclusive_caps=1,1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %v, want %v", got, want)
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
	if got := m.frameSize(); got != 1280*720*3/2 {
		t.Errorf("frameSize = %d", got)
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
	go func() { m.readIngest(ctx, "format=yuv420p"); close(done) }()
	select {
	case <-done:
	case <-time.After(ingestStallTimeout + 4*time.Second):
		t.Fatal("readIngest did not return after a stall")
	}
}

func TestMixerForwardsLiveFrameWithoutWaitingForTick(t *testing.T) {
	// Stand in for ffmpeg with a script that copies the mixer's stdin to a file.
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	fake := filepath.Join(dir, "ffmpeg")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexec cat > "+out+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// FPS 1 makes the idle interval a full second, so a frame that shows up well
	// before then can only have been forwarded on arrival.
	m := New(Config{Width: 4, Height: 2, FPS: 1, FFmpegPath: fake}, nil)
	placeholder := bytes.Repeat([]byte{'P'}, m.frameSize())
	live := bytes.Repeat([]byte{'L'}, m.frameSize())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = m.runMixer(ctx, placeholder) }()

	time.Sleep(100 * time.Millisecond)
	m.mu.Lock()
	m.lastFrame, m.lastAt = live, time.Now()
	m.mu.Unlock()
	m.newFrame <- struct{}{}

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if b, _ := os.ReadFile(out); bytes.Contains(b, live) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("live frame was not forwarded before the next idle tick")
}

func TestIngestProducing(t *testing.T) {
	old := sysfsVideo
	t.Cleanup(func() { sysfsVideo = old })
	sysfsVideo = t.TempDir()
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
		if got := ingestProducing("/dev/video1"); got != tc.want {
			t.Errorf("state %q: got %v, want %v", tc.state, got, tc.want)
		}
	}
	// No sysfs attribute: fall back to whether the device node exists.
	if !ingestProducing("/dev/null") {
		t.Error("existing device without sysfs state should count as producing")
	}
	if ingestProducing("/dev/does-not-exist") {
		t.Error("missing device should not count as producing")
	}
}
