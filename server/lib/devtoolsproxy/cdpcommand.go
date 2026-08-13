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

// classification is what became of one forwarded frame. Anything other than
// classifyEvent is a frame that produced no event, and the caller counts each
// reason apart: configuration is not the same as a command nobody could read.
type classification int

const (
	// classifyIgnored: not a browser-control command. Not a loss — most client
	// traffic is DOM and Runtime bookkeeping that was never in scope.
	classifyIgnored classification = iota
	// classifyExcluded: supported, but configured out via excluded_methods.
	classifyExcluded
	// classifyMalformed: supported, but the frame or its arguments did not
	// decode. The command was still forwarded, so this is a real loss.
	classifyMalformed
	classifyEvent
)

// cdpCommandEvent builds the cdp_command event for a client-to-upstream frame
// and reports what became of the frame. ts is when the command reached
// Chromium, passed in so time spent queued for classification does not show up
// as event time.
//
// Every supported command gets one event. Subtypes like mouseMoved and keyUp
// are commands in their own right — a mouseMoved with buttons held is a drag
// path, a keyUp releases a modifier — so the stream is never coalesced down to
// what looks like the interesting phases.
func cdpCommandEvent(msg []byte, ts int64, connectionID string, excluded map[string]struct{}) (events.Event, classification) {
	var probe cdpCommandMethod
	if err := json.Unmarshal(msg, &probe); err != nil {
		// Not decodable as a CDP frame at all, so nothing says it was a command.
		return events.Event{}, classifyIgnored
	}
	sanitize, ok := sanitizers[probe.Method]
	if !ok {
		return events.Event{}, classifyIgnored
	}
	// Excluding a method suppresses only its event; the raw command was
	// forwarded to Chromium before this ran.
	if _, skip := excluded[probe.Method]; skip {
		return events.Event{}, classifyExcluded
	}

	// Only now is the frame worth copying arguments out of. A browser-control
	// command is small, so the second pass is cheap.
	cmd := cdpCommand{connectionID: connectionID}
	if err := json.Unmarshal(msg, &cmd); err != nil {
		return events.Event{}, classifyMalformed
	}
	data, err := sanitize(cmd)
	if err != nil {
		return events.Event{}, classifyMalformed
	}
	payload, err := json.Marshal(data)
	if err != nil {
		return events.Event{}, classifyMalformed
	}
	return events.Event{
		Ts:       ts,
		Type:     "cdp_command",
		Category: events.Control,
		Source:   oapi.BrowserEventSource{Kind: oapi.KernelApi},
		Data:     payload,
	}, classifyEvent
}
