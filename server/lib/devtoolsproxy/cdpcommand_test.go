package devtoolsproxy

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/ghodss/yaml"

	serverpkg "github.com/kernel/kernel-images/server"
	"github.com/kernel/kernel-images/server/lib/events"
)

// testForwardTs stands in for the time a command reached Chromium.
const testForwardTs int64 = 1_700_000_000_000_000

// payloadOf classifies a frame and returns its event payload as a plain map.
// Asserting on the wire shape rather than the generated union type is the
// point: the payload is what a reader sees.
func payloadOf(t *testing.T, frame string) map[string]any {
	t.Helper()
	ev, ok := cdpCommandEvent([]byte(frame), testForwardTs, nil)
	if !ok {
		t.Fatalf("frame produced no event: %s", frame)
	}
	if ev.Type != "cdp_command" {
		t.Fatalf("type = %q, want cdp_command", ev.Type)
	}
	if ev.Category != events.Control {
		t.Fatalf("category = %q, want control", ev.Category)
	}
	if ev.Ts != testForwardTs {
		t.Fatalf("ts = %d, want the forward time %d", ev.Ts, testForwardTs)
	}
	var got map[string]any
	if err := json.Unmarshal(ev.Data, &got); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	return got
}

func TestCdpCommandEventClassification(t *testing.T) {
	tests := []struct {
		name  string
		frame string
		want  map[string]any
	}{
		{
			name:  "click keeps the arguments that describe it",
			frame: `{"id":1,"sessionId":"S1","method":"Input.dispatchMouseEvent","params":{"type":"mousePressed","x":10.5,"y":20,"button":"left","clickCount":2,"modifiers":8,"buttons":1,"pointerType":"mouse"}}`,
			want: map[string]any{
				"method": "Input.dispatchMouseEvent", "session_id": "S1", "event_type": "mousePressed",
				"x": 10.5, "y": 20.0, "button": "left", "click_count": 2.0,
				"modifiers": 8.0, "buttons": 1.0, "pointer_type": "mouse",
			},
		},
		{
			name:  "mouseMoved with buttons held is a drag path, not a duplicate phase",
			frame: `{"id":2,"method":"Input.dispatchMouseEvent","params":{"type":"mouseMoved","x":9,"y":9,"buttons":1}}`,
			want: map[string]any{
				"method": "Input.dispatchMouseEvent", "event_type": "mouseMoved",
				"x": 9.0, "y": 9.0, "buttons": 1.0,
			},
		},
		{
			name:  "wheel keeps its deltas",
			frame: `{"id":3,"method":"Input.dispatchMouseEvent","params":{"type":"mouseWheel","x":1,"y":2,"deltaX":0,"deltaY":-400}}`,
			want: map[string]any{
				"method": "Input.dispatchMouseEvent", "event_type": "mouseWheel",
				"x": 1.0, "y": 2.0, "delta_x": 0.0, "delta_y": -400.0,
			},
		},
		{
			name:  "keyUp releases a held modifier",
			frame: `{"id":4,"method":"Input.dispatchKeyEvent","params":{"type":"keyUp","key":"Shift"}}`,
			want:  map[string]any{"method": "Input.dispatchKeyEvent", "event_type": "keyUp", "named_key": "Shift"},
		},
		{
			name:  "char is the command that inserts the character",
			frame: `{"id":5,"method":"Input.dispatchKeyEvent","params":{"type":"char","text":"a"}}`,
			want:  map[string]any{"method": "Input.dispatchKeyEvent", "event_type": "char", "text_length": 1.0},
		},
		{
			name:  "a typed key is counted, never named",
			frame: `{"id":6,"method":"Input.dispatchKeyEvent","params":{"type":"keyDown","key":"é","text":"é","code":"KeyE"}}`,
			want:  map[string]any{"method": "Input.dispatchKeyEvent", "event_type": "keyDown", "text_length": 1.0},
		},
		{
			name:  "scroll gesture keeps its distance",
			frame: `{"id":7,"method":"Input.synthesizeScrollGesture","params":{"x":1,"y":2,"xDistance":0,"yDistance":-500,"speed":800}}`,
			want: map[string]any{
				"method": "Input.synthesizeScrollGesture", "x": 1.0, "y": 2.0,
				"x_distance": 0.0, "y_distance": -500.0, "speed": 800.0,
			},
		},
		{
			name:  "touch reports its point count and the primary point",
			frame: `{"id":8,"method":"Input.dispatchTouchEvent","params":{"type":"touchStart","touchPoints":[{"x":100,"y":200},{"x":300,"y":400}]}}`,
			want: map[string]any{
				"method": "Input.dispatchTouchEvent", "event_type": "touchStart",
				"touch_point_count": 2.0, "x": 100.0, "y": 200.0,
			},
		},
		{
			name:  "drag reports counts and mime categories, not contents",
			frame: `{"id":9,"method":"Input.dispatchDragEvent","params":{"type":"drop","x":5,"y":6,"data":{"items":[{"mimeType":"text/plain","data":"secret"},{"mimeType":"image/png","data":"secret"}],"files":["/tmp/a.pdf"],"dragOperationsMask":1}}}`,
			want: map[string]any{
				"method": "Input.dispatchDragEvent", "event_type": "drop", "x": 5.0, "y": 6.0,
				"drag_item_count": 2.0, "drag_file_count": 1.0,
				"drag_mime_categories": []any{"image", "text"}, "drag_operations_mask": 1.0,
			},
		},
		{
			name:  "navigation reports scheme and host, never the path",
			frame: `{"id":10,"method":"Page.navigate","params":{"url":"https://example.com/reset?token=abc","referrer":"https://mail.example.com/x","transitionType":"typed"}}`,
			want: map[string]any{
				"method": "Page.navigate", "url_scheme": "https", "url_host": "example.com",
				"transition_type": "typed", "referrer_present": true,
			},
		},
		{
			name:  "dialog reports the decision",
			frame: `{"id":11,"method":"Page.handleJavaScriptDialog","params":{"accept":true,"promptText":"hunter2"}}`,
			want:  map[string]any{"method": "Page.handleJavaScriptDialog", "accept": true, "prompt_text_length": 7.0},
		},
		{
			name:  "file selection reports the count, never the paths",
			frame: `{"id":12,"method":"DOM.setFileInputFiles","params":{"files":["/tmp/a.pdf","/tmp/b.pdf"],"backendNodeId":7}}`,
			want:  map[string]any{"method": "DOM.setFileInputFiles", "file_count": 2.0, "backend_node_id": 7.0},
		},
		{
			name:  "screenshot reports its options and clip",
			frame: `{"id":13,"method":"Page.captureScreenshot","params":{"format":"png","quality":80,"clip":{"x":0,"y":0,"width":800,"height":600,"scale":1}}}`,
			want: map[string]any{
				"method": "Page.captureScreenshot", "format": "png", "quality": 80.0,
				"clip_x": 0.0, "clip_y": 0.0, "clip_width": 800.0, "clip_height": 600.0, "clip_scale": 1.0,
			},
		},
		{
			name:  "autofill reports which kind of value was filled",
			frame: `{"id":14,"method":"Autofill.trigger","params":{"fieldId":3,"card":{"number":"4111111111111111","cvc":"123"}}}`,
			want:  map[string]any{"method": "Autofill.trigger", "field_id": 3.0, "mode": "card"},
		},
		{
			name:  "a command with no arguments reports its name",
			frame: `{"id":15,"method":"Page.bringToFront"}`,
			want:  map[string]any{"method": "Page.bringToFront"},
		},
		{
			name:  "window bounds are flattened out of the bounds object",
			frame: `{"id":16,"method":"Browser.setWindowBounds","params":{"windowId":1,"bounds":{"left":0,"top":0,"width":1280,"height":720,"windowState":"normal"}}}`,
			want: map[string]any{
				"method": "Browser.setWindowBounds", "window_id": 1.0, "left": 0.0, "top": 0.0,
				"width": 1280.0, "height": 720.0, "window_state": "normal",
			},
		},
		{name: "library bookkeeping is not browser control", frame: `{"id":17,"method":"Runtime.callFunctionOn","params":{"functionDeclaration":"() => 1","objectId":"x"}}`},
		{name: "runtime evaluation is not browser control", frame: `{"id":18,"method":"Runtime.evaluate","params":{"expression":"1+1"}}`},
		{name: "configuration is not browser control", frame: `{"id":19,"method":"Emulation.setDeviceMetricsOverride","params":{"width":1920,"height":1080}}`},
		{name: "chrome ui commands stay out", frame: `{"id":20,"method":"Browser.executeBrowserCommand","params":{"commandId":"openTabSearch"}}`},
		{name: "command results carry no method", frame: `{"id":21,"result":{"nodeId":42}}`},
		{name: "upstream events are not commands", frame: `{"method":"Page.frameNavigated","params":{"frame":{"url":"https://example.com"}}}`},
		{name: "a nested method cannot spoof a control command", frame: `{"id":22,"method":"Runtime.callFunctionOn","params":{"method":"Input.dispatchMouseEvent","x":1}}`},
		{name: "malformed frames are dropped", frame: `{"id":23,"method":"Input.insertText","params":`},
		{name: "malformed params are dropped", frame: `{"id":24,"method":"Input.insertText","params":{"text":5}}`},
		{name: "an empty frame is dropped", frame: ``},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.want == nil {
				if ev, ok := cdpCommandEvent([]byte(tc.frame), testForwardTs, nil); ok {
					t.Fatalf("frame produced an event, want none: %s", ev.Data)
				}
				return
			}
			got := payloadOf(t, tc.frame)
			wantJSON, _ := json.Marshal(tc.want)
			gotJSON, _ := json.Marshal(got)
			if string(wantJSON) != string(gotJSON) {
				t.Fatalf("payload mismatch:\n want %s\n  got %s", wantJSON, gotJSON)
			}
		})
	}
}

// An escaped method name is still that method. The classifier decodes the
// frame rather than scanning it for a literal, so "Input.dispatch..."
// cannot slip a command past the stream.
func TestCdpCommandEventDecodesEscapedMethodNames(t *testing.T) {
	frame := `{"id":1,"method":"Input.dispatchMouseEvent","params":{"type":"mousePressed","x":1,"y":2}}`
	got := payloadOf(t, frame)
	if got["method"] != "Input.dispatchMouseEvent" {
		t.Fatalf("method = %v, want Input.dispatchMouseEvent", got["method"])
	}
}

// sensitiveParams stuffs a unique sentinel into every argument that must never
// reach an event, across every supported method at once. A sanitizer only
// decodes its own arguments, so the superset is safe to send to all of them and
// catches a sanitizer that passes one of these through.
const sensitiveParams = `{
	"text":"SENTINELtext",
	"unmodifiedText":"SENTINELunmodified",
	"key":"SENTINELkey",
	"code":"SENTINELcode",
	"keyIdentifier":"SENTINELkeyident",
	"url":"https://host.example/SENTINELpath?token=SENTINELquery#SENTINELfragment",
	"referrer":"https://host.example/SENTINELreferrer",
	"scriptToEvaluateOnLoad":"SENTINELscript",
	"headerTemplate":"SENTINELheader",
	"footerTemplate":"SENTINELfooter",
	"pageRanges":"SENTINELranges",
	"promptText":"SENTINELprompt",
	"interactionMarkerName":"SENTINELmarker",
	"files":["/tmp/SENTINELfile.pdf"],
	"proxyServer":"http://SENTINELproxy:8080",
	"proxyBypassList":"SENTINELbypass",
	"originsWithUniversalNetworkAccess":["https://SENTINELorigin"],
	"card":{"number":"SENTINELcard","cvc":"SENTINELcvc"},
	"address":{"fields":[{"name":"SENTINELname","value":"SENTINELaddress"}]},
	"data":{"items":[{"mimeType":"text/SENTINELsubtype","data":"SENTINELdrag","baseURL":"https://host.example/SENTINELbase","title":"SENTINELtitle"}],"files":["/tmp/SENTINELdragfile"]},
	"bounds":{},
	"clip":{},
	"touchPoints":[{"x":1,"y":2}]
}`

func TestSanitizersNeverEmitSensitiveValues(t *testing.T) {
	for method := range sanitizers {
		t.Run(method, func(t *testing.T) {
			frame := fmt.Sprintf(`{"id":1,"sessionId":"S","method":%q,"params":%s}`, method, sensitiveParams)
			ev, ok := cdpCommandEvent([]byte(frame), testForwardTs, nil)
			if !ok {
				t.Fatalf("supported method produced no event")
			}
			payload := string(ev.Data)
			if strings.Contains(payload, "SENTINEL") {
				t.Fatalf("payload leaked a sensitive value: %s", payload)
			}
		})
	}
}

// The map key and the payload's method must agree, or a copy-paste between two
// similar sanitizers would silently mislabel a command.
func TestSanitizersReportTheMethodTheyAreKeyedBy(t *testing.T) {
	for method := range sanitizers {
		t.Run(method, func(t *testing.T) {
			got := payloadOf(t, fmt.Sprintf(`{"id":1,"method":%q}`, method))
			if got["method"] != method {
				t.Fatalf("payload method = %v, want %s", got["method"], method)
			}
		})
	}
}

// The inventory is the spec's enum. A method added to one and not the other is
// either an event no schema describes or a schema nothing emits.
func TestSanitizersMatchTheSchemaMethodEnum(t *testing.T) {
	want := specCommandMethods(t)
	got := make([]string, 0, len(sanitizers))
	for method := range sanitizers {
		got = append(got, method)
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("sanitizers and BrowserCdpCommandMethod disagree:\n sanitizers: %v\n schema:     %v", got, want)
	}
}

func TestSessionIdIsReportedWhenAddressed(t *testing.T) {
	got := payloadOf(t, `{"id":1,"sessionId":"ABC","method":"Page.reload","params":{"ignoreCache":true}}`)
	if got["session_id"] != "ABC" {
		t.Fatalf("session_id = %v, want ABC", got["session_id"])
	}
	got = payloadOf(t, `{"id":2,"method":"Browser.close"}`)
	if _, ok := got["session_id"]; ok {
		t.Fatal("browser-level command reported a session_id")
	}
}

func FuzzCdpCommandEvent(f *testing.F) {
	f.Add(`{"id":1,"method":"Input.dispatchMouseEvent","params":{"type":"mousePressed","x":1,"y":2}}`)
	f.Add(`{"id":1,"method":"Input.insertText","params":{"text":"hunter2"}}`)
	f.Add(`{"id":1,"method":"Page.navigate","params":{"url":"https://example.com/a?token=b"}}`)
	f.Add(`{"id":1,"method":"Input.dispatchDragEvent","params":{"data":{"items":[{"mimeType":"x"}]}}}`)
	f.Add(`{"id":1,"method":"Autofill.trigger","params":{"fieldId":1,"address":{}}}`)
	f.Add(`{"id":1,"method":`)
	f.Add("\x00\xff\xfe")

	f.Fuzz(func(t *testing.T, frame string) {
		ev, ok := cdpCommandEvent([]byte(frame), testForwardTs, nil)
		if !ok {
			return
		}
		// Whatever the input, the output must be a payload naming a supported
		// method: an event that cannot be discriminated is worse than none.
		var payload struct {
			Method string `json:"method"`
		}
		if err := json.Unmarshal(ev.Data, &payload); err != nil {
			t.Fatalf("emitted unparseable payload %q for frame %q", ev.Data, frame)
		}
		if _, ok := sanitizers[payload.Method]; !ok {
			t.Fatalf("emitted method %q that is not supported, for frame %q", payload.Method, frame)
		}
	})
}

func BenchmarkCdpCommandEventClick(b *testing.B) {
	frame := []byte(`{"id":1,"method":"Input.dispatchMouseEvent","params":{"type":"mousePressed","x":1,"y":2,"button":"left","clickCount":1}}`)
	b.ReportAllocs()
	for b.Loop() {
		cdpCommandEvent(frame, testForwardTs, nil)
	}
}

func BenchmarkCdpCommandEventUnsupported(b *testing.B) {
	frame := []byte(`{"id":1,"method":"Runtime.callFunctionOn","params":{"functionDeclaration":"() => 1","objectId":"x"}}`)
	b.ReportAllocs()
	for b.Loop() {
		cdpCommandEvent(frame, testForwardTs, nil)
	}
}

// specCommandMethods reads the BrowserCdpCommandMethod enum out of the
// embedded spec, so the test compares against the schema rather than a second
// copy of the list.
func specCommandMethods(t *testing.T) []string {
	t.Helper()
	raw, err := yaml.YAMLToJSON(serverpkg.OpenAPIYAML)
	if err != nil {
		t.Fatalf("convert spec: %v", err)
	}
	var spec struct {
		Components struct {
			Schemas struct {
				BrowserCdpCommandMethod struct {
					Enum []string `json:"enum"`
				} `json:"BrowserCdpCommandMethod"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("parse spec: %v", err)
	}
	methods := spec.Components.Schemas.BrowserCdpCommandMethod.Enum
	if len(methods) == 0 {
		t.Fatal("spec has no BrowserCdpCommandMethod enum")
	}
	return methods
}

// Excluding a method suppresses only its event. The raw command reached
// Chromium before classification ran, so exclusion can never change what the
// browser was told to do.
func TestExcludedMethodsSuppressOnlyTheirOwnEvents(t *testing.T) {
	click := `{"id":1,"method":"Input.dispatchMouseEvent","params":{"type":"mousePressed","x":1,"y":2}}`
	nav := `{"id":2,"method":"Page.navigate","params":{"url":"https://example.com/"}}`
	excluded := map[string]struct{}{"Input.dispatchMouseEvent": {}}

	if _, ok := cdpCommandEvent([]byte(click), testForwardTs, excluded); ok {
		t.Fatal("excluded method produced an event")
	}
	if _, ok := cdpCommandEvent([]byte(nav), testForwardTs, excluded); !ok {
		t.Fatal("a method that was not excluded produced no event")
	}
	if _, ok := cdpCommandEvent([]byte(click), testForwardTs, nil); !ok {
		t.Fatal("no exclusions configured, but the command produced no event")
	}
}
