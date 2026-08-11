package devtoolsproxy

import (
	"encoding/json"

	"github.com/kernel/kernel-images/server/lib/events"
	oapi "github.com/kernel/kernel-images/server/lib/oapi"
)

// cdpCommand is the JSON-RPC envelope of a client command, matching the shape
// the rest of the repo uses (cdpmonitor, cdpclient). Params stays raw so the
// per-method sanitizer decides what is worth decoding: a method with no entry
// in sanitizers never has its arguments touched.
type cdpCommand struct {
	Method    string          `json:"method"`
	SessionID string          `json:"sessionId"`
	Params    json.RawMessage `json:"params"`
}

func (c cdpCommand) sessionID() *string {
	if c.SessionID == "" {
		return nil
	}
	return &c.SessionID
}

// cdpCommandEvent builds the cdp_command event for a client-to-upstream frame,
// or reports false when the frame is not a browser-control command. ts is when
// the command reached Chromium, passed in so time spent queued for
// classification does not show up as event time.
//
// Every supported command gets one event. Subtypes like mouseMoved and keyUp
// are commands in their own right — a mouseMoved with buttons held is a drag
// path, a keyUp releases a modifier — so the stream is never coalesced down to
// what looks like the interesting phases.
func cdpCommandEvent(msg []byte, ts int64) (events.Event, bool) {
	var cmd cdpCommand
	if err := json.Unmarshal(msg, &cmd); err != nil {
		return events.Event{}, false
	}
	sanitize, ok := sanitizers[cmd.Method]
	if !ok {
		return events.Event{}, false
	}
	data, err := sanitize(cmd)
	if err != nil {
		return events.Event{}, false
	}
	payload, err := json.Marshal(data)
	if err != nil {
		return events.Event{}, false
	}
	return events.Event{
		Ts:       ts,
		Type:     "cdp_command",
		Category: events.Control,
		Source:   oapi.BrowserEventSource{Kind: oapi.KernelApi},
		Data:     payload,
	}, true
}
