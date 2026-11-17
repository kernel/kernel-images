package camera

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os/exec"
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
