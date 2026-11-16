package camera

import (
	"encoding/json"
	"net/http/httptest"
	"os/exec"
	"reflect"
	"syscall"
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

func TestKillOnStallKillsIdleReader(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skip("sleep unavailable:", err)
	}
	done := make(chan struct{})
	defer close(done)
	start := time.Now()
	go killOnStall(cmd, make(chan struct{}), done, 100*time.Millisecond)
	_ = cmd.Wait()
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("idle reader not killed promptly: %v", d)
	}
}

func TestKillOnStallSparesActiveReader(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skip("sleep unavailable:", err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	frames := make(chan struct{}, 1)
	done := make(chan struct{})
	go killOnStall(cmd, frames, done, 150*time.Millisecond)
	for i := 0; i < 10; i++ {
		frames <- struct{}{}
		time.Sleep(50 * time.Millisecond)
	}
	close(done)
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("active reader was killed: %v", err)
	}
}
