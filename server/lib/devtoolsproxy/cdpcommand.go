package devtoolsproxy

import (
	"encoding/json"

	"github.com/kernel/kernel-images/server/lib/events"
	oapi "github.com/kernel/kernel-images/server/lib/oapi"
)

// cdpCommandMethod is the first of two decodes: the method alone. With no
// Params field, encoding/json walks the arguments without copying them, so
// deciding that a large Runtime.callFunctionOn is not browser control costs a
// scan rather than a megabyte. It is a real decode rather than a scan for the
// method name, so an escaped name like "Input.\u0064ispatchMouseEvent"
// resolves to the method it actually names.
type cdpCommandMethod struct {
	Method string `json:"method"`
}

// cdpCommand is the JSON-RPC envelope of a client command, matching the shape
// the rest of the repo uses (cdpmonitor, cdpclient). Params stays raw so the
// per-method sanitizer decides what is worth decoding. Only a frame that
// already named a supported method is decoded this far.
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
// or reports false when the frame is not a browser-control command or its
// method is excluded by telemetry configuration. ts is when the command
// reached Chromium, passed in so time spent queued for classification does not
// show up as event time.
//
// Every supported command gets one event. Subtypes like mouseMoved and keyUp
// are commands in their own right — a mouseMoved with buttons held is a drag
// path, a keyUp releases a modifier — so the stream is never coalesced down to
// what looks like the interesting phases.
func cdpCommandEvent(msg []byte, ts int64, excluded map[string]struct{}) (events.Event, bool) {
	var probe cdpCommandMethod
	if err := json.Unmarshal(msg, &probe); err != nil {
		return events.Event{}, false
	}
	sanitize, ok := sanitizers[probe.Method]
	if !ok {
		return events.Event{}, false
	}
	// Excluding a method suppresses only its event; the raw command was
	// forwarded to Chromium before this ran.
	if _, skip := excluded[probe.Method]; skip {
		return events.Event{}, false
	}

	// Only now is the frame worth copying arguments out of. A browser-control
	// command is small, so the second pass is cheap.
	var cmd cdpCommand
	if err := json.Unmarshal(msg, &cmd); err != nil {
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
