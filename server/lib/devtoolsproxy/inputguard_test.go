package devtoolsproxy

import (
	"context"
	"testing"

	"github.com/coder/websocket"
	"github.com/kernel/kernel-images/server/lib/ambientmouse"
	"github.com/stretchr/testify/assert"
)

type fakeGuard struct {
	enabled bool
	awaited []string
	pointer []ambientmouse.PointerEvent
	method  string
}

func (g *fakeGuard) Enabled() bool { return g.enabled }

func (g *fakeGuard) Await(context.Context) { g.awaited = append(g.awaited, g.method) }

func (g *fakeGuard) AgentPointer(ev ambientmouse.PointerEvent) { g.pointer = append(g.pointer, ev) }

func TestInputGateHoldsInputAndNavigation(t *testing.T) {
	tests := []struct {
		name    string
		frame   string
		held    bool
		pointer []ambientmouse.PointerEvent
	}{
		{"navigate", `{"id":1,"method":"Page.navigate","params":{"url":"https://example.com"}}`, true, nil},
		{"reload", `{"id":1,"method":"Page.reload"}`, true, nil},
		{"key", `{"id":1,"method":"Input.dispatchKeyEvent","params":{"type":"keyDown","key":"Enter"}}`, true, nil},
		{"mouse move", `{"id":1,"method":"Input.dispatchMouseEvent","params":{"type":"mouseMoved","x":1,"y":2}}`, true, []ambientmouse.PointerEvent{ambientmouse.PointerMove}},
		{"mouse press", `{"id":1,"method":"Input.dispatchMouseEvent","params":{"type":"mousePressed","x":1,"y":2}}`, true, []ambientmouse.PointerEvent{ambientmouse.PointerPress}},
		{"mouse release", `{"id":1,"method":"Input.dispatchMouseEvent","params":{"type":"mouseReleased","x":1,"y":2}}`, true, []ambientmouse.PointerEvent{ambientmouse.PointerRelease}},
		{"escaped method", `{"id":1,"method":"Input.dispatchMouseEvent","params":{"type":"mousePressed"}}`, true, []ambientmouse.PointerEvent{ambientmouse.PointerPress}},
		{"touch", `{"id":1,"method":"Input.dispatchTouchEvent","params":{"type":"touchStart","touchPoints":[]}}`, true, []ambientmouse.PointerEvent{ambientmouse.PointerMove}},
		{"evaluate", `{"id":1,"method":"Runtime.evaluate","params":{"expression":"1"}}`, false, nil},
		{"screenshot", `{"id":1,"method":"Page.captureScreenshot"}`, false, nil},
		{"malformed", `{"method":`, false, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			guard := &fakeGuard{enabled: true, method: tt.name}
			newInputGate(guard).hold(context.Background(), websocket.MessageText, []byte(tt.frame))
			assert.Equal(t, tt.held, len(guard.awaited) == 1)
			assert.Equal(t, tt.pointer, guard.pointer)
		})
	}
}

func TestInputGateIgnoresFramesWhileDisabled(t *testing.T) {
	guard := &fakeGuard{}
	newInputGate(guard).hold(context.Background(), websocket.MessageText, []byte(`{"id":1,"method":"Page.navigate"}`))
	assert.Empty(t, guard.awaited)

	guard.enabled = true
	newInputGate(guard).hold(context.Background(), websocket.MessageBinary, []byte(`{"id":1,"method":"Page.navigate"}`))
	assert.Empty(t, guard.awaited)

	var nilGate *inputGate
	nilGate.hold(context.Background(), websocket.MessageText, []byte(`{"id":1,"method":"Page.navigate"}`))
}
