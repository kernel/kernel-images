package camera

import (
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"testing"
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
