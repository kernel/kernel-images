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
	ID        *int64          `json:"id"`
	Method    string          `json:"method"`
	SessionID string          `json:"sessionId"`
	Params    json.RawMessage `json:"params"`

	// connectionID names the proxy connection the command arrived on. It comes
	// from the observer rather than the frame, so concurrent clients driving one
	// browser can be told apart.
	connectionID string
}

func (c cdpCommand) sessionID() *string {
	if c.SessionID == "" {
		return nil
	}
	return clipIDPtr(&c.SessionID)
}

func (c cdpCommand) connID() *string {
	if c.connectionID == "" {
		return nil
	}
	return clipIDPtr(&c.connectionID)
}

// supportedMethod reports the browser-control method a frame names, if any. It
// is the admission test, so it copies nothing out of the frame: with no Params
// field, encoding/json walks the arguments without materializing them, and a
// large Runtime.callFunctionOn costs a scan rather than a megabyte.
func supportedMethod(msg []byte) (string, bool) {
	var probe cdpCommandMethod
	if err := json.Unmarshal(msg, &probe); err != nil {
		return "", false
	}
	if _, ok := sanitizers[probe.Method]; !ok {
		return "", false
	}
	return probe.Method, true
}

// cdpCommandEvent builds the cdp_command event for a frame whose method
// supportedMethod already resolved, or reports false when the arguments do not
// decode. ts is when the command reached Chromium, passed in so time spent
// queued for classification does not show up as event time.
//
// Every supported command gets one event. Subtypes like mouseMoved and keyUp
// are commands in their own right — a mouseMoved with buttons held is a drag
// path, a keyUp releases a modifier — so the stream is never coalesced down to
// what looks like the interesting phases.
func cdpCommandEvent(msg []byte, ts int64, connectionID, method string) (events.Event, bool) {
	sanitize, ok := sanitizers[method]
	if !ok {
		return events.Event{}, false
	}
	// Only now is the frame worth copying arguments out of. A browser-control
	// command is small, so this second pass is cheap.
	cmd := cdpCommand{connectionID: connectionID}
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
