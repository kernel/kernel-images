package devtoolsproxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/kernel/kernel-images/server/lib/events"
	oapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/kernel/kernel-images/server/lib/scaletozero"
)

func TestCdpCommandEvent(t *testing.T) {
	tests := []struct {
		name  string
		frame string
		want  *oapi.BrowserCdpCommandEventData
	}{
		{
			name:  "click reports coordinates and button",
			frame: `{"id":7,"sessionId":"S1","method":"Input.dispatchMouseEvent","params":{"type":"mousePressed","x":120.5,"y":48,"button":"left","clickCount":1}}`,
			want: &oapi.BrowserCdpCommandEventData{
				Method:    "Input.dispatchMouseEvent",
				SessionId: strPtr("S1"),
				EventType: strPtr("mousePressed"),
				X:         float32Ptr(120.5),
				Y:         float32Ptr(48),
				Button:    strPtr("left"),
			},
		},
		{
			name:  "insertText reports length, never the text",
			frame: `{"id":8,"method":"Input.insertText","params":{"text":"hunter2"}}`,
			want: &oapi.BrowserCdpCommandEventData{
				Method:     "Input.insertText",
				TextLength: intPtr(7),
			},
		},
		{
			name:  "a typed character is counted, not captured",
			frame: `{"id":9,"method":"Input.dispatchKeyEvent","params":{"type":"keyDown","key":"a","text":"a"}}`,
			want: &oapi.BrowserCdpCommandEventData{
				Method:     "Input.dispatchKeyEvent",
				EventType:  strPtr("keyDown"),
				TextLength: intPtr(1),
			},
		},
		{
			name:  "a named key is captured",
			frame: `{"id":10,"method":"Input.dispatchKeyEvent","params":{"type":"keyDown","key":"Enter"}}`,
			want: &oapi.BrowserCdpCommandEventData{
				Method:    "Input.dispatchKeyEvent",
				EventType: strPtr("keyDown"),
				NamedKey:  strPtr("Enter"),
			},
		},
		{
			name:  "navigation reports the method without the url",
			frame: `{"id":10,"method":"Page.navigate","params":{"url":"https://example.com/reset?token=abc"}}`,
			want:  &oapi.BrowserCdpCommandEventData{Method: "Page.navigate"},
		},
		{
			name:  "mouseMoved is dropped as a duplicate phase",
			frame: `{"id":11,"method":"Input.dispatchMouseEvent","params":{"type":"mouseMoved","x":1,"y":2}}`,
		},
		{
			name:  "keyUp is dropped as a duplicate phase",
			frame: `{"id":12,"method":"Input.dispatchKeyEvent","params":{"type":"keyUp","key":"a"}}`,
		},
		{
			name:  "char is dropped as a duplicate phase",
			frame: `{"id":13,"method":"Input.dispatchKeyEvent","params":{"type":"char","text":"a"}}`,
		},
		{
			name:  "library bookkeeping is not browser control",
			frame: `{"id":14,"method":"Runtime.callFunctionOn","params":{"functionDeclaration":"() => 1","objectId":"x"}}`,
		},
		{
			name:  "configuration is not browser control",
			frame: `{"id":15,"method":"Emulation.setDeviceMetricsOverride","params":{"width":1920,"height":1080}}`,
		},
		{
			name:  "command results carry no method",
			frame: `{"id":7,"result":{"nodeId":42}}`,
		},
		{
			name:  "upstream events are not commands",
			frame: `{"method":"Network.responseReceived","params":{"requestId":"1"}}`,
		},
		{
			name:  "a nested method does not stand in for the real one",
			frame: `{"id":16,"params":{"method":"Input.dispatchMouseEvent"},"method":"Runtime.evaluate"}`,
		},
		{
			name:  "a nested method ahead of the real one does not hide it",
			frame: `{"id":17,"params":{"method":"Foo.bar","type":"mousePressed","x":4,"y":9},"method":"Input.dispatchMouseEvent"}`,
			want: &oapi.BrowserCdpCommandEventData{
				Method:    "Input.dispatchMouseEvent",
				EventType: strPtr("mousePressed"),
				X:         float32Ptr(4),
				Y:         float32Ptr(9),
			},
		},
		{
			name:  "a param value that looks like the method key does not hide it",
			frame: `{"id":18,"params":{"text":"method"},"method":"Input.insertText"}`,
			want: &oapi.BrowserCdpCommandEventData{
				Method:     "Input.insertText",
				TextLength: intPtr(6),
			},
		},
		{
			name:  "a decomposed character is not mistaken for a named key",
			frame: `{"id":19,"method":"Input.dispatchKeyEvent","params":{"type":"keyDown","key":"é"}}`,
			want: &oapi.BrowserCdpCommandEventData{
				Method:    "Input.dispatchKeyEvent",
				EventType: strPtr("keyDown"),
			},
		},
		{
			name:  "malformed frames are dropped",
			frame: `{"id":17,"method":"Input.insertText","params":`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ev, ok := cdpCommandEvent([]byte(tc.frame))
			if tc.want == nil {
				if ok {
					t.Fatalf("frame produced an event, want none: %s", ev.Data)
				}
				return
			}
			if !ok {
				t.Fatal("frame produced no event")
			}
			if ev.Type != "cdp_command" {
				t.Fatalf("type = %q, want cdp_command", ev.Type)
			}
			if ev.Category != events.Control {
				t.Fatalf("category = %q, want control", ev.Category)
			}
			var got oapi.BrowserCdpCommandEventData
			if err := json.Unmarshal(ev.Data, &got); err != nil {
				t.Fatalf("unmarshal data: %v", err)
			}
			if diff := cdpCommandDataDiff(*tc.want, got); diff != "" {
				t.Fatalf("data mismatch: %s", diff)
			}
		})
	}
}

// The typed round trip above cannot catch a field the payload carries but the
// struct drops, and typed text is the one field that must never appear.
func TestCdpCommandEventOmitsSubmittedText(t *testing.T) {
	for _, frame := range []string{
		`{"id":1,"method":"Input.insertText","params":{"text":"hunter2"}}`,
		`{"id":2,"method":"Input.dispatchKeyEvent","params":{"type":"keyDown","key":"Q","text":"Q","unmodifiedText":"q"}}`,
		`{"id":3,"method":"Page.navigate","params":{"url":"https://example.com/reset?token=abc"}}`,
		`{"id":4,"method":"DOM.setFileInputFiles","params":{"files":["/tmp/passport.pdf"],"objectId":"x"}}`,
	} {
		ev, ok := cdpCommandEvent([]byte(frame))
		if !ok {
			t.Fatalf("frame produced no event: %s", frame)
		}
		payload := string(ev.Data)
		for _, secret := range []string{"hunter2", "Q", "token=abc", "passport"} {
			if strings.Contains(payload, secret) {
				t.Fatalf("payload %s leaked %q", payload, secret)
			}
		}
	}
}

func TestWebSocketProxyHandler_EmitsCdpCommandForClientControlTraffic(t *testing.T) {
	echoSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
		if err != nil {
			t.Errorf("accept failed: %v", err)
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")
		for {
			mt, msg, err := c.Read(r.Context())
			if err != nil {
				return
			}
			// Echo back a frame that names a control method, so the test fails
			// if the proxy reports the upstream direction too.
			if err := c.Write(r.Context(), mt, msg); err != nil {
				return
			}
		}
	}))
	defer echoSrv.Close()

	u, _ := url.Parse(echoSrv.URL)
	u.Scheme = "ws"
	u.Path = "/devtools/browser/x"

	logger := silentLogger()
	mgr := NewUpstreamManager("/dev/null", logger)
	mgr.setCurrent(u.String())

	rp := &recordingPublisher{}
	proxySrv := httptest.NewServer(WebSocketProxyHandler(mgr, logger, false, scaletozero.NewNoopController(), rp.publish, nil))
	defer proxySrv.Close()

	pu, _ := url.Parse(proxySrv.URL)
	pu.Scheme = "ws"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, pu.String(), nil)
	if err != nil {
		t.Fatalf("dial proxy failed: %v", err)
	}

	frames := []string{
		`{"id":1,"method":"Runtime.callFunctionOn","params":{"functionDeclaration":"() => 1"}}`,
		`{"id":2,"method":"Input.dispatchMouseEvent","params":{"type":"mousePressed","x":10,"y":20,"button":"left"}}`,
		`{"id":3,"method":"Input.dispatchMouseEvent","params":{"type":"mouseReleased","x":10,"y":20,"button":"left"}}`,
	}
	for i, frame := range frames {
		if err := conn.Write(ctx, websocket.MessageText, []byte(frame)); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		if _, _, err := conn.Read(ctx); err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
	}

	// Close and wait for cdp_disconnect rather than for a count: the disconnect
	// is published after the pump has drained, so any event the upstream
	// direction wrongly produced has already been recorded by the time the
	// count is asserted.
	_ = conn.Close(websocket.StatusNormalClosure, "bye")
	if !waitForCondition(5*time.Second, func() bool {
		for _, ev := range rp.snapshot() {
			if ev.Type == "cdp_disconnect" {
				return true
			}
		}
		return false
	}) {
		t.Fatal("proxy never published cdp_disconnect")
	}

	commands := commandEvents(rp.snapshot())
	if len(commands) != 2 {
		t.Fatalf("cdp_command count = %d, want 2 (the two client gestures, none for the echoes or for Runtime.callFunctionOn)", len(commands))
	}
}

func commandEvents(evs []events.Event) []events.Event {
	var out []events.Event
	for _, ev := range evs {
		if ev.Type == "cdp_command" {
			out = append(out, ev)
		}
	}
	return out
}

func cdpCommandDataDiff(want, got oapi.BrowserCdpCommandEventData) string {
	wantJSON, _ := json.Marshal(want)
	gotJSON, _ := json.Marshal(got)
	if string(wantJSON) == string(gotJSON) {
		return ""
	}
	return "want " + string(wantJSON) + ", got " + string(gotJSON)
}

func strPtr(s string) *string       { return &s }
func intPtr(i int) *int             { return &i }
func float32Ptr(f float32) *float32 { return &f }
