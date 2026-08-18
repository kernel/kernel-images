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
	oapi "github.com/kernel/kernel-images/server/lib/oapi"
)

// bs is a single backslash, kept out of the fixtures below so an editing
// pass cannot silently strip the escape they are testing.
const bs = `\`

// testForwardTs stands in for the time a command reached Chromium.
const testForwardTs int64 = 1_700_000_000_000_000

// payloadOf classifies a frame and returns its event payload as a plain map.
// Asserting on the wire shape rather than the generated union type is the
// point: the payload is what a reader sees.
func payloadOf(t *testing.T, frame string) map[string]any {
	t.Helper()
	ev, ok := classifyFrame(t, frame, nil)
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
				"command_id": 1.0, "method": "Input.dispatchMouseEvent", "session_id": "S1", "event_type": "mousePressed",
				"x": 10.5, "y": 20.0, "button": "left", "click_count": 2.0,
				"modifiers": 8.0, "buttons": 1.0, "pointer_type": "mouse",
			},
		},
		{
			name:  "mouseMoved with buttons held is a drag path, not a duplicate phase",
			frame: `{"id":2,"method":"Input.dispatchMouseEvent","params":{"type":"mouseMoved","x":9,"y":9,"buttons":1}}`,
			want: map[string]any{
				"command_id": 2.0, "method": "Input.dispatchMouseEvent", "event_type": "mouseMoved",
				"x": 9.0, "y": 9.0, "buttons": 1.0,
			},
		},
		{
			name:  "wheel keeps its deltas",
			frame: `{"id":3,"method":"Input.dispatchMouseEvent","params":{"type":"mouseWheel","x":1,"y":2,"deltaX":0,"deltaY":-400}}`,
			want: map[string]any{
				"command_id": 3.0, "method": "Input.dispatchMouseEvent", "event_type": "mouseWheel",
				"x": 1.0, "y": 2.0, "delta_x": 0.0, "delta_y": -400.0,
			},
		},
		{
			name:  "keyUp releases a held modifier",
			frame: `{"id":4,"method":"Input.dispatchKeyEvent","params":{"type":"keyUp","key":"Shift"}}`,
			want:  map[string]any{"command_id": 4.0, "method": "Input.dispatchKeyEvent", "event_type": "keyUp", "named_key": "Shift"},
		},
		{
			name:  "char is the command that inserts the character",
			frame: `{"id":5,"method":"Input.dispatchKeyEvent","params":{"type":"char","text":"a"}}`,
			want:  map[string]any{"command_id": 5.0, "method": "Input.dispatchKeyEvent", "event_type": "char", "text_length": 1.0},
		},
		{
			name:  "a typed key is counted, never named",
			frame: `{"id":6,"method":"Input.dispatchKeyEvent","params":{"type":"keyDown","key":"é","text":"é","code":"KeyE"}}`,
			want:  map[string]any{"command_id": 6.0, "method": "Input.dispatchKeyEvent", "event_type": "keyDown", "text_length": 1.0},
		},
		{
			name:  "scroll gesture keeps its distance",
			frame: `{"id":7,"method":"Input.synthesizeScrollGesture","params":{"x":1,"y":2,"xDistance":0,"yDistance":-500,"speed":800}}`,
			want: map[string]any{
				"command_id": 7.0, "method": "Input.synthesizeScrollGesture", "x": 1.0, "y": 2.0,
				"x_distance": 0.0, "y_distance": -500.0, "speed": 800.0,
			},
		},
		{
			name:  "touch reports its point count and the primary point",
			frame: `{"id":8,"method":"Input.dispatchTouchEvent","params":{"type":"touchStart","touchPoints":[{"x":100,"y":200},{"x":300,"y":400}]}}`,
			want: map[string]any{
				"command_id": 8.0, "method": "Input.dispatchTouchEvent", "event_type": "touchStart",
				"touch_point_count": 2.0, "x": 100.0, "y": 200.0,
			},
		},
		{
			name:  "drag reports counts and mime categories, not contents",
			frame: `{"id":9,"method":"Input.dispatchDragEvent","params":{"type":"drop","x":5,"y":6,"data":{"items":[{"mimeType":"text/plain","data":"secret"},{"mimeType":"image/png","data":"secret"}],"files":["/tmp/a.pdf"],"dragOperationsMask":1}}}`,
			want: map[string]any{
				"command_id": 9.0, "method": "Input.dispatchDragEvent", "event_type": "drop", "x": 5.0, "y": 6.0,
				"drag_item_count": 2.0, "drag_file_count": 1.0,
				"drag_mime_categories": []any{"image", "text"}, "drag_operations_mask": 1.0,
			},
		},
		{
			name:  "navigation reports the scheme, never the host or the path",
			frame: `{"id":10,"method":"Page.navigate","params":{"url":"https://example.com/reset?token=abc","referrer":"https://mail.example.com/x","transitionType":"typed"}}`,
			want: map[string]any{
				"command_id": 10.0, "method": "Page.navigate", "url_scheme": "https",
				"transition_type": "typed", "referrer_present": true,
			},
		},
		{
			name:  "dialog reports the decision",
			frame: `{"id":11,"method":"Page.handleJavaScriptDialog","params":{"accept":true,"promptText":"hunter2"}}`,
			want:  map[string]any{"command_id": 11.0, "method": "Page.handleJavaScriptDialog", "accept": true, "prompt_text_length": 7.0},
		},
		{
			name:  "file selection reports the count, never the paths",
			frame: `{"id":12,"method":"DOM.setFileInputFiles","params":{"files":["/tmp/a.pdf","/tmp/b.pdf"],"backendNodeId":7}}`,
			want:  map[string]any{"command_id": 12.0, "method": "DOM.setFileInputFiles", "file_count": 2.0, "backend_node_id": 7.0},
		},
		{
			name:  "screenshot reports its options and clip",
			frame: `{"id":13,"method":"Page.captureScreenshot","params":{"format":"png","quality":80,"clip":{"x":0,"y":0,"width":800,"height":600,"scale":1}}}`,
			want: map[string]any{
				"command_id": 13.0, "method": "Page.captureScreenshot", "format": "png", "quality": 80.0,
				"clip_x": 0.0, "clip_y": 0.0, "clip_width": 800.0, "clip_height": 600.0, "clip_scale": 1.0,
			},
		},
		{
			name:  "autofill reports which kind of value was filled",
			frame: `{"id":14,"method":"Autofill.trigger","params":{"fieldId":3,"card":{"number":"4111111111111111","cvc":"123"}}}`,
			want:  map[string]any{"command_id": 14.0, "method": "Autofill.trigger", "field_id": 3.0, "mode": "card"},
		},
		{
			name:  "a command with no arguments reports its name",
			frame: `{"id":15,"method":"Page.bringToFront"}`,
			want:  map[string]any{"command_id": 15.0, "method": "Page.bringToFront"},
		},
		{
			name:  "window bounds are flattened out of the bounds object",
			frame: `{"id":16,"method":"Browser.setWindowBounds","params":{"windowId":1,"bounds":{"left":0,"top":0,"width":1280,"height":720,"windowState":"normal"}}}`,
			want: map[string]any{
				"command_id": 16.0, "method": "Browser.setWindowBounds", "window_id": 1.0, "left": 0.0, "top": 0.0,
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
				if ev, ok := classifyFrame(t, tc.frame, nil); ok {
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
	// The "d" arrives as a unicode escape. A byte scan for the literal method
	// name misses this; a decode does not.
	escaped := `{"id":1,"method":"Input.\u0064ispatchMouseEvent","params":{"type":"mousePressed","x":1,"y":2}}`
	if !strings.Contains(escaped, bs+`u0064`) {
		t.Fatal("the fixture lost its escape, so this test proves nothing")
	}
	got := payloadOf(t, escaped)
	if got["method"] != "Input.dispatchMouseEvent" {
		t.Fatalf("method = %v, want Input.dispatchMouseEvent", got["method"])
	}
	if got["event_type"] != "mousePressed" {
		t.Fatalf("event_type = %v, want mousePressed", got["event_type"])
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
	"url":"https://SENTINELhost.example/SENTINELpath?token=SENTINELquery#SENTINELfragment",
	"referrer":"https://SENTINELhost.example/SENTINELreferrer",
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
	"data":{"items":[{"mimeType":"text/SENTINELsubtype","data":"SENTINELdrag","baseURL":"https://SENTINELhost.example/SENTINELbase","title":"SENTINELtitle"}],"files":["/tmp/SENTINELdragfile"]},
	"type":"SENTINELtype",
	"button":"SENTINELbutton",
	"pointerType":"SENTINELpointer",
	"format":"SENTINELformat",
	"state":"SENTINELstate",
	"transferMode":"SENTINELtransfer",
	"gestureSourceType":"SENTINELgesture",
	"transitionType":"SENTINELtransition",
	"referrerPolicy":"SENTINELpolicy",
	"windowState":"SENTINELwindowstate",
	"bounds":{"windowState":"SENTINELboundsstate"},
	"clip":{},
	"touchPoints":[{"x":1,"y":2}]
}`

func TestSanitizersNeverEmitSensitiveValues(t *testing.T) {
	for method := range sanitizers {
		t.Run(method, func(t *testing.T) {
			frame := fmt.Sprintf(`{"id":1,"sessionId":"S","method":%q,"params":%s}`, method, sensitiveParams)
			ev, ok := classifyFrame(t, frame, nil)
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

// The schema has to describe what the sanitizers emit, so a method reported
// with no variant to decode it, or a variant nothing emits, fails here.
// Detecting a canonical method or argument that nobody handled is a different
// question, and neither side of this comparison can answer it — that is what
// the pinned-protocol checks in cdpmanifest_test.go are for.
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
		ev, ok := classifyFrameOrSkip(frame)
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
		classifyFrameOrSkip(string(frame))
	}
}

func BenchmarkCdpCommandEventUnsupported(b *testing.B) {
	frame := []byte(`{"id":1,"method":"Runtime.callFunctionOn","params":{"functionDeclaration":"() => 1","objectId":"x"}}`)
	b.ReportAllocs()
	for b.Loop() {
		classifyFrameOrSkip(string(frame))
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

	if _, ok := classifyFrame(t, click, excluded); ok {
		t.Fatal("excluded method produced an event")
	}
	if _, ok := classifyFrame(t, nav, excluded); !ok {
		t.Fatal("a method that was not excluded produced no event")
	}
	if _, ok := classifyFrame(t, click, nil); !ok {
		t.Fatal("no exclusions configured, but the command produced no event")
	}
}

// The scheme is the only part of a URL the control category carries. A reader
// who needs the destination opts into the page category, where navigation
// events report the URL itself.
func TestNavigationReportsSchemeOnly(t *testing.T) {
	for _, tc := range []struct {
		frame string
		want  string
	}{
		{`{"id":1,"method":"Page.navigate","params":{"url":"https://internal.acme.example/admin?token=abc"}}`, "https"},
		{`{"id":2,"method":"Page.navigate","params":{"url":"data:text/html,<h1>hi</h1>"}}`, "data"},
		{`{"id":3,"method":"Target.createTarget","params":{"url":"about:blank"}}`, "about"},
	} {
		got := payloadOf(t, tc.frame)
		if got["url_scheme"] != tc.want {
			t.Fatalf("url_scheme = %v, want %v", got["url_scheme"], tc.want)
		}
		if _, ok := got["url_host"]; ok {
			t.Fatalf("payload carried a url_host: %v", got)
		}
		for key, value := range got {
			if str, isStr := value.(string); isStr && strings.Contains(str, "acme") {
				t.Fatalf("payload leaked the host in %s: %v", key, value)
			}
		}
	}
}

// A relative or unparseable URL has no scheme, and the event says so rather
// than inventing one.
func TestNavigationOmitsSchemeWhenThereIsNone(t *testing.T) {
	got := payloadOf(t, `{"id":1,"method":"Page.navigate","params":{"url":"/relative/path"}}`)
	if _, ok := got["url_scheme"]; ok {
		t.Fatalf("relative URL reported a scheme: %v", got)
	}
}

// The failure this guards: a client-controlled string was copied into the
// payload verbatim, so a 1.1 MB button produced a 1.1 MB event, and
// truncateIfNeeded nulls the whole event rather than clipping the field. Every
// such value is now either a protocol enum or a clipped identifier, so the
// payload is bounded whatever the client sends.
func TestPayloadStaysBoundedWhateverTheClientSends(t *testing.T) {
	huge := strings.Repeat("z", 1_100_000)
	for _, frame := range []string{
		`{"id":1,"method":"Input.dispatchMouseEvent","params":{"type":"mousePressed","button":"` + huge + `"}}`,
		`{"id":1,"method":"Input.dispatchMouseEvent","params":{"type":"` + huge + `"}}`,
		`{"id":1,"sessionId":"` + huge + `","method":"Page.bringToFront"}`,
		`{"id":1,"method":"Target.activateTarget","params":{"targetId":"` + huge + `"}}`,
		`{"id":1,"method":"Page.navigate","params":{"url":"https://h.example/","transitionType":"` + huge + `"}}`,
		`{"id":1,"method":"Page.captureScreenshot","params":{"format":"` + huge + `"}}`,
		`{"id":1,"method":"Browser.setWindowBounds","params":{"windowId":1,"bounds":{"windowState":"` + huge + `"}}}`,
	} {
		ev, ok := classifyFrame(t, frame, nil)
		if !ok {
			t.Fatalf("no event for %.60s", frame)
		}
		// Comfortably inside the 1 MB envelope limit, so the event survives whole.
		if len(ev.Data) > 4096 {
			t.Fatalf("payload is %d bytes for a frame with one huge value: %.200s", len(ev.Data), ev.Data)
		}
		// An enum is replaced outright; an identifier is clipped. Either way no
		// single value carries more than the identifier bound.
		var fields map[string]any
		if err := json.Unmarshal(ev.Data, &fields); err != nil {
			t.Fatal(err)
		}
		for name, value := range fields {
			str, isStr := value.(string)
			if isStr && len(str) > maxOpaqueIDBytes {
				t.Fatalf("%s carries %d bytes of client value", name, len(str))
			}
		}
	}
}

// A value the protocol does not define is reported, but as `other` rather than
// whatever the client chose to send.
func TestUnknownEnumValuesReportAsOther(t *testing.T) {
	got := payloadOf(t, `{"id":1,"method":"Input.dispatchMouseEvent","params":{"type":"teleported","button":"elbow","pointerType":"nose"}}`)
	for field, want := range map[string]string{"event_type": "other", "button": "other", "pointer_type": "other"} {
		if got[field] != want {
			t.Fatalf("%s = %v, want %v", field, got[field], want)
		}
	}
}

// An identifier past the bound is clipped, not dropped: the field is still
// there and still partially readable.
func TestOpaqueIdentifiersAreClipped(t *testing.T) {
	got := payloadOf(t, `{"id":7,"sessionId":"`+strings.Repeat("S", 500)+`","method":"Page.reload"}`)
	sid, _ := got["session_id"].(string)
	if len(sid) != maxOpaqueIDBytes {
		t.Fatalf("session_id length = %d, want %d", len(sid), maxOpaqueIDBytes)
	}
}

// The JSON-RPC id and the connection id are what let a reader join a command to
// the result the browser returned, and attribute it to one of several clients.
func TestCommandAndConnectionIdsAreReported(t *testing.T) {
	ev, ok := classifyFrameConn(t, `{"id":42,"method":"Page.reload"}`, "conn-abc")
	if !ok {
		t.Fatal("no event")
	}
	var got map[string]any
	if err := json.Unmarshal(ev.Data, &got); err != nil {
		t.Fatal(err)
	}
	if got["command_id"] != 42.0 {
		t.Fatalf("command_id = %v, want 42", got["command_id"])
	}
	if got["connection_id"] != "conn-abc" {
		t.Fatalf("connection_id = %v, want conn-abc", got["connection_id"])
	}
	// A notification carries no id, and the event says so rather than inventing one.
	ev2, _ := classifyFrameConn(t, `{"method":"Page.reload"}`, "conn-abc")
	var got2 map[string]any
	json.Unmarshal(ev2.Data, &got2)
	if _, ok := got2["command_id"]; ok {
		t.Fatalf("command_id present for a frame with no id: %v", got2)
	}
}

// classifyFrame runs a frame through the two steps production uses: resolve the
// method at admission, then build the event. A frame whose method is not
// supported or is excluded never reaches the second step, exactly as it does
// not reach the queue.
func classifyFrame(t *testing.T, frame string, excluded map[string]struct{}) (events.Event, bool) {
	t.Helper()
	return classifyFrameWith(frame, "", excluded)
}

func classifyFrameConn(t *testing.T, frame, connectionID string) (events.Event, bool) {
	t.Helper()
	return classifyFrameWith(frame, connectionID, nil)
}

// classifyFrameOrSkip is classifyFrame for callers with no *testing.T to hand,
// such as the fuzz target.
func classifyFrameOrSkip(frame string) (events.Event, bool) {
	return classifyFrameWith(frame, "", nil)
}

func classifyFrameWith(frame, connectionID string, excluded map[string]struct{}) (events.Event, bool) {
	method, supported := supportedMethod([]byte(frame))
	if !supported {
		return events.Event{}, false
	}
	if _, skip := excluded[method]; skip {
		return events.Event{}, false
	}
	return cdpCommandEvent([]byte(frame), testForwardTs, connectionID, method)
}

// specMaxLengthFields returns, per command method, the payload fields the
// schema bounds. Reading it from the spec rather than listing them here is the
// point: a field that gains a maxLength is covered without anyone remembering
// to extend this test.
func specMaxLengthFields(t *testing.T) map[string]map[string]int {
	t.Helper()
	raw, err := yaml.YAMLToJSON(serverpkg.OpenAPIYAML)
	if err != nil {
		t.Fatalf("convert spec: %v", err)
	}
	var spec struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]struct {
					Const     string `json:"const"`
					MaxLength *int   `json:"maxLength"`
				} `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("parse spec: %v", err)
	}
	out := map[string]map[string]int{}
	for name, schema := range spec.Components.Schemas {
		if !strings.HasSuffix(name, "CommandData") {
			continue
		}
		method := schema.Properties["method"].Const
		if method == "" {
			continue
		}
		for field, prop := range schema.Properties {
			if prop.MaxLength == nil {
				continue
			}
			if out[method] == nil {
				out[method] = map[string]int{}
			}
			out[method][field] = *prop.MaxLength
		}
	}
	if len(out) == 0 {
		t.Fatal("spec declares no bounded fields, so this test proves nothing")
	}
	return out
}

// paramForField names a CDP argument that lands in a given payload field, so
// the bound can be exercised through a real frame.
var paramForField = map[string]string{
	"session_id":         "", // envelope, not params
	"connection_id":      "", // supplied by the proxy
	"object_id":          "objectId",
	"frame_id":           "frameId",
	"target_id":          "targetId",
	"browser_context_id": "browserContextId",
	"loader_id":          "loaderId",
	"download_guid":      "guid",
	"panel_id":           "panelId",
	"url_scheme":         "url", // derived via urlScheme(); test value is scheme://x
}

// Every field the schema bounds has to be bounded in the payload too, on every
// command that carries it. Five of these were emitted raw against a
// maxLength: 128 schema, which put the whole-event nulling back in reach.
func TestBoundedFieldsAreClippedOnEveryCommand(t *testing.T) {
	huge := strings.Repeat("Q", 512)
	for method, fields := range specMaxLengthFields(t) {
		for field, max := range fields {
			t.Run(method+"/"+field, func(t *testing.T) {
				param, known := paramForField[field]
				if !known {
					t.Fatalf("no CDP argument mapped for bounded field %q; add it to paramForField", field)
				}
				var frame string
				switch field {
				case "session_id":
					frame = fmt.Sprintf(`{"id":1,"sessionId":%q,"method":%q,"params":{}}`, huge, method)
				case "connection_id":
					t.Skip("supplied by the proxy, covered by TestConnectionIdIsClipped")
				case "url_scheme":
					frame = fmt.Sprintf(`{"id":1,"method":%q,"params":{%q:%q}}`, method, param, huge+"://x")
				default:
					frame = fmt.Sprintf(`{"id":1,"method":%q,"params":{%q:%q}}`, method, param, huge)
				}
				got := payloadOf(t, frame)
				v, present := got[field].(string)
				if !present {
					t.Fatalf("%s did not report %s at all", method, field)
				}
				if len(v) > max {
					t.Fatalf("%s.%s is %d bytes, over the schema's maxLength %d", method, field, len(v), max)
				}
			})
		}
	}
}

// The connection id is ours rather than the client's, but it is bounded by the
// same schema and reaches every command.
func TestConnectionIdIsClipped(t *testing.T) {
	ev, ok := classifyFrameConn(t, `{"id":1,"method":"Page.reload"}`, strings.Repeat("C", 512))
	if !ok {
		t.Fatal("no event")
	}
	var got map[string]any
	if err := json.Unmarshal(ev.Data, &got); err != nil {
		t.Fatal(err)
	}
	if v, _ := got["connection_id"].(string); len(v) != maxOpaqueIDBytes {
		t.Fatalf("connection_id is %d bytes, want %d", len(v), maxOpaqueIDBytes)
	}
}

// specPayloadEnums returns the value set of every enum a cdp_command payload
// field can carry. Membership comes from what the CommandData schemas actually
// reference, not from a naming convention, so the config-only method enum is
// not swept in and a new payload enum is covered without extending this test.
func specPayloadEnums(t *testing.T) map[string]map[string]bool {
	t.Helper()
	raw, err := yaml.YAMLToJSON(serverpkg.OpenAPIYAML)
	if err != nil {
		t.Fatalf("convert spec: %v", err)
	}
	var spec struct {
		Components struct {
			Schemas map[string]struct {
				Enum       []string `json:"enum"`
				Properties map[string]struct {
					Ref   string `json:"$ref"`
					Items *struct {
						Ref string `json:"$ref"`
					} `json:"items"`
				} `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("parse spec: %v", err)
	}

	referenced := map[string]bool{}
	note := func(ref string) {
		if name := strings.TrimPrefix(ref, "#/components/schemas/"); name != ref && name != "" {
			referenced[name] = true
		}
	}
	for name, schema := range spec.Components.Schemas {
		if !strings.HasSuffix(name, "CommandData") {
			continue
		}
		for _, prop := range schema.Properties {
			note(prop.Ref)
			if prop.Items != nil {
				note(prop.Items.Ref)
			}
		}
	}

	out := map[string]map[string]bool{}
	for name := range referenced {
		schema := spec.Components.Schemas[name]
		if len(schema.Enum) == 0 {
			continue
		}
		members := map[string]bool{}
		for _, v := range schema.Enum {
			members[v] = true
		}
		out[name] = members
	}
	return out
}

// Every enum a payload can carry must have a fallback the schema accepts. The
// drag MIME category emitted "other" while its enum did not list it, so a
// client sending an unusual MIME type produced a payload the spec rejects.
func TestEveryPayloadEnumHasItsFallbackInTheSchema(t *testing.T) {
	members := specPayloadEnums(t)
	if len(members) == 0 {
		t.Fatal("spec declares no payload enums, so this test proves nothing")
	}
	// Autofill mode is the one enum the proxy derives rather than copying, so
	// it has no unknown case to fall back on.
	derived := map[string]bool{"BrowserCdpAutofillMode": true}
	for name, values := range members {
		if derived[name] {
			continue
		}
		if !values[unknownEnumValue] {
			t.Errorf("%s cannot represent an unrecognised value: %q is not in its enum", name, unknownEnumValue)
		}
	}
}

// An unusual value on any enum-bearing argument must still produce a payload
// whose every enum passes the generated Valid().
func TestUnrecognisedEnumValuesStaySchemaValid(t *testing.T) {
	for _, frame := range []string{
		`{"id":1,"method":"Input.dispatchDragEvent","params":{"type":"drop","data":{"items":[{"mimeType":"weirdtype/x-thing"},{"mimeType":"no-slash"}]}}}`,
		`{"id":1,"method":"Input.dispatchMouseEvent","params":{"type":"teleported","button":"elbow","pointerType":"nose"}}`,
		`{"id":1,"method":"Page.captureScreenshot","params":{"format":"holograph"}}`,
		`{"id":1,"method":"Page.navigate","params":{"url":"https://h.example/","transitionType":"osmosis","referrerPolicy":"whatever"}}`,
		`{"id":1,"method":"Browser.setWindowBounds","params":{"windowId":1,"bounds":{"windowState":"sideways"}}}`,
		`{"id":1,"method":"Page.setWebLifecycleState","params":{"state":"marinating"}}`,
		// Missing type field: enumOf must produce the fallback, not "".
		`{"id":1,"method":"Input.dispatchMouseEvent","params":{}}`,
		`{"id":1,"method":"Input.dispatchKeyEvent","params":{}}`,
		`{"id":1,"method":"Input.dispatchTouchEvent","params":{"touchPoints":[]}}`,
		`{"id":1,"method":"Input.dispatchDragEvent","params":{}}`,
	} {
		ev, ok := classifyFrame(t, frame, nil)
		if !ok {
			t.Fatalf("no event for %.70s", frame)
		}
		assertPayloadEnumsValid(t, ev.Data)
	}
}

// assertPayloadEnumsValid round-trips the payload through the generated union,
// whose enum types vet their own values.
func assertPayloadEnumsValid(t *testing.T, payload []byte) {
	t.Helper()
	var data oapi.BrowserCdpCommandEventData
	if err := json.Unmarshal(payload, &data); err != nil {
		t.Fatalf("payload does not decode into the union: %v", err)
	}
	var probe struct {
		Method string `json:"method"`
	}
	json.Unmarshal(payload, &probe)

	switch probe.Method {
	case "Input.dispatchDragEvent":
		v, err := data.AsBrowserCdpInputDispatchDragEventCommandData()
		if err != nil {
			t.Fatalf("decode drag payload: %v", err)
		}
		if !v.EventType.Valid() {
			t.Errorf("event_type %q fails Valid()", v.EventType)
		}
		if v.DragMimeCategories != nil {
			for _, c := range *v.DragMimeCategories {
				if !c.Valid() {
					t.Errorf("drag_mime_categories %q fails Valid(): payload violates its own schema", c)
				}
			}
		}
	case "Input.dispatchMouseEvent":
		v, _ := data.AsBrowserCdpInputDispatchMouseEventCommandData()
		if !v.EventType.Valid() {
			t.Errorf("event_type %q fails Valid()", v.EventType)
		}
		if v.Button != nil && !v.Button.Valid() {
			t.Errorf("button %q fails Valid()", *v.Button)
		}
		if v.PointerType != nil && !v.PointerType.Valid() {
			t.Errorf("pointer_type %q fails Valid()", *v.PointerType)
		}
	case "Page.captureScreenshot":
		v, _ := data.AsBrowserCdpPageCaptureScreenshotCommandData()
		if v.Format != nil && !v.Format.Valid() {
			t.Errorf("format %q fails Valid()", *v.Format)
		}
	case "Page.navigate":
		v, _ := data.AsBrowserCdpPageNavigateCommandData()
		if v.TransitionType != nil && !v.TransitionType.Valid() {
			t.Errorf("transition_type %q fails Valid()", *v.TransitionType)
		}
		if v.ReferrerPolicy != nil && !v.ReferrerPolicy.Valid() {
			t.Errorf("referrer_policy %q fails Valid()", *v.ReferrerPolicy)
		}
	case "Browser.setWindowBounds":
		v, _ := data.AsBrowserCdpBrowserSetWindowBoundsCommandData()
		if v.WindowState != nil && !v.WindowState.Valid() {
			t.Errorf("window_state %q fails Valid()", *v.WindowState)
		}
	case "Input.dispatchKeyEvent":
		v, _ := data.AsBrowserCdpInputDispatchKeyEventCommandData()
		if !v.EventType.Valid() {
			t.Errorf("event_type %q fails Valid()", v.EventType)
		}
	case "Input.dispatchTouchEvent":
		v, _ := data.AsBrowserCdpInputDispatchTouchEventCommandData()
		if !v.EventType.Valid() {
			t.Errorf("event_type %q fails Valid()", v.EventType)
		}
	case "Page.setWebLifecycleState":
		v, _ := data.AsBrowserCdpPageSetWebLifecycleStateCommandData()
		if !v.State.Valid() {
			t.Errorf("state %q fails Valid()", v.State)
		}
	default:
		t.Fatalf("no enum assertions for %s", probe.Method)
	}
}
