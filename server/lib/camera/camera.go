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
// way to feed synthetic or recorded video to an identity check — liveness
// vendors legitimately reject virtual cameras, and that is out of scope here.
package camera

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// Defaults for the capture pipeline. Width/Height are the fixed native mode the
// browser-facing device advertises; see Manager docs on orientation.
const (
	DefaultOutputDevice = "/dev/video0"
	DefaultIngestDevice = "/dev/video1"
	DefaultWidth        = 1280
	DefaultHeight       = 720
	DefaultFPS          = 30
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
	}
}
