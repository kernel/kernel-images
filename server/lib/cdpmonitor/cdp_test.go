package cdpmonitor

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/kernel/kernel-images/server/lib/events"
	"github.com/stretchr/testify/require"
)

var discardLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

// minimalPNG is a valid 1x1 PNG used as a test fixture for screenshot tests.
var minimalPNG = func() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{R: 255, G: 0, B: 0, A: 255})
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}()

// testServer is a minimal WebSocket server that accepts connections and
// lets the test drive scripted message sequences.
type testServer struct {
	srv    *httptest.Server
	conn   *websocket.Conn
	connMu sync.Mutex
	connCh chan struct{} // closed when the first connection is accepted
	msgCh  chan []byte   // inbound messages from Monitor
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	s := &testServer{
		msgCh:  make(chan []byte, 128),
		connCh: make(chan struct{}),
	}
	var connOnce sync.Once
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		s.connMu.Lock()
		s.conn = c
		s.connMu.Unlock()
		connOnce.Do(func() { close(s.connCh) })
		go func() {
			for {
				_, b, err := c.Read(context.Background())
				if err != nil {
					return
				}
				s.msgCh <- b
			}
		}()
	}))
	return s
}

func (s *testServer) wsURL() string {
	return "ws" + strings.TrimPrefix(s.srv.URL, "http")
}

func (s *testServer) sendToMonitor(t *testing.T, msg any) {
	t.Helper()
	s.connMu.Lock()
	c := s.conn
	s.connMu.Unlock()
	require.NotNil(t, c, "no active connection")
	require.NoError(t, wsjson.Write(context.Background(), c, msg))
}

func (s *testServer) readFromMonitor(t *testing.T, timeout time.Duration) cdpMessage {
	t.Helper()
	select {
	case b := <-s.msgCh:
		var msg cdpMessage
		require.NoError(t, json.Unmarshal(b, &msg))
		return msg
	case <-time.After(timeout):
		t.Fatal("timeout waiting for message from Monitor")
		return cdpMessage{}
	}
}

func (s *testServer) close() {
	s.connMu.Lock()
	if s.conn != nil {
		_ = s.conn.Close(websocket.StatusNormalClosure, "done")
	}
	s.connMu.Unlock()
	s.srv.Close()
}

// testUpstream implements UpstreamProvider for tests.
type testUpstream struct {
	mu      sync.Mutex
	current string
	subs    []chan string
}

func newTestUpstream(url string) *testUpstream {
	return &testUpstream{current: url}
}

func (u *testUpstream) Current() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.current
}

func (u *testUpstream) Subscribe() (<-chan string, func()) {
	ch := make(chan string, 1)
	u.mu.Lock()
	u.subs = append(u.subs, ch)
	u.mu.Unlock()
	cancel := func() {
		u.mu.Lock()
		for i, s := range u.subs {
			if s == ch {
				u.subs = append(u.subs[:i], u.subs[i+1:]...)
				break
			}
		}
		u.mu.Unlock()
		close(ch)
	}
	return ch, cancel
}

func (u *testUpstream) notifyRestart(newURL string) {
	u.mu.Lock()
	u.current = newURL
	subs := make([]chan string, len(u.subs))
	copy(subs, u.subs)
	u.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- newURL:
		default:
		}
	}
}

// eventCollector captures published events with channel-based notification.
type eventCollector struct {
	mu     sync.Mutex
	events []events.Event
	notify chan struct{} // signaled on every publish
}

func newEventCollector() *eventCollector {
	return &eventCollector{notify: make(chan struct{}, 256)}
}

func (c *eventCollector) publishFn() PublishFunc {
	return func(ev events.Event) {
		c.mu.Lock()
		c.events = append(c.events, ev)
		c.mu.Unlock()
		select {
		case c.notify <- struct{}{}:
		default:
		}
	}
}

// waitFor blocks until an event of the given type is published, or fails.
func (c *eventCollector) waitFor(t *testing.T, eventType string, timeout time.Duration) events.Event {
	t.Helper()
	deadline := time.After(timeout)
	for {
		c.mu.Lock()
		for _, ev := range c.events {
			if ev.Type == eventType {
				c.mu.Unlock()
				return ev
			}
		}
		c.mu.Unlock()
		select {
		case <-c.notify:
		case <-deadline:
			t.Fatalf("timeout waiting for event type=%q", eventType)
			return events.Event{}
		}
	}
}

// waitForNew blocks until a NEW event of the given type is published after this
// call, ignoring any events already in the collector.
func (c *eventCollector) waitForNew(t *testing.T, eventType string, timeout time.Duration) events.Event {
	t.Helper()
	c.mu.Lock()
	skip := len(c.events)
	c.mu.Unlock()

	deadline := time.After(timeout)
	for {
		c.mu.Lock()
		for i := skip; i < len(c.events); i++ {
			if c.events[i].Type == eventType {
				ev := c.events[i]
				c.mu.Unlock()
				return ev
			}
		}
		c.mu.Unlock()
		select {
		case <-c.notify:
		case <-deadline:
			t.Fatalf("timeout waiting for new event type=%q", eventType)
			return events.Event{}
		}
	}
}

// assertNone verifies that no event of the given type arrives within d.
func (c *eventCollector) assertNone(t *testing.T, eventType string, d time.Duration) {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case <-c.notify:
			c.mu.Lock()
			for _, ev := range c.events {
				if ev.Type == eventType {
					c.mu.Unlock()
					t.Fatalf("unexpected event %q published", eventType)
					return
				}
			}
			c.mu.Unlock()
		case <-deadline:
			return
		}
	}
}

// ResponderFunc is called for each CDP command the Monitor sends.
// Return nil to use the default empty result.
type ResponderFunc func(msg cdpMessage) any

// listenAndRespond drains srv.msgCh, calls fn for each command, and sends the
// response. If fn is nil or returns nil, sends {"id": msg.ID, "result": {}}.
func listenAndRespond(srv *testServer, stopCh <-chan struct{}, fn ResponderFunc) {
	for {
		select {
		case b := <-srv.msgCh:
			var msg cdpMessage
			if json.Unmarshal(b, &msg) != nil || msg.ID == nil {
				continue
			}
			srv.connMu.Lock()
			c := srv.conn
			srv.connMu.Unlock()
			if c == nil {
				continue
			}
			var resp any
			if fn != nil {
				resp = fn(msg)
			}
			if resp == nil {
				resp = map[string]any{"id": msg.ID, "result": map[string]any{}}
			}
			_ = wsjson.Write(context.Background(), c, resp)
		case <-stopCh:
			return
		}
	}
}

// startMonitor creates a Monitor against srv, starts it, and returns a cleanup func.
// Waits for Target.getTargets (the last command in initSession) before returning.
func startMonitor(t *testing.T, srv *testServer, fn ResponderFunc) (*Monitor, *eventCollector, func()) {
	t.Helper()
	ec := newEventCollector()
	upstream := newTestUpstream(srv.wsURL())
	m := New(upstream, ec.publishFn(), 99, discardLogger)
	require.NoError(t, m.Start(context.Background()))

	// Closed when Target.getTargets is responded to (last command of initSession).
	// Tests needing attachExistingTargets to finish should use require.Eventually.
	initDone := make(chan struct{})
	var initOnce sync.Once

	wrappedFn := func(msg cdpMessage) any {
		var result any
		if fn != nil {
			result = fn(msg)
		}
		if msg.Method == "Target.getTargets" {
			initOnce.Do(func() { close(initDone) })
		}
		return result
	}

	stopResponder := make(chan struct{})
	go listenAndRespond(srv, stopResponder, wrappedFn)

	// Wait for the websocket connection to be established.
	select {
	case <-srv.connCh:
	case <-time.After(3 * time.Second):
		t.Fatal("fake server never received a connection")
	}
	// Wait for the init sequence to complete.
	select {
	case <-initDone:
	case <-time.After(5 * time.Second):
		t.Fatal("init sequence (Target.getTargets) did not complete")
	}

	cleanup := func() {
		close(stopResponder)
		m.Stop()
	}
	return m, ec, cleanup
}

// newComputedMonitor creates an unconnected Monitor for testing computed state
// (network_idle, layout_settled, navigation_settled) without a real websocket.
func newComputedMonitor(t *testing.T) (*Monitor, *eventCollector) {
	t.Helper()
	ec := newEventCollector()
	upstream := newTestUpstream("ws://127.0.0.1:0")
	m := New(upstream, ec.publishFn(), 0, discardLogger)
	return m, ec
}

// navigateMonitor sends a Page.frameNavigated to reset computed state.
// It ensures session "s1" has a page-like computedState before navigating,
// mirroring what handleAttachedToTarget would do in production.
func navigateMonitor(m *Monitor, url string) {
	m.sessionsMu.Lock()
	if _, ok := m.sessions["s1"]; !ok {
		m.sessions["s1"] = targetInfo{targetID: "test-target", targetType: targetTypePage}
		m.computedStates["s1"] = newComputedState(m.publish)
	}
	m.sessionsMu.Unlock()
	m.handleFrameNavigated(cdpPageFrameNavigatedParams{
		Frame: cdpPageFrame{ID: "f1", URL: url},
	}, "s1")
}

// simulateRequest sends a Network.requestWillBeSent through the handler.
func simulateRequest(m *Monitor, id string) {
	m.handleNetworkRequest(cdpNetworkRequestWillBeSentParams{
		RequestID: id,
		Type:      "Document",
		Request: cdpNetworkRequest{
			Method: "GET",
			URL:    "https://example.com/" + id,
		},
	}, "s1")
}

// simulateFinished stores minimal state and sends Network.loadingFinished.
func simulateFinished(m *Monitor, id string) {
	m.pendReqMu.Lock()
	m.pendingRequests[id] = networkReqState{sessionID: "s1", method: "GET", url: "https://example.com/" + id}
	m.pendReqMu.Unlock()
	m.handleLoadingFinished(context.Background(), cdpNetworkLoadingFinishedParams{RequestID: id}, "s1")
}
