package devtoolsproxy

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/kernel/kernel-images/server/lib/events"
	oapi "github.com/kernel/kernel-images/server/lib/oapi"
)

// controlMethods are the CDP methods that drive the browser the way an agent
// does: input gestures, navigation, dialog handling, file selection and
// screenshots. Everything else a client sends over the proxy is either
// configuration (Emulation, Network.enable) or DOM/Runtime bookkeeping a
// library issues on the caller's behalf, so it stays out of the stream.
var controlMethods = lookup(`
	Input.dispatchMouseEvent Input.dispatchKeyEvent Input.dispatchTouchEvent
	Input.dispatchDragEvent Input.insertText
	Input.synthesizeScrollGesture Input.synthesizeTapGesture
	Page.navigate Page.navigateToHistoryEntry Page.reload
	Page.captureScreenshot Page.handleJavaScriptDialog
	DOM.setFileInputFiles
`)

// skipEventTypes drops the phases that say nothing the kept phases don't. A
// click arrives as mouseMoved, mousePressed, mouseReleased and a keystroke as
// rawKeyDown/keyDown, char, keyUp; press and release are worth having (a
// long-held button is visible in the gap), the rest is filler. mouseMoved also
// arrives in bulk from humanized cursor paths.
var skipEventTypes = lookup(`mouseMoved keyUp char`)

// namedKeys are the KeyboardEvent.key values worth reading back: keys that
// command the page rather than type into it. This is an allowlist rather than a
// "more than one character" rule because key for typed input can itself be
// multi-rune — a decomposed "é" is two runes and is the letter someone typed,
// so a length rule would publish it.
var namedKeys = lookup(`
	Enter Tab Escape Backspace Delete Insert
	Home End PageUp PageDown ArrowUp ArrowDown ArrowLeft ArrowRight
	Shift Control Alt Meta CapsLock NumLock ScrollLock
	ContextMenu Pause PrintScreen
	F1 F2 F3 F4 F5 F6 F7 F8 F9 F10 F11 F12
`)

// lookup builds a membership set from a whitespace-separated list, so the lists
// above read as lists.
func lookup(words string) map[string]struct{} {
	out := make(map[string]struct{})
	for _, word := range strings.Fields(words) {
		out[word] = struct{}{}
	}
	return out
}

var methodKey = []byte(`"method"`)

// cdpCommand is the subset of a CDP command frame this package reports. Params
// covers every allowlisted method with one shape rather than a type per method:
// the fields are the ones worth reporting, and a method that carries none of
// them reports its name alone.
type cdpCommand struct {
	Method    string    `json:"method"`
	SessionId string    `json:"sessionId"`
	Params    cdpParams `json:"params"`
}

type cdpParams struct {
	Type   string   `json:"type"`
	X      *float32 `json:"x"`
	Y      *float32 `json:"y"`
	Button string   `json:"button"`
	Text   string   `json:"text"`
	Key    string   `json:"key"`
}

// cdpCommandEvent builds the cdp_command event for a client-to-upstream frame,
// or reports false when the frame is not a browser-control command. ts is when
// the command reached Chromium, passed in so time spent queued for
// classification does not show up as event time. Params are reported as shape
// only — coordinates, button, event type and the length of typed text — never
// the text itself, which on a login page is the password.
func cdpCommandEvent(msg []byte, ts int64) (events.Event, bool) {
	if !mayBeControlCommand(msg) {
		return events.Event{}, false
	}

	var cmd cdpCommand
	if err := json.Unmarshal(msg, &cmd); err != nil {
		return events.Event{}, false
	}
	// The parsed top-level method decides, not the scanned one: a client is free
	// to nest a "method" key inside params ahead of its own.
	if _, ok := controlMethods[cmd.Method]; !ok {
		return events.Event{}, false
	}
	if _, skip := skipEventTypes[cmd.Params.Type]; skip {
		return events.Event{}, false
	}

	data := oapi.BrowserCdpCommandEventData{
		Method:    cmd.Method,
		SessionId: nilIfEmpty(cmd.SessionId),
		EventType: nilIfEmpty(cmd.Params.Type),
		Button:    nilIfEmpty(cmd.Params.Button),
		X:         cmd.Params.X,
		Y:         cmd.Params.Y,
	}
	if n := utf8.RuneCountInString(cmd.Params.Text); n > 0 {
		data.TextLength = &n
	}
	if _, named := namedKeys[cmd.Params.Key]; named {
		data.NamedKey = &cmd.Params.Key
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

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// mayBeControlCommand rejects frames that cannot be a browser-control command
// without unmarshalling them, so the hot path — a large Runtime.callFunctionOn
// payload — costs a scan instead of a full parse. It checks every "method" the
// frame contains, not just the first: a client is free to serialize params (or
// a value that reads like the key) ahead of its own method, and a frame whose
// real method is a control method must not be dropped because something else
// matched first. Passing here only buys a parse; cdpCommandEvent decides.
func mayBeControlCommand(msg []byte) bool {
	for off := 0; off < len(msg); {
		i := bytes.Index(msg[off:], methodKey)
		if i < 0 {
			return false
		}
		off += i + len(methodKey)
		rest := msg[off:]
		open := bytes.IndexByte(rest, '"')
		if open < 0 {
			return false
		}
		// A colon is the only thing that may sit between the key and its value.
		if !bytes.Contains(rest[:open], []byte(":")) {
			continue
		}
		value := rest[open+1:]
		end := bytes.IndexByte(value, '"')
		if end < 0 {
			return false
		}
		if _, ok := controlMethods[string(value[:end])]; ok {
			return true
		}
	}
	return false
}
