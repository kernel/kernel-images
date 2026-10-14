package devtoolsproxy

import (
	"context"
	"encoding/json"

	"github.com/coder/websocket"
	"github.com/kernel/kernel-images/server/lib/ambientmouse"
)

// InputGuard is the ambient pointer controller as the proxy sees it.
type InputGuard interface {
	Enabled() bool
	Await(ctx context.Context)
	AgentPointer(ev ambientmouse.PointerEvent)
}

// heldMethods can move the pointer, submit a form, or leave the page. Each is
// held until the current page has had its ambient pointer movement, so a fast
// client cannot leave a page before it has seen any.
//
// A navigation started from page script, such as a Runtime.evaluate that
// clicks a link, is not held: the proxy cannot tell it apart from any other
// script.
var heldMethods = map[string]bool{
	"Page.navigate":                    false,
	"Page.reload":                      false,
	"Page.navigateToHistoryEntry":      false,
	"Input.dispatchKeyEvent":           false,
	"Input.insertText":                 false,
	"Input.imeSetComposition":          false,
	"Input.dispatchMouseEvent":         true,
	"Input.dispatchDragEvent":          true,
	"Input.dispatchTouchEvent":         true,
	"Input.emulateTouchFromMouseEvent": true,
	"Input.synthesizePinchGesture":     true,
	"Input.synthesizeScrollGesture":    true,
	"Input.synthesizeTapGesture":       true,
}

type inputGate struct {
	guard InputGuard
}

func newInputGate(guard InputGuard) *inputGate {
	if guard == nil {
		return nil
	}
	return &inputGate{guard: guard}
}

// hold blocks a client command that heldMethods names until the guard
// releases it, then records pointer commands so ambient movement keeps out of
// the client's way. It never refuses a command.
func (g *inputGate) hold(ctx context.Context, mt websocket.MessageType, msg []byte) {
	if g == nil || mt != websocket.MessageText || !g.guard.Enabled() {
		return
	}
	var probe cdpCommandMethod
	if json.Unmarshal(msg, &probe) != nil {
		return
	}
	pointer, held := heldMethods[probe.Method]
	if !held {
		return
	}
	g.guard.Await(ctx)
	if pointer {
		g.guard.AgentPointer(pointerEvent(probe.Method, msg))
	}
}

func pointerEvent(method string, msg []byte) ambientmouse.PointerEvent {
	if method != "Input.dispatchMouseEvent" {
		return ambientmouse.PointerMove
	}
	var cmd struct {
		Params struct {
			Type string `json:"type"`
		} `json:"params"`
	}
	_ = json.Unmarshal(msg, &cmd)
	switch cmd.Params.Type {
	case "mousePressed":
		return ambientmouse.PointerPress
	case "mouseReleased":
		return ambientmouse.PointerRelease
	}
	return ambientmouse.PointerMove
}
