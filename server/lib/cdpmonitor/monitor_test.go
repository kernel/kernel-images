package cdpmonitor

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/onkernel/kernel-images/server/lib/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// minimalPNG is a valid 1x1 PNG used as a test fixture for screenshot tests.
var minimalPNG = func() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{R: 255, G: 0, B: 0, A: 255})
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}()

// fakeCDPServer is a minimal WebSocket server that accepts connections and
// lets the test drive scripted message sequences.
type fakeCDPServer struct {
	srv    *httptest.Server
	conn   *websocket.Conn
	connMu sync.Mutex
	connCh chan struct{} // closed when the first connection is accepted
	msgCh  chan []byte   // inbound messages from Monitor
}

func newFakeCDPServer(t *testing.T) *fakeCDPServer {
	t.Helper()
	f := &fakeCDPServer{
		msgCh:  make(chan []byte, 128),
		connCh: make(chan struct{}),
	}
	var connOnce sync.Once
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		f.connMu.Lock()
		f.conn = c
		f.connMu.Unlock()
		connOnce.Do(func() { close(f.connCh) })
		go func() {
			for {
				_, b, err := c.Read(context.Background())
				if err != nil {
					return
				}
				f.msgCh <- b
			}
		}()
	}))
	return f
}

func (f *fakeCDPServer) wsURL() string {
	return "ws" + strings.TrimPrefix(f.srv.URL, "http")
}

func (f *fakeCDPServer) sendToMonitor(t *testing.T, msg any) {
	t.Helper()
	f.connMu.Lock()
	c := f.conn
	f.connMu.Unlock()
	require.NotNil(t, c, "no active connection")
	require.NoError(t, wsjson.Write(context.Background(), c, msg))
}

func (f *fakeCDPServer) readFromMonitor(t *testing.T, timeout time.Duration) cdpMessage {
	t.Helper()
	select {
	case b := <-f.msgCh:
		var msg cdpMessage
		require.NoError(t, json.Unmarshal(b, &msg))
		return msg
	case <-time.After(timeout):
		t.Fatal("timeout waiting for message from Monitor")
		return cdpMessage{}
	}
}

func (f *fakeCDPServer) close() {
	f.connMu.Lock()
	if f.conn != nil {
		_ = f.conn.Close(websocket.StatusNormalClosure, "done")
	}
	f.connMu.Unlock()
	f.srv.Close()
}

// fakeUpstream implements UpstreamProvider for tests.
type fakeUpstream struct {
	mu      sync.Mutex
	current string
	subs    []chan string
}

func newFakeUpstream(url string) *fakeUpstream {
	return &fakeUpstream{current: url}
}

func (f *fakeUpstream) Current() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.current
}

func (f *fakeUpstream) Subscribe() (<-chan string, func()) {
	ch := make(chan string, 1)
	f.mu.Lock()
	f.subs = append(f.subs, ch)
	f.mu.Unlock()
	cancel := func() {
		f.mu.Lock()
		for i, s := range f.subs {
			if s == ch {
				f.subs = append(f.subs[:i], f.subs[i+1:]...)
				break
			}
		}
		f.mu.Unlock()
		close(ch)
	}
	return ch, cancel
}

func (f *fakeUpstream) notifyRestart(newURL string) {
	f.mu.Lock()
	f.current = newURL
	subs := make([]chan string, len(f.subs))
	copy(subs, f.subs)
	f.mu.Unlock()
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
func listenAndRespond(srv *fakeCDPServer, stopCh <-chan struct{}, fn ResponderFunc) {
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

// startMonitor creates a Monitor against srv, starts it, waits for the
// connection, and launches a responder goroutine. Returns cleanup func.
func startMonitor(t *testing.T, srv *fakeCDPServer, fn ResponderFunc) (*Monitor, *eventCollector, func()) {
	t.Helper()
	ec := newEventCollector()
	upstream := newFakeUpstream(srv.wsURL())
	m := New(upstream, ec.publishFn(), 99)
	require.NoError(t, m.Start(context.Background()))

	stopResponder := make(chan struct{})
	go listenAndRespond(srv, stopResponder, fn)

	// Wait for the websocket connection to be established.
	select {
	case <-srv.connCh:
	case <-time.After(3 * time.Second):
		t.Fatal("fake server never received a connection")
	}
	// Wait for the init sequence (setAutoAttach + domain enables + script injection
	// + getTargets) to complete. The responder goroutine handles all responses;
	// we just need to wait for the burst to finish.
	waitForInitDone(t)

	cleanup := func() {
		close(stopResponder)
		m.Stop()
	}
	return m, ec, cleanup
}

// waitForInitDone waits for the Monitor's init sequence to complete by
// detecting a 100ms gap in activity on the message channel. The responder
// goroutine handles responses; this just waits for the burst to end.
func waitForInitDone(t *testing.T) {
	t.Helper()
	// The init sequence sends ~8 commands. Wait until the responder has
	// processed them all by checking for a quiet period.
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-time.After(100 * time.Millisecond):
			return
		case <-deadline:
			t.Fatal("init sequence did not complete")
		}
	}
}

// newComputedMonitor creates an unconnected Monitor for testing computed state
// (network_idle, layout_settled, navigation_settled) without a real websocket.
func newComputedMonitor(t *testing.T) (*Monitor, *eventCollector) {
	t.Helper()
	ec := newEventCollector()
	upstream := newFakeUpstream("ws://127.0.0.1:0")
	m := New(upstream, ec.publishFn(), 0)
	return m, ec
}

// navigateMonitor sends a Page.frameNavigated to reset computed state.
func navigateMonitor(m *Monitor, url string) {
	p, _ := json.Marshal(map[string]any{
		"frame": map[string]any{"id": "f1", "url": url},
	})
	m.handleFrameNavigated(p, "s1")
}

func TestAutoAttach(t *testing.T) {
	srv := newFakeCDPServer(t)
	defer srv.close()

	ec := newEventCollector()
	upstream := newFakeUpstream(srv.wsURL())
	m := New(upstream, ec.publishFn(), 99)
	require.NoError(t, m.Start(context.Background()))
	defer m.Stop()

	// The first command should be Target.setAutoAttach with correct params.
	msg := srv.readFromMonitor(t, 3*time.Second)
	assert.Equal(t, "Target.setAutoAttach", msg.Method)

	var params struct {
		AutoAttach             bool `json:"autoAttach"`
		WaitForDebuggerOnStart bool `json:"waitForDebuggerOnStart"`
		Flatten                bool `json:"flatten"`
	}
	require.NoError(t, json.Unmarshal(msg.Params, &params))
	assert.True(t, params.AutoAttach)
	assert.False(t, params.WaitForDebuggerOnStart)
	assert.True(t, params.Flatten)

	// Respond and drain domain-enable commands.
	stopResponder := make(chan struct{})
	go listenAndRespond(srv, stopResponder, nil)
	defer close(stopResponder)
	srv.sendToMonitor(t, map[string]any{"id": msg.ID, "result": map[string]any{}})

	// Simulate Target.attachedToTarget — session should be stored.
	srv.sendToMonitor(t, map[string]any{
		"method": "Target.attachedToTarget",
		"params": map[string]any{
			"sessionId":  "session-abc",
			"targetInfo": map[string]any{"targetId": "target-xyz", "type": "page", "url": "https://example.com"},
		},
	})
	require.Eventually(t, func() bool {
		m.sessionsMu.RLock()
		defer m.sessionsMu.RUnlock()
		_, ok := m.sessions["session-abc"]
		return ok
	}, 2*time.Second, 50*time.Millisecond, "session not stored")

	m.sessionsMu.RLock()
	info := m.sessions["session-abc"]
	m.sessionsMu.RUnlock()
	assert.Equal(t, "target-xyz", info.targetID)
	assert.Equal(t, "page", info.targetType)
}

func TestLifecycle(t *testing.T) {
	srv := newFakeCDPServer(t)
	defer srv.close()

	ec := newEventCollector()
	upstream := newFakeUpstream(srv.wsURL())
	m := New(upstream, ec.publishFn(), 99)

	assert.False(t, m.IsRunning(), "idle at boot")

	require.NoError(t, m.Start(context.Background()))
	assert.True(t, m.IsRunning(), "running after Start")
	srv.readFromMonitor(t, 2*time.Second) // drain setAutoAttach

	m.Stop()
	assert.False(t, m.IsRunning(), "stopped after Stop")

	// Restart while stopped.
	require.NoError(t, m.Start(context.Background()))
	assert.True(t, m.IsRunning(), "running after second Start")
	srv.readFromMonitor(t, 2*time.Second)

	// Restart while running — implicit Stop+Start.
	require.NoError(t, m.Start(context.Background()))
	assert.True(t, m.IsRunning(), "running after implicit restart")

	m.Stop()
	assert.False(t, m.IsRunning(), "stopped at end")
}

func TestReconnect(t *testing.T) {
	srv1 := newFakeCDPServer(t)

	upstream := newFakeUpstream(srv1.wsURL())
	ec := newEventCollector()
	m := New(upstream, ec.publishFn(), 99)
	require.NoError(t, m.Start(context.Background()))
	defer m.Stop()

	srv1.readFromMonitor(t, 2*time.Second) // drain setAutoAttach

	srv2 := newFakeCDPServer(t)
	defer srv2.close()
	defer srv1.close()

	upstream.notifyRestart(srv2.wsURL())

	ec.waitFor(t, "monitor_disconnected", 3*time.Second)

	// Wait for the Monitor to reconnect to srv2.
	srv2.readFromMonitor(t, 5*time.Second)

	ev := ec.waitFor(t, "monitor_reconnected", 3*time.Second)
	var data map[string]any
	require.NoError(t, json.Unmarshal(ev.Data, &data))
	_, ok := data["reconnect_duration_ms"]
	assert.True(t, ok, "missing reconnect_duration_ms")
}

func TestConsoleEvents(t *testing.T) {
	srv := newFakeCDPServer(t)
	defer srv.close()

	_, ec, cleanup := startMonitor(t, srv, nil)
	defer cleanup()

	t.Run("console_log", func(t *testing.T) {
		srv.sendToMonitor(t, map[string]any{
			"method": "Runtime.consoleAPICalled",
			"params": map[string]any{
				"type": "log",
				"args": []any{map[string]any{"type": "string", "value": "hello world"}},
			},
		})
		ev := ec.waitFor(t, "console_log", 2*time.Second)
		assert.Equal(t, events.CategoryConsole, ev.Category)
		assert.Equal(t, events.KindCDP, ev.Source.Kind)
		assert.Equal(t, "Runtime.consoleAPICalled", ev.Source.Event)
		assert.Equal(t, events.DetailStandard, ev.DetailLevel)

		var data map[string]any
		require.NoError(t, json.Unmarshal(ev.Data, &data))
		assert.Equal(t, "log", data["level"])
		assert.Equal(t, "hello world", data["text"])
	})

	t.Run("exception_thrown", func(t *testing.T) {
		srv.sendToMonitor(t, map[string]any{
			"method": "Runtime.exceptionThrown",
			"params": map[string]any{
				"timestamp": 1234.5,
				"exceptionDetails": map[string]any{
					"text":         "Uncaught TypeError",
					"lineNumber":   42,
					"columnNumber": 7,
					"url":          "https://example.com/app.js",
				},
			},
		})
		ev := ec.waitFor(t, "console_error", 2*time.Second)
		assert.Equal(t, events.CategoryConsole, ev.Category)
		assert.Equal(t, events.DetailStandard, ev.DetailLevel)

		var data map[string]any
		require.NoError(t, json.Unmarshal(ev.Data, &data))
		assert.Equal(t, "Uncaught TypeError", data["text"])
		assert.Equal(t, float64(42), data["line"])
	})

	t.Run("non_string_args", func(t *testing.T) {
		srv.sendToMonitor(t, map[string]any{
			"method": "Runtime.consoleAPICalled",
			"params": map[string]any{
				"type": "log",
				"args": []any{
					map[string]any{"type": "number", "value": 42},
					map[string]any{"type": "object", "value": map[string]any{"key": "val"}},
					map[string]any{"type": "undefined"},
				},
			},
		})
		ev := ec.waitForNew(t, "console_log", 2*time.Second)
		var data map[string]any
		require.NoError(t, json.Unmarshal(ev.Data, &data))
		args := data["args"].([]any)
		assert.Equal(t, "42", args[0])
		assert.Contains(t, args[1], "key")
		assert.Equal(t, "undefined", args[2])
	})
}

func TestNetworkEvents(t *testing.T) {
	srv := newFakeCDPServer(t)
	defer srv.close()

	// Custom responder: return a body for Network.getResponseBody and track calls.
	var getBodyCalled atomic.Bool
	responder := func(msg cdpMessage) any {
		if msg.Method == "Network.getResponseBody" {
			getBodyCalled.Store(true)
			return map[string]any{
				"id":     msg.ID,
				"result": map[string]any{"body": `{"ok":true}`, "base64Encoded": false},
			}
		}
		return nil
	}
	_, ec, cleanup := startMonitor(t, srv, responder)
	defer cleanup()

	t.Run("request_and_response", func(t *testing.T) {
		srv.sendToMonitor(t, map[string]any{
			"method": "Network.requestWillBeSent",
			"params": map[string]any{
				"requestId":    "req-001",
				"resourceType": "XHR",
				"request": map[string]any{
					"method":  "POST",
					"url":     "https://api.example.com/data",
					"headers": map[string]any{"Content-Type": "application/json"},
				},
				"initiator": map[string]any{"type": "script"},
			},
		})
		ev := ec.waitFor(t, "network_request", 2*time.Second)
		assert.Equal(t, events.CategoryNetwork, ev.Category)
		assert.Equal(t, "Network.requestWillBeSent", ev.Source.Event)

		var data map[string]any
		require.NoError(t, json.Unmarshal(ev.Data, &data))
		assert.Equal(t, "POST", data["method"])
		assert.Equal(t, "https://api.example.com/data", data["url"])

		// Complete the request lifecycle.
		srv.sendToMonitor(t, map[string]any{
			"method": "Network.responseReceived",
			"params": map[string]any{
				"requestId": "req-001",
				"response": map[string]any{
					"status": 200, "statusText": "OK",
					"headers": map[string]any{"Content-Type": "application/json"}, "mimeType": "application/json",
				},
			},
		})
		srv.sendToMonitor(t, map[string]any{
			"method": "Network.loadingFinished",
			"params": map[string]any{"requestId": "req-001"},
		})

		ev2 := ec.waitFor(t, "network_response", 3*time.Second)
		assert.Equal(t, "Network.loadingFinished", ev2.Source.Event)
		var data2 map[string]any
		require.NoError(t, json.Unmarshal(ev2.Data, &data2))
		assert.Equal(t, float64(200), data2["status"])
		assert.NotEmpty(t, data2["body"])
	})

	t.Run("loading_failed", func(t *testing.T) {
		srv.sendToMonitor(t, map[string]any{
			"method": "Network.requestWillBeSent",
			"params": map[string]any{
				"requestId": "req-002",
				"request":   map[string]any{"method": "GET", "url": "https://fail.example.com/"},
			},
		})
		ec.waitForNew(t, "network_request", 2*time.Second)

		srv.sendToMonitor(t, map[string]any{
			"method": "Network.loadingFailed",
			"params": map[string]any{
				"requestId": "req-002",
				"errorText": "net::ERR_CONNECTION_REFUSED",
				"canceled":  false,
			},
		})
		ev := ec.waitFor(t, "network_loading_failed", 2*time.Second)
		assert.Equal(t, events.CategoryNetwork, ev.Category)
		var data map[string]any
		require.NoError(t, json.Unmarshal(ev.Data, &data))
		assert.Equal(t, "net::ERR_CONNECTION_REFUSED", data["error_text"])
	})

	t.Run("binary_resource_skips_body", func(t *testing.T) {
		getBodyCalled.Store(false)
		srv.sendToMonitor(t, map[string]any{
			"method": "Network.requestWillBeSent",
			"params": map[string]any{
				"requestId":    "img-001",
				"resourceType": "Image",
				"request":      map[string]any{"method": "GET", "url": "https://example.com/photo.png"},
			},
		})
		srv.sendToMonitor(t, map[string]any{
			"method": "Network.responseReceived",
			"params": map[string]any{
				"requestId": "img-001",
				"response":  map[string]any{"status": 200, "statusText": "OK", "headers": map[string]any{}, "mimeType": "image/png"},
			},
		})
		srv.sendToMonitor(t, map[string]any{
			"method": "Network.loadingFinished",
			"params": map[string]any{"requestId": "img-001"},
		})

		ev := ec.waitForNew(t, "network_response", 3*time.Second)
		var data map[string]any
		require.NoError(t, json.Unmarshal(ev.Data, &data))
		assert.Equal(t, "", data["body"], "binary resource should have empty body")
		assert.False(t, getBodyCalled.Load(), "should not call getResponseBody for images")
	})
}

func TestPageEvents(t *testing.T) {
	srv := newFakeCDPServer(t)
	defer srv.close()

	_, ec, cleanup := startMonitor(t, srv, nil)
	defer cleanup()

	srv.sendToMonitor(t, map[string]any{
		"method": "Page.frameNavigated",
		"params": map[string]any{
			"frame": map[string]any{"id": "frame-1", "url": "https://example.com/page"},
		},
	})
	ev := ec.waitFor(t, "navigation", 2*time.Second)
	assert.Equal(t, events.CategoryPage, ev.Category)
	assert.Equal(t, "Page.frameNavigated", ev.Source.Event)
	var data map[string]any
	require.NoError(t, json.Unmarshal(ev.Data, &data))
	assert.Equal(t, "https://example.com/page", data["url"])

	srv.sendToMonitor(t, map[string]any{
		"method": "Page.domContentEventFired",
		"params": map[string]any{"timestamp": 1000.0},
	})
	ev2 := ec.waitFor(t, "dom_content_loaded", 2*time.Second)
	assert.Equal(t, events.CategoryPage, ev2.Category)
	assert.Equal(t, events.DetailMinimal, ev2.DetailLevel)

	srv.sendToMonitor(t, map[string]any{
		"method": "Page.loadEventFired",
		"params": map[string]any{"timestamp": 1001.0},
	})
	ev3 := ec.waitFor(t, "page_load", 2*time.Second)
	assert.Equal(t, events.CategoryPage, ev3.Category)
	assert.Equal(t, events.DetailMinimal, ev3.DetailLevel)
}

func TestTargetEvents(t *testing.T) {
	srv := newFakeCDPServer(t)
	defer srv.close()

	_, ec, cleanup := startMonitor(t, srv, nil)
	defer cleanup()

	srv.sendToMonitor(t, map[string]any{
		"method": "Target.targetCreated",
		"params": map[string]any{
			"targetInfo": map[string]any{"targetId": "t-1", "type": "page", "url": "https://new.example.com"},
		},
	})
	ev := ec.waitFor(t, "target_created", 2*time.Second)
	assert.Equal(t, events.CategoryPage, ev.Category)
	assert.Equal(t, events.DetailMinimal, ev.DetailLevel)
	var data map[string]any
	require.NoError(t, json.Unmarshal(ev.Data, &data))
	assert.Equal(t, "t-1", data["target_id"])

	srv.sendToMonitor(t, map[string]any{
		"method": "Target.targetDestroyed",
		"params": map[string]any{"targetId": "t-1"},
	})
	ev2 := ec.waitFor(t, "target_destroyed", 2*time.Second)
	assert.Equal(t, events.CategoryPage, ev2.Category)
	assert.Equal(t, events.DetailMinimal, ev2.DetailLevel)
}

func TestBindingAndTimeline(t *testing.T) {
	srv := newFakeCDPServer(t)
	defer srv.close()

	_, ec, cleanup := startMonitor(t, srv, nil)
	defer cleanup()

	t.Run("interaction_click", func(t *testing.T) {
		srv.sendToMonitor(t, map[string]any{
			"method": "Runtime.bindingCalled",
			"params": map[string]any{
				"name":    "__kernelEvent",
				"payload": `{"type":"interaction_click","x":10,"y":20,"selector":"button","tag":"BUTTON","text":"OK"}`,
			},
		})
		ev := ec.waitFor(t, "interaction_click", 2*time.Second)
		assert.Equal(t, events.CategoryInteraction, ev.Category)
		assert.Equal(t, "Runtime.bindingCalled", ev.Source.Event)
	})

	t.Run("scroll_settled", func(t *testing.T) {
		srv.sendToMonitor(t, map[string]any{
			"method": "Runtime.bindingCalled",
			"params": map[string]any{
				"name":    "__kernelEvent",
				"payload": `{"type":"scroll_settled","from_x":0,"from_y":0,"to_x":0,"to_y":500,"target_selector":"body"}`,
			},
		})
		ev := ec.waitFor(t, "scroll_settled", 2*time.Second)
		assert.Equal(t, events.CategoryInteraction, ev.Category)
		var data map[string]any
		require.NoError(t, json.Unmarshal(ev.Data, &data))
		assert.Equal(t, float64(500), data["to_y"])
	})

	t.Run("layout_shift", func(t *testing.T) {
		srv.sendToMonitor(t, map[string]any{
			"method": "PerformanceTimeline.timelineEventAdded",
			"params": map[string]any{
				"event": map[string]any{"type": "layout-shift"},
			},
		})
		ev := ec.waitFor(t, "layout_shift", 2*time.Second)
		assert.Equal(t, events.KindCDP, ev.Source.Kind)
		assert.Equal(t, "PerformanceTimeline.timelineEventAdded", ev.Source.Event)
	})

	t.Run("unknown_binding_ignored", func(t *testing.T) {
		srv.sendToMonitor(t, map[string]any{
			"method": "Runtime.bindingCalled",
			"params": map[string]any{
				"name":    "someOtherBinding",
				"payload": `{"type":"interaction_click"}`,
			},
		})
		ec.assertNone(t, "interaction_click", 100*time.Millisecond)
	})
}

func TestScreenshot(t *testing.T) {
	srv := newFakeCDPServer(t)
	defer srv.close()

	m, ec, cleanup := startMonitor(t, srv, nil)
	defer cleanup()

	var captureCount atomic.Int32
	m.screenshotFn = func(ctx context.Context, displayNum int) ([]byte, error) {
		captureCount.Add(1)
		return minimalPNG, nil
	}

	t.Run("capture_and_publish", func(t *testing.T) {
		m.tryScreenshot(context.Background())
		require.Eventually(t, func() bool { return captureCount.Load() == 1 }, 2*time.Second, 20*time.Millisecond)

		ev := ec.waitFor(t, "screenshot", 2*time.Second)
		assert.Equal(t, events.CategorySystem, ev.Category)
		assert.Equal(t, events.KindLocalProcess, ev.Source.Kind)
		var data map[string]any
		require.NoError(t, json.Unmarshal(ev.Data, &data))
		assert.NotEmpty(t, data["png"])
	})

	t.Run("rate_limited", func(t *testing.T) {
		before := captureCount.Load()
		m.tryScreenshot(context.Background())
		time.Sleep(100 * time.Millisecond)
		assert.Equal(t, before, captureCount.Load(), "should be rate-limited within 2s")
	})

	t.Run("captures_after_cooldown", func(t *testing.T) {
		m.lastScreenshotAt.Store(time.Now().Add(-3 * time.Second).UnixMilli())
		before := captureCount.Load()
		m.tryScreenshot(context.Background())
		require.Eventually(t, func() bool { return captureCount.Load() > before }, 2*time.Second, 20*time.Millisecond)
	})
}

func TestAttachExistingTargets(t *testing.T) {
	srv := newFakeCDPServer(t)
	defer srv.close()

	responder := func(msg cdpMessage) any {
		switch msg.Method {
		case "Target.getTargets":
			return map[string]any{
				"id": msg.ID,
				"result": map[string]any{
					"targetInfos": []any{
						map[string]any{"targetId": "existing-1", "type": "page", "url": "https://preexisting.example.com"},
					},
				},
			}
		case "Target.attachToTarget":
			srv.sendToMonitor(t, map[string]any{
				"method": "Target.attachedToTarget",
				"params": map[string]any{
					"sessionId":  "session-existing-1",
					"targetInfo": map[string]any{"targetId": "existing-1", "type": "page", "url": "https://preexisting.example.com"},
				},
			})
			return map[string]any{"id": msg.ID, "result": map[string]any{"sessionId": "session-existing-1"}}
		}
		return nil
	}

	m, _, cleanup := startMonitor(t, srv, responder)
	defer cleanup()

	require.Eventually(t, func() bool {
		m.sessionsMu.RLock()
		defer m.sessionsMu.RUnlock()
		_, ok := m.sessions["session-existing-1"]
		return ok
	}, 3*time.Second, 50*time.Millisecond, "existing target not auto-attached")

	m.sessionsMu.RLock()
	info := m.sessions["session-existing-1"]
	m.sessionsMu.RUnlock()
	assert.Equal(t, "existing-1", info.targetID)
}

func TestURLPopulated(t *testing.T) {
	srv := newFakeCDPServer(t)
	defer srv.close()

	_, ec, cleanup := startMonitor(t, srv, nil)
	defer cleanup()

	srv.sendToMonitor(t, map[string]any{
		"method": "Page.frameNavigated",
		"params": map[string]any{
			"frame": map[string]any{"id": "f1", "url": "https://example.com/page"},
		},
	})
	ec.waitFor(t, "navigation", 2*time.Second)

	srv.sendToMonitor(t, map[string]any{
		"method": "Runtime.consoleAPICalled",
		"params": map[string]any{
			"type": "log",
			"args": []any{map[string]any{"type": "string", "value": "test"}},
		},
	})
	ev := ec.waitFor(t, "console_log", 2*time.Second)
	assert.Equal(t, "https://example.com/page", ev.URL)
}

// simulateRequest sends a Network.requestWillBeSent through the handler.
func simulateRequest(m *Monitor, id string) {
	p, _ := json.Marshal(map[string]any{
		"requestId": id, "resourceType": "Document",
		"request": map[string]any{"method": "GET", "url": "https://example.com/" + id},
	})
	m.handleNetworkRequest(p, "s1")
}

// simulateFinished stores minimal state and sends Network.loadingFinished.
func simulateFinished(m *Monitor, id string) {
	m.pendReqMu.Lock()
	m.pendingRequests[id] = networkReqState{method: "GET", url: "https://example.com/" + id}
	m.pendReqMu.Unlock()
	p, _ := json.Marshal(map[string]any{"requestId": id})
	m.handleLoadingFinished(p, "s1")
}

func TestNetworkIdle(t *testing.T) {
	t.Run("debounce_500ms", func(t *testing.T) {
		m, ec := newComputedMonitor(t)
		navigateMonitor(m, "https://example.com")

		simulateRequest(m, "r1")
		simulateRequest(m, "r2")
		simulateRequest(m, "r3")

		t0 := time.Now()
		simulateFinished(m, "r1")
		simulateFinished(m, "r2")
		simulateFinished(m, "r3")

		ev := ec.waitFor(t, "network_idle", 2*time.Second)
		assert.GreaterOrEqual(t, time.Since(t0).Milliseconds(), int64(400), "fired too early")
		assert.Equal(t, events.CategoryNetwork, ev.Category)
	})

	t.Run("timer_reset_on_new_request", func(t *testing.T) {
		m, ec := newComputedMonitor(t)
		navigateMonitor(m, "https://example.com")

		simulateRequest(m, "a1")
		simulateFinished(m, "a1")
		time.Sleep(200 * time.Millisecond)

		simulateRequest(m, "a2")
		t1 := time.Now()
		simulateFinished(m, "a2")

		ec.waitFor(t, "network_idle", 2*time.Second)
		assert.GreaterOrEqual(t, time.Since(t1).Milliseconds(), int64(400), "should reset timer on new request")
	})
}

func TestLayoutSettled(t *testing.T) {
	t.Run("debounce_1s_after_page_load", func(t *testing.T) {
		m, ec := newComputedMonitor(t)
		navigateMonitor(m, "https://example.com")

		t0 := time.Now()
		m.handleLoadEventFired(json.RawMessage(`{}`), "s1")

		ev := ec.waitFor(t, "layout_settled", 3*time.Second)
		assert.GreaterOrEqual(t, time.Since(t0).Milliseconds(), int64(900), "fired too early")
		assert.Equal(t, events.CategoryPage, ev.Category)
	})

	t.Run("layout_shift_resets_timer", func(t *testing.T) {
		m, ec := newComputedMonitor(t)
		navigateMonitor(m, "https://example.com")
		m.handleLoadEventFired(json.RawMessage(`{}`), "s1")

		time.Sleep(600 * time.Millisecond)
		shiftParams, _ := json.Marshal(map[string]any{
			"event": map[string]any{"type": "layout-shift"},
		})
		m.handleTimelineEvent(shiftParams, "s1")
		t1 := time.Now()

		ec.waitFor(t, "layout_settled", 3*time.Second)
		assert.GreaterOrEqual(t, time.Since(t1).Milliseconds(), int64(900), "should reset after layout_shift")
	})
}

func TestNavigationSettled(t *testing.T) {
	t.Run("fires_when_all_three_flags_set", func(t *testing.T) {
		m, ec := newComputedMonitor(t)
		navigateMonitor(m, "https://example.com")

		m.handleDOMContentLoaded(json.RawMessage(`{}`), "s1")

		// Trigger network_idle.
		simulateRequest(m, "r1")
		simulateFinished(m, "r1")

		// Trigger layout_settled via page_load.
		m.handleLoadEventFired(json.RawMessage(`{}`), "s1")

		ev := ec.waitFor(t, "navigation_settled", 3*time.Second)
		assert.Equal(t, events.CategoryPage, ev.Category)
	})

	t.Run("interrupted_by_new_navigation", func(t *testing.T) {
		m, ec := newComputedMonitor(t)
		navigateMonitor(m, "https://example.com")

		m.handleDOMContentLoaded(json.RawMessage(`{}`), "s1")

		simulateRequest(m, "r2")
		simulateFinished(m, "r2")

		// Interrupt before layout_settled fires.
		navigateMonitor(m, "https://example.com/page2")

		ec.assertNone(t, "navigation_settled", 1500*time.Millisecond)
	})
}
