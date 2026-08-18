package devtoolsproxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/kernel/kernel-images/server/lib/events"
	oapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/kernel/kernel-images/server/lib/scaletozero"
)

// echoProxy stands up a proxy in front of an echoing upstream and returns a
// connected client. The upstream echoes every frame back, so a test that
// counted the upstream direction as commands would fail.
func echoProxy(t *testing.T, publish EventPublisher, controlEnabled ControlEnabledFunc) (*websocket.Conn, context.Context) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
		if err != nil {
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")
		c.SetReadLimit(100 * 1024 * 1024)
		for {
			mt, msg, err := c.Read(r.Context())
			if err != nil {
				return
			}
			if err := c.Write(r.Context(), mt, msg); err != nil {
				return
			}
		}
	}))
	t.Cleanup(upstream.Close)

	u, _ := url.Parse(upstream.URL)
	u.Scheme = "ws"
	u.Path = "/devtools/browser/x"

	logger := silentLogger()
	mgr := NewUpstreamManager("/dev/null", logger)
	mgr.setCurrent(u.String())

	proxy := httptest.NewServer(WebSocketProxyHandler(
		mgr, logger, false, scaletozero.NewNoopController(), publish, controlEnabled, nil, nil))
	t.Cleanup(proxy.Close)

	pu, _ := url.Parse(proxy.URL)
	pu.Scheme = "ws"

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	conn, _, err := websocket.Dial(ctx, pu.String(), nil)
	if err != nil {
		t.Fatalf("dial proxy failed: %v", err)
	}
	conn.SetReadLimit(100 * 1024 * 1024)
	t.Cleanup(func() { conn.Close(websocket.StatusNormalClosure, "") })
	return conn, ctx
}

// roundTrip writes each frame and reads the echo back, so the test only
// proceeds once the proxy has relayed in both directions.
func roundTrip(t *testing.T, conn *websocket.Conn, ctx context.Context, frames ...string) []string {
	t.Helper()
	echoes := make([]string, 0, len(frames))
	for i, frame := range frames {
		if err := conn.Write(ctx, websocket.MessageText, []byte(frame)); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		_, echo, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		echoes = append(echoes, string(echo))
	}
	return echoes
}

func commandEvents(evs []events.Event) []events.Event {
	out := make([]events.Event, 0, len(evs))
	for _, ev := range evs {
		if ev.Type == "cdp_command" {
			out = append(out, ev)
		}
	}
	return out
}

// waitForDisconnect blocks until the proxy has published cdp_disconnect, which
// happens after the observer has drained. Anything the upstream direction
// wrongly produced has been recorded by then.
func waitForDisconnect(t *testing.T, rp *recordingPublisher) events.Event {
	t.Helper()
	var found events.Event
	if !waitForCondition(10*time.Second, func() bool {
		for _, ev := range rp.snapshot() {
			if ev.Type == "cdp_disconnect" {
				found = ev
				return true
			}
		}
		return false
	}) {
		t.Fatal("proxy never published cdp_disconnect")
	}
	return found
}

func TestProxyEmitsOneEventPerClientControlCommand(t *testing.T) {
	rp := &recordingPublisher{}
	conn, ctx := echoProxy(t, rp.publish, controlOn)

	roundTrip(t, conn, ctx,
		`{"id":1,"method":"Runtime.callFunctionOn","params":{"functionDeclaration":"() => 1"}}`,
		`{"id":2,"method":"Input.dispatchMouseEvent","params":{"type":"mouseMoved","x":9,"y":9}}`,
		`{"id":3,"method":"Input.dispatchMouseEvent","params":{"type":"mousePressed","x":10,"y":20,"button":"left"}}`,
		`{"id":4,"method":"Input.dispatchMouseEvent","params":{"type":"mouseReleased","x":10,"y":20,"button":"left"}}`,
	)
	_ = conn.Close(websocket.StatusNormalClosure, "bye")

	disconnect := waitForDisconnect(t, rp)
	commands := commandEvents(rp.snapshot())
	if len(commands) != 3 {
		t.Fatalf("cdp_command count = %d, want 3 (the move and both click phases; nothing for Runtime.callFunctionOn or the echoes)", len(commands))
	}

	var data map[string]any
	if err := json.Unmarshal(disconnect.Data, &data); err != nil {
		t.Fatalf("unmarshal disconnect: %v", err)
	}
	if data["telemetry_dropped"] != 0.0 {
		t.Fatalf("telemetry_dropped = %v, want 0", data["telemetry_dropped"])
	}
	// Optional in the schema for compatibility, but this image always sets it,
	// so a reader can tell "nothing lost" from "not reported".
	if _, ok := data["telemetry_dropped"]; !ok {
		t.Fatal("cdp_disconnect omitted telemetry_dropped")
	}
}

func TestProxyEmitsNothingWhenControlIsDisabled(t *testing.T) {
	rp := &recordingPublisher{}
	conn, ctx := echoProxy(t, rp.publish, func() bool { return false })

	roundTrip(t, conn, ctx,
		`{"id":1,"method":"Input.dispatchMouseEvent","params":{"type":"mousePressed","x":1,"y":2}}`,
		`{"id":2,"method":"Page.navigate","params":{"url":"https://example.com/"}}`,
	)
	_ = conn.Close(websocket.StatusNormalClosure, "bye")

	waitForDisconnect(t, rp)
	if got := len(commandEvents(rp.snapshot())); got != 0 {
		t.Fatalf("cdp_command count = %d with control disabled, want 0", got)
	}
}

// The failure that motivated moving classification off the pump: a publisher
// that never returns used to stall the message transform, and with it the
// browser.
//
// Only cdp_command misbehaves here. cdp_connect and cdp_disconnect are
// published once per connection from the request goroutine, which chi's
// Recoverer covers and which forwards nothing; the pump is the path under
// test.
func TestBlockedPublisherDoesNotStallForwarding(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	blocking := func(ev events.Event) (events.Envelope, bool) {
		if ev.Type == "cdp_command" {
			<-release
		}
		return events.Envelope{Event: ev}, true
	}
	conn, ctx := echoProxy(t, blocking, controlOn)

	// Far more commands than the queue holds, so the publisher is wedged and
	// the queue is full well before the last one. Forwarding must not notice.
	frames := make([]string, 0, cdpObserverQueueDepth*2)
	for i := range cap(frames) {
		frames = append(frames, `{"id":`+strconv.Itoa(i)+`,"method":"Input.dispatchMouseEvent","params":{"type":"mousePressed","x":1,"y":2}}`)
	}
	echoes := roundTrip(t, conn, ctx, frames...)
	for i, echo := range echoes {
		if echo != frames[i] {
			t.Fatalf("frame %d came back changed:\n sent %s\n got  %s", i, frames[i], echo)
		}
	}
}

// A panicking publisher used to unwind through the pump goroutine, which no
// Recoverer covers, and take the process down with it. Forwarding must survive
// it, in both bytes and order.
func TestPanickingPublisherDoesNotBreakForwarding(t *testing.T) {
	panicking := func(ev events.Event) (events.Envelope, bool) {
		if ev.Type == "cdp_command" {
			panic("publisher exploded")
		}
		return events.Envelope{Event: ev}, true
	}
	conn, ctx := echoProxy(t, panicking, controlOn)

	frames := []string{
		`{"id":1,"method":"Input.dispatchMouseEvent","params":{"type":"mousePressed","x":1,"y":2}}`,
		`{"id":2,"method":"Page.navigate","params":{"url":"https://example.com/"}}`,
		`{"id":3,"method":"Input.insertText","params":{"text":"still forwarding"}}`,
	}
	echoes := roundTrip(t, conn, ctx, frames...)
	for i, echo := range echoes {
		if echo != frames[i] {
			t.Fatalf("frame %d came back changed:\n sent %s\n got  %s", i, frames[i], echo)
		}
	}
}

// Telemetry looks at frames; it must not change them. Whatever the client
// sends — malformed JSON, binary, invalid UTF-8, a large paste — the browser
// gets the same bytes in the same order.
func TestForwardingPreservesBytesAndOrderForAwkwardTraffic(t *testing.T) {
	rp := &recordingPublisher{}
	conn, ctx := echoProxy(t, rp.publish, controlOn)

	big := `{"id":9,"method":"Input.insertText","params":{"text":"` +
		strings.Repeat("x", 256<<10) + `"}}`
	frames := []string{
		`{"id":1,"method":"Input.dispatchMouseEvent","params":`,
		`{"id":2,"method":"Input.insertText","params":{"text":"\ud800"}}`,
		`{"id":3,"method":"Input.dispatchMouseEvent","params":{"type":"mousePressed","x":1,"y":2},"method":"Page.close"}`,
		`{"id":4,"method":"Input.dispatchMouseEvent","params":{"type":"mousePressed","x":1,"y":2}}`,
		big,
		`{"id":5,"method":"Page.reload"}`,
	}
	echoes := roundTrip(t, conn, ctx, frames...)
	for i, echo := range echoes {
		if echo != frames[i] {
			t.Fatalf("frame %d came back changed (len sent %d, len got %d)", i, len(frames[i]), len(echo))
		}
	}

	_ = conn.Close(websocket.StatusNormalClosure, "bye")
	disconnect := waitForDisconnect(t, rp)

	// Awkward traffic is classified or discarded, never counted as loss.
	var data map[string]any
	if err := json.Unmarshal(disconnect.Data, &data); err != nil {
		t.Fatalf("unmarshal disconnect: %v", err)
	}
	if data["telemetry_dropped"] != 0.0 {
		t.Fatalf("telemetry_dropped = %v, want 0", data["telemetry_dropped"])
	}
}

// Binary frames are not CDP commands, so they are relayed and ignored.
func TestBinaryFramesAreForwardedAndNotClassified(t *testing.T) {
	rp := &recordingPublisher{}
	conn, ctx := echoProxy(t, rp.publish, controlOn)

	payload := []byte{0x00, 0xff, 0xfe, 0x7b, 0x22}
	if err := conn.Write(ctx, websocket.MessageBinary, payload); err != nil {
		t.Fatalf("write binary: %v", err)
	}
	mt, echo, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read binary: %v", err)
	}
	if mt != websocket.MessageBinary || string(echo) != string(payload) {
		t.Fatalf("binary frame came back as %v %q", mt, echo)
	}

	_ = conn.Close(websocket.StatusNormalClosure, "bye")
	waitForDisconnect(t, rp)
	if got := len(commandEvents(rp.snapshot())); got != 0 {
		t.Fatalf("cdp_command count = %d for binary traffic, want 0", got)
	}
}

// telemetry_dropped was added to cdp_disconnect after the event type shipped,
// so it stays optional: a payload from an image that predates it must still
// decode, and must be distinguishable from one reporting zero.
func TestDisconnectPayloadFromAnOlderImageStillDecodes(t *testing.T) {
	old := `{"duration_ms":12.5,"message_count":3,"reason":"client_close"}`
	var data oapi.BrowserCdpDisconnectEventData
	if err := json.Unmarshal([]byte(old), &data); err != nil {
		t.Fatalf("payload without telemetry_dropped failed to decode: %v", err)
	}
	if data.TelemetryDropped != nil {
		t.Fatalf("telemetry_dropped = %v, want absent", *data.TelemetryDropped)
	}
	if data.MessageCount != 3 || data.Reason != oapi.ClientClose {
		t.Fatalf("decoded the rest wrong: %+v", data)
	}
}
