package cdpmonitor

import (
	"context"
	"encoding/json"
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

// fakeCDPServer is a minimal WebSocket server that accepts connections and
// lets the test drive scripted message sequences.
type fakeCDPServer struct {
	srv    *httptest.Server
	conn   *websocket.Conn
	connMu sync.Mutex
	msgCh  chan []byte // inbound messages from Monitor
}

func newFakeCDPServer(t *testing.T) *fakeCDPServer {
	t.Helper()
	f := &fakeCDPServer{
		msgCh: make(chan []byte, 128),
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		f.connMu.Lock()
		f.conn = c
		f.connMu.Unlock()
		// drain messages from Monitor into msgCh until connection closes
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

// wsURL returns a ws:// URL pointing at the fake server.
func (f *fakeCDPServer) wsURL() string {
	return "ws" + strings.TrimPrefix(f.srv.URL, "http")
}

// sendToMonitor pushes a raw JSON message to the Monitor's readLoop.
func (f *fakeCDPServer) sendToMonitor(t *testing.T, msg any) {
	t.Helper()
	f.connMu.Lock()
	c := f.conn
	f.connMu.Unlock()
	require.NotNil(t, c, "no active connection")
	err := wsjson.Write(context.Background(), c, msg)
	require.NoError(t, err)
}

// readFromMonitor blocks until the Monitor sends a message (with timeout).
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

// notifyRestart simulates Chrome restarting with a new DevTools URL.
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

// --- Tests ---

// TestMonitorStart verifies that Monitor.Start() dials the URL from
// UpstreamProvider.Current() and establishes an isolated WebSocket connection.
func TestMonitorStart(t *testing.T) {
	srv := newFakeCDPServer(t)
	defer srv.close()

	upstream := newFakeUpstream(srv.wsURL())
	var published []events.Event
	var publishMu sync.Mutex
	publishFn := func(ev events.Event) {
		publishMu.Lock()
		published = append(published, ev)
		publishMu.Unlock()
	}

	m := New(upstream, publishFn, 99)

	ctx := context.Background()
	err := m.Start(ctx)
	require.NoError(t, err)
	defer m.Stop()

	// Give readLoop time to start and send the setAutoAttach command.
	// We just verify the connection was made and the Monitor is running.
	assert.True(t, m.IsRunning())

	// Read the first message sent by the Monitor — it should be Target.setAutoAttach.
	msg := srv.readFromMonitor(t, 3*time.Second)
	assert.Equal(t, "Target.setAutoAttach", msg.Method)
}

// TestAutoAttach verifies that after Start(), the Monitor sends
// Target.setAutoAttach{autoAttach:true, waitForDebuggerOnStart:false, flatten:true}
// and that on receiving Target.attachedToTarget the session is stored.
func TestAutoAttach(t *testing.T) {
	srv := newFakeCDPServer(t)
	defer srv.close()

	upstream := newFakeUpstream(srv.wsURL())
	publishFn := func(ev events.Event) {}

	m := New(upstream, publishFn, 99)

	ctx := context.Background()
	err := m.Start(ctx)
	require.NoError(t, err)
	defer m.Stop()

	// Read the setAutoAttach request from the Monitor.
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

	// Acknowledge the command with a response.
	srv.sendToMonitor(t, map[string]any{
		"id":     msg.ID,
		"result": map[string]any{},
	})

	// Drain any domain-enable commands sent after setAutoAttach.
	// The Monitor calls enableDomains (Runtime.enable, Network.enable, Page.enable, DOM.enable).
	drainTimeout := time.NewTimer(500 * time.Millisecond)
	for {
		select {
		case b := <-srv.msgCh:
			var m2 cdpMessage
			_ = json.Unmarshal(b, &m2)
			// respond to enable commands
			srv.connMu.Lock()
			c := srv.conn
			srv.connMu.Unlock()
			if c != nil && m2.ID != 0 {
				_ = wsjson.Write(context.Background(), c, map[string]any{
					"id":     m2.ID,
					"result": map[string]any{},
				})
			}
		case <-drainTimeout.C:
			goto afterDrain
		}
	}
afterDrain:

	// Now simulate Target.attachedToTarget event.
	const testSessionID = "session-abc-123"
	const testTargetID = "target-xyz-456"
	srv.sendToMonitor(t, map[string]any{
		"method": "Target.attachedToTarget",
		"params": map[string]any{
			"sessionId": testSessionID,
			"targetInfo": map[string]any{
				"targetId": testTargetID,
				"type":     "page",
				"url":      "https://example.com",
			},
		},
	})

	// Give the Monitor time to process the event and store the session.
	require.Eventually(t, func() bool {
		m.sessionsMu.RLock()
		defer m.sessionsMu.RUnlock()
		_, ok := m.sessions[testSessionID]
		return ok
	}, 2*time.Second, 50*time.Millisecond, "session not stored after attachedToTarget")

	m.sessionsMu.RLock()
	info := m.sessions[testSessionID]
	m.sessionsMu.RUnlock()
	assert.Equal(t, testTargetID, info.targetID)
	assert.Equal(t, "page", info.targetType)
}

// TestLifecycle verifies the idle→running→stopped→restart state machine.
func TestLifecycle(t *testing.T) {
	srv := newFakeCDPServer(t)
	defer srv.close()

	upstream := newFakeUpstream(srv.wsURL())
	publishFn := func(ev events.Event) {}

	m := New(upstream, publishFn, 99)

	// Idle at boot.
	assert.False(t, m.IsRunning(), "should be idle at boot")

	ctx := context.Background()

	// First Start.
	err := m.Start(ctx)
	require.NoError(t, err)
	assert.True(t, m.IsRunning(), "should be running after Start")

	// Drain the setAutoAttach message.
	select {
	case <-srv.msgCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for setAutoAttach")
	}

	// Stop.
	m.Stop()
	assert.False(t, m.IsRunning(), "should be stopped after Stop")

	// Second Start while stopped — should start fresh.
	err = m.Start(ctx)
	require.NoError(t, err)
	assert.True(t, m.IsRunning(), "should be running after second Start")

	// Drain the setAutoAttach message for the second start.
	select {
	case <-srv.msgCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for setAutoAttach on second start")
	}

	// Second Start while already running — stop+restart.
	err = m.Start(ctx)
	require.NoError(t, err)
	assert.True(t, m.IsRunning(), "should be running after stop+restart")

	m.Stop()
	assert.False(t, m.IsRunning(), "should be stopped at end")
}

// TestReconnect verifies that when UpstreamManager emits a new URL (Chrome restart),
// the monitor emits monitor_disconnected, reconnects, and emits monitor_reconnected.
func TestReconnect(t *testing.T) {
	srv1 := newFakeCDPServer(t)

	upstream := newFakeUpstream(srv1.wsURL())

	var published []events.Event
	var publishMu sync.Mutex
	var publishCount atomic.Int32
	publishFn := func(ev events.Event) {
		publishMu.Lock()
		published = append(published, ev)
		publishMu.Unlock()
		publishCount.Add(1)
	}

	m := New(upstream, publishFn, 99)

	ctx := context.Background()
	err := m.Start(ctx)
	require.NoError(t, err)
	defer m.Stop()

	// Drain setAutoAttach from srv1.
	select {
	case <-srv1.msgCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for initial setAutoAttach")
	}

	// Set up srv2 as the new Chrome URL.
	srv2 := newFakeCDPServer(t)
	defer srv2.close()
	defer srv1.close()

	// Trigger Chrome restart notification.
	upstream.notifyRestart(srv2.wsURL())

	// Wait for monitor_disconnected event.
	require.Eventually(t, func() bool {
		publishMu.Lock()
		defer publishMu.Unlock()
		for _, ev := range published {
			if ev.Type == "monitor_disconnected" {
				return true
			}
		}
		return false
	}, 3*time.Second, 50*time.Millisecond, "monitor_disconnected not published")

	// Wait for the Monitor to connect to srv2 and send setAutoAttach.
	select {
	case <-srv2.msgCh:
		// setAutoAttach received on srv2
	case <-time.After(5*time.Second):
		t.Fatal("timeout waiting for setAutoAttach on srv2 after reconnect")
	}

	// Wait for monitor_reconnected event.
	require.Eventually(t, func() bool {
		publishMu.Lock()
		defer publishMu.Unlock()
		for _, ev := range published {
			if ev.Type == "monitor_reconnected" {
				return true
			}
		}
		return false
	}, 3*time.Second, 50*time.Millisecond, "monitor_reconnected not published")

	// Verify monitor_reconnected contains reconnect_duration_ms.
	publishMu.Lock()
	var reconnEv events.Event
	for _, ev := range published {
		if ev.Type == "monitor_reconnected" {
			reconnEv = ev
			break
		}
	}
	publishMu.Unlock()

	require.NotEmpty(t, reconnEv.Type)
	var data map[string]any
	require.NoError(t, json.Unmarshal(reconnEv.Data, &data))
	_, hasField := data["reconnect_duration_ms"]
	assert.True(t, hasField, "monitor_reconnected missing reconnect_duration_ms field")
}

// listenAndRespondAll drains srv.msgCh and responds with empty results until stopCh is closed.
func listenAndRespondAll(srv *fakeCDPServer, stopCh <-chan struct{}) {
	for {
		select {
		case b := <-srv.msgCh:
			var msg cdpMessage
			if err := json.Unmarshal(b, &msg); err != nil {
				continue
			}
			if msg.ID == 0 {
				continue
			}
			srv.connMu.Lock()
			c := srv.conn
			srv.connMu.Unlock()
			if c != nil {
				_ = wsjson.Write(context.Background(), c, map[string]any{
					"id":     msg.ID,
					"result": map[string]any{},
				})
			}
		case <-stopCh:
			return
		}
	}
}


// startMonitorWithFakeServer is a helper that starts a monitor against a fake CDP server,
// drains the initial setAutoAttach + domain-enable commands, and returns a cleanup func.
func startMonitorWithFakeServer(t *testing.T, srv *fakeCDPServer) (*Monitor, *[]events.Event, *sync.Mutex, func()) {
	t.Helper()
	published := make([]events.Event, 0, 32)
	var mu sync.Mutex
	publishFn := func(ev events.Event) {
		mu.Lock()
		published = append(published, ev)
		mu.Unlock()
	}
	upstream := newFakeUpstream(srv.wsURL())
	m := New(upstream, publishFn, 99)
	ctx := context.Background()
	require.NoError(t, m.Start(ctx))

	stopResponder := make(chan struct{})
	go listenAndRespondAll(srv, stopResponder)

	cleanup := func() {
		close(stopResponder)
		m.Stop()
	}
	// Wait until the fake server has an active connection.
	require.Eventually(t, func() bool {
		srv.connMu.Lock()
		defer srv.connMu.Unlock()
		return srv.conn != nil
	}, 3*time.Second, 20*time.Millisecond, "fake server never received a connection")
	// Allow the readLoop and init commands to settle before sending test events.
	time.Sleep(150 * time.Millisecond)
	return m, &published, &mu, cleanup
}

// waitForEvent blocks until an event of the given type is published, or times out.
func waitForEvent(t *testing.T, published *[]events.Event, mu *sync.Mutex, eventType string, timeout time.Duration) events.Event {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		mu.Lock()
		for _, ev := range *published {
			if ev.Type == eventType {
				mu.Unlock()
				return ev
			}
		}
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for event type=%q", eventType)
	return events.Event{}
}


// TestConsoleEvents verifies console_log, console_error, and [KERNEL_EVENT] sentinel routing.
func TestConsoleEvents(t *testing.T) {
	srv := newFakeCDPServer(t)
	defer srv.close()

	_, published, mu, cleanup := startMonitorWithFakeServer(t, srv)
	defer cleanup()

	// 1. consoleAPICalled → console_log
	srv.sendToMonitor(t, map[string]any{
		"method": "Runtime.consoleAPICalled",
		"params": map[string]any{
			"type":               "log",
			"args":               []any{map[string]any{"type": "string", "value": "hello world"}},
			"executionContextId": 1,
		},
	})
	ev := waitForEvent(t, published, mu, "console_log", 2*time.Second)
	assert.Equal(t, events.CategoryConsole, ev.Category)
	assert.Equal(t, events.KindCDP, ev.Source.Kind)
	assert.Equal(t, "Runtime.consoleAPICalled", ev.Source.Event)
	assert.Equal(t, events.DetailStandard, ev.DetailLevel)
	var data map[string]any
	require.NoError(t, json.Unmarshal(ev.Data, &data))
	assert.Equal(t, "log", data["level"])
	assert.Equal(t, "hello world", data["text"])

	// 2. exceptionThrown → console_error
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
	ev2 := waitForEvent(t, published, mu, "console_error", 2*time.Second)
	assert.Equal(t, events.CategoryConsole, ev2.Category)
	assert.Equal(t, events.KindCDP, ev2.Source.Kind)
	assert.Equal(t, "Runtime.exceptionThrown", ev2.Source.Event)
	assert.Equal(t, events.DetailStandard, ev2.DetailLevel)
	var data2 map[string]any
	require.NoError(t, json.Unmarshal(ev2.Data, &data2))
	assert.Equal(t, "Uncaught TypeError", data2["text"])
	assert.Equal(t, float64(42), data2["line"])
	assert.Equal(t, float64(7), data2["column"])

	// 3. Runtime.bindingCalled → interaction_click (via __kernelEvent binding)
	srv.sendToMonitor(t, map[string]any{
		"method": "Runtime.bindingCalled",
		"params": map[string]any{
			"name":    "__kernelEvent",
			"payload": `{"type":"interaction_click","x":10,"y":20,"selector":"button","tag":"BUTTON","text":"OK"}`,
		},
	})
	ev3 := waitForEvent(t, published, mu, "interaction_click", 2*time.Second)
	assert.Equal(t, events.CategoryInteraction, ev3.Category)
	assert.Equal(t, "Runtime.bindingCalled", ev3.Source.Event)
}

// TestNetworkEvents verifies network_request, network_response, and network_loading_failed.
func TestNetworkEvents(t *testing.T) {
	srv := newFakeCDPServer(t)
	defer srv.close()

	published := make([]events.Event, 0, 32)
	var mu sync.Mutex
	upstream := newFakeUpstream(srv.wsURL())
	m := New(upstream, func(ev events.Event) {
		mu.Lock()
		published = append(published, ev)
		mu.Unlock()
	}, 99)
	ctx := context.Background()
	require.NoError(t, m.Start(ctx))
	defer m.Stop()

	// Responder goroutine: answer all commands from the monitor.
	// For Network.getResponseBody, return a real body; for everything else return {}.
	stopResponder := make(chan struct{})
	defer close(stopResponder)
	go func() {
		for {
			select {
			case b := <-srv.msgCh:
				var msg cdpMessage
				if err := json.Unmarshal(b, &msg); err != nil {
					continue
				}
				if msg.ID == 0 {
					continue
				}
				srv.connMu.Lock()
				c := srv.conn
				srv.connMu.Unlock()
				if c == nil {
					continue
				}
				var resp any
				if msg.Method == "Network.getResponseBody" {
					resp = map[string]any{
						"id":     msg.ID,
						"result": map[string]any{"body": `{"ok":true}`, "base64Encoded": false},
					}
				} else {
					resp = map[string]any{"id": msg.ID, "result": map[string]any{}}
				}
				_ = wsjson.Write(context.Background(), c, resp)
			case <-stopResponder:
				return
			}
		}
	}()

	// Wait for connection.
	require.Eventually(t, func() bool {
		srv.connMu.Lock()
		defer srv.connMu.Unlock()
		return srv.conn != nil
	}, 3*time.Second, 20*time.Millisecond)
	time.Sleep(150 * time.Millisecond)

	const reqID = "req-001"

	// 1. requestWillBeSent → network_request
	srv.sendToMonitor(t, map[string]any{
		"method": "Network.requestWillBeSent",
		"params": map[string]any{
			"requestId":    reqID,
			"resourceType": "XHR",
			"request": map[string]any{
				"method":  "POST",
				"url":     "https://api.example.com/data",
				"headers": map[string]any{"Content-Type": "application/json"},
			},
			"initiator": map[string]any{"type": "script"},
		},
	})
	ev := waitForEvent(t, &published, &mu, "network_request", 2*time.Second)
	assert.Equal(t, events.CategoryNetwork, ev.Category)
	assert.Equal(t, events.KindCDP, ev.Source.Kind)
	assert.Equal(t, "Network.requestWillBeSent", ev.Source.Event)
	var data map[string]any
	require.NoError(t, json.Unmarshal(ev.Data, &data))
	assert.Equal(t, "POST", data["method"])
	assert.Equal(t, "https://api.example.com/data", data["url"])

	// 2. responseReceived + loadingFinished → network_response (with body via getResponseBody)
	srv.sendToMonitor(t, map[string]any{
		"method": "Network.responseReceived",
		"params": map[string]any{
			"requestId": reqID,
			"response": map[string]any{
				"status":     200,
				"statusText": "OK",
				"url":        "https://api.example.com/data",
				"headers":    map[string]any{"Content-Type": "application/json"},
				"mimeType":   "application/json",
			},
		},
	})
	srv.sendToMonitor(t, map[string]any{
		"method": "Network.loadingFinished",
		"params": map[string]any{
			"requestId": reqID,
		},
	})

	ev2 := waitForEvent(t, &published, &mu, "network_response", 3*time.Second)
	assert.Equal(t, events.CategoryNetwork, ev2.Category)
	assert.Equal(t, "Network.loadingFinished", ev2.Source.Event)
	var data2 map[string]any
	require.NoError(t, json.Unmarshal(ev2.Data, &data2))
	assert.Equal(t, float64(200), data2["status"])
	assert.NotEmpty(t, data2["body"])

	// 3. loadingFailed → network_loading_failed
	const reqID2 = "req-002"
	srv.sendToMonitor(t, map[string]any{
		"method": "Network.requestWillBeSent",
		"params": map[string]any{
			"requestId": reqID2,
			"request": map[string]any{
				"method": "GET",
				"url":    "https://fail.example.com/",
			},
		},
	})
	waitForEvent(t, &published, &mu, "network_request", 2*time.Second)

	mu.Lock()
	published = published[:0]
	mu.Unlock()

	srv.sendToMonitor(t, map[string]any{
		"method": "Network.loadingFailed",
		"params": map[string]any{
			"requestId": reqID2,
			"errorText": "net::ERR_CONNECTION_REFUSED",
			"canceled":  false,
		},
	})
	ev3 := waitForEvent(t, &published, &mu, "network_loading_failed", 2*time.Second)
	assert.Equal(t, events.CategoryNetwork, ev3.Category)
	var data3 map[string]any
	require.NoError(t, json.Unmarshal(ev3.Data, &data3))
	assert.Equal(t, "net::ERR_CONNECTION_REFUSED", data3["error_text"])
}

// TestPageEvents verifies navigation, dom_content_loaded, page_load, and dom_updated.
func TestPageEvents(t *testing.T) {
	srv := newFakeCDPServer(t)
	defer srv.close()

	_, published, mu, cleanup := startMonitorWithFakeServer(t, srv)
	defer cleanup()

	// frameNavigated → navigation
	srv.sendToMonitor(t, map[string]any{
		"method": "Page.frameNavigated",
		"params": map[string]any{
			"frame": map[string]any{
				"id":  "frame-1",
				"url": "https://example.com/page",
			},
		},
	})
	ev := waitForEvent(t, published, mu, "navigation", 2*time.Second)
	assert.Equal(t, events.CategoryPage, ev.Category)
	assert.Equal(t, events.KindCDP, ev.Source.Kind)
	assert.Equal(t, "Page.frameNavigated", ev.Source.Event)
	var data map[string]any
	require.NoError(t, json.Unmarshal(ev.Data, &data))
	assert.Equal(t, "https://example.com/page", data["url"])

	// domContentEventFired → dom_content_loaded
	srv.sendToMonitor(t, map[string]any{
		"method": "Page.domContentEventFired",
		"params": map[string]any{"timestamp": 1000.0},
	})
	ev2 := waitForEvent(t, published, mu, "dom_content_loaded", 2*time.Second)
	assert.Equal(t, events.CategoryPage, ev2.Category)

	// loadEventFired → page_load
	srv.sendToMonitor(t, map[string]any{
		"method": "Page.loadEventFired",
		"params": map[string]any{"timestamp": 1001.0},
	})
	ev3 := waitForEvent(t, published, mu, "page_load", 2*time.Second)
	assert.Equal(t, events.CategoryPage, ev3.Category)

	// documentUpdated → dom_updated
	srv.sendToMonitor(t, map[string]any{
		"method": "DOM.documentUpdated",
		"params": map[string]any{},
	})
	ev4 := waitForEvent(t, published, mu, "dom_updated", 2*time.Second)
	assert.Equal(t, events.CategoryPage, ev4.Category)
}

// TestTargetEvents verifies target_created and target_destroyed.
func TestTargetEvents(t *testing.T) {
	srv := newFakeCDPServer(t)
	defer srv.close()

	_, published, mu, cleanup := startMonitorWithFakeServer(t, srv)
	defer cleanup()

	// targetCreated → target_created
	srv.sendToMonitor(t, map[string]any{
		"method": "Target.targetCreated",
		"params": map[string]any{
			"targetInfo": map[string]any{
				"targetId": "target-1",
				"type":     "page",
				"url":      "https://new.example.com",
			},
		},
	})
	ev := waitForEvent(t, published, mu, "target_created", 2*time.Second)
	assert.Equal(t, events.CategoryPage, ev.Category)
	assert.Equal(t, events.KindCDP, ev.Source.Kind)
	assert.Equal(t, "Target.targetCreated", ev.Source.Event)
	var data map[string]any
	require.NoError(t, json.Unmarshal(ev.Data, &data))
	assert.Equal(t, "target-1", data["target_id"])

	// targetDestroyed → target_destroyed
	srv.sendToMonitor(t, map[string]any{
		"method": "Target.targetDestroyed",
		"params": map[string]any{
			"targetId": "target-1",
		},
	})
	ev2 := waitForEvent(t, published, mu, "target_destroyed", 2*time.Second)
	assert.Equal(t, events.CategoryPage, ev2.Category)
	var data2 map[string]any
	require.NoError(t, json.Unmarshal(ev2.Data, &data2))
	assert.Equal(t, "target-1", data2["target_id"])
}

// TestBindingAndTimeline verifies that scroll_settled arrives via
// Runtime.bindingCalled and layout_shift arrives via PerformanceTimeline.
func TestBindingAndTimeline(t *testing.T) {
	srv := newFakeCDPServer(t)
	defer srv.close()

	_, published, mu, cleanup := startMonitorWithFakeServer(t, srv)
	defer cleanup()

	// scroll_settled via Runtime.bindingCalled
	srv.sendToMonitor(t, map[string]any{
		"method": "Runtime.bindingCalled",
		"params": map[string]any{
			"name":    "__kernelEvent",
			"payload": `{"type":"scroll_settled","from_x":0,"from_y":0,"to_x":0,"to_y":500,"target_selector":"body"}`,
		},
	})
	ev := waitForEvent(t, published, mu, "scroll_settled", 2*time.Second)
	assert.Equal(t, events.CategoryInteraction, ev.Category)
	assert.Equal(t, "Runtime.bindingCalled", ev.Source.Event)
	var data map[string]any
	require.NoError(t, json.Unmarshal(ev.Data, &data))
	assert.Equal(t, float64(500), data["to_y"])

	// layout_shift via PerformanceTimeline.timelineEventAdded
	srv.sendToMonitor(t, map[string]any{
		"method": "PerformanceTimeline.timelineEventAdded",
		"params": map[string]any{
			"event": map[string]any{
				"type": "layout-shift",
			},
		},
	})
	ev2 := waitForEvent(t, published, mu, "layout_shift", 2*time.Second)
	assert.Equal(t, events.KindCDP, ev2.Source.Kind)
	assert.Equal(t, "PerformanceTimeline.timelineEventAdded", ev2.Source.Event)

	noEventWithin(t, published, mu, "console_log", 100*time.Millisecond)
}

// TestScreenshot verifies rate limiting and the screenshotFn testable seam.
func TestScreenshot(t *testing.T) {
	srv := newFakeCDPServer(t)
	defer srv.close()

	m, published, mu, cleanup := startMonitorWithFakeServer(t, srv)
	defer cleanup()

	// Inject a mock screenshotFn that returns a tiny valid PNG.
	var captureCount atomic.Int32
	// 1x1 white PNG (minimal valid PNG bytes)
	minimalPNG := []byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, // PNG signature
		0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52, // IHDR chunk length + type
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, // width=1, height=1
		0x08, 0x02, 0x00, 0x00, 0x00, 0x90, 0x77, 0x53, // bit depth=8, color type=2, ...
		0xde, 0x00, 0x00, 0x00, 0x0c, 0x49, 0x44, 0x41, // IDAT chunk
		0x54, 0x08, 0xd7, 0x63, 0xf8, 0xcf, 0xc0, 0x00,
		0x00, 0x00, 0x02, 0x00, 0x01, 0xe2, 0x21, 0xbc,
		0x33, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, // IEND chunk
		0x44, 0xae, 0x42, 0x60, 0x82,
	}
	m.screenshotFn = func(ctx context.Context, displayNum int) ([]byte, error) {
		captureCount.Add(1)
		return minimalPNG, nil
	}

	// First maybeScreenshot call — should capture.
	ctx := context.Background()
	m.maybeScreenshot(ctx)
	// Give the goroutine time to run.
	require.Eventually(t, func() bool {
		return captureCount.Load() == 1
	}, 2*time.Second, 20*time.Millisecond)

	// Second call immediately after — should be rate-limited (no capture).
	m.maybeScreenshot(ctx)
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, int32(1), captureCount.Load(), "second call within 2s should be rate-limited")

	// Verify screenshot event was published with png field.
	ev := waitForEvent(t, published, mu, "screenshot", 2*time.Second)
	assert.Equal(t, events.CategorySystem, ev.Category)
	assert.Equal(t, events.KindLocalProcess, ev.Source.Kind)
	var data map[string]any
	require.NoError(t, json.Unmarshal(ev.Data, &data))
	assert.NotEmpty(t, data["png"])

	// Fast-forward lastScreenshotAt to simulate 2s+ elapsed.
	m.lastScreenshotAt.Store(time.Now().Add(-3 * time.Second).UnixMilli())
	m.maybeScreenshot(ctx)
	require.Eventually(t, func() bool {
		return captureCount.Load() == 2
	}, 2*time.Second, 20*time.Millisecond)
}

// --- Computed meta-event tests ---

// newComputedMonitor creates a Monitor with a capture function and returns
// the published events slice and its mutex for inspection.
func newComputedMonitor(t *testing.T) (*Monitor, *[]events.Event, *sync.Mutex) {
	t.Helper()
	var mu sync.Mutex
	published := make([]events.Event, 0)
	publishFn := func(ev events.Event) {
		mu.Lock()
		published = append(published, ev)
		mu.Unlock()
	}
	upstream := newFakeUpstream("ws://127.0.0.1:0") // not used; no real dial
	m := New(upstream, publishFn, 0)
	return m, &published, &mu
}


// noEventWithin asserts that no event of the given type is published within d.
func noEventWithin(t *testing.T, published *[]events.Event, mu *sync.Mutex, eventType string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		mu.Lock()
		for _, ev := range *published {
			if ev.Type == eventType {
				mu.Unlock()
				t.Fatalf("unexpected event %q published", eventType)
			}
		}
		mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
}

// TestNetworkIdle verifies the 500ms debounce for network_idle.
func TestNetworkIdle(t *testing.T) {
	m, published, mu := newComputedMonitor(t)

	// Simulate navigation (resets computed state).
	navParams, _ := json.Marshal(map[string]any{
		"frame": map[string]any{"id": "f1", "url": "https://example.com"},
	})
	m.handleFrameNavigated(navParams, "s1")
	// Drain the navigation event from published.

	// Helper to send requestWillBeSent.
	sendReq := func(id string) {
		p, _ := json.Marshal(map[string]any{
			"requestId":    id,
			"resourceType": "Document",
			"request":      map[string]any{"method": "GET", "url": "https://example.com/" + id},
		})
		m.handleNetworkRequest(p, "s1")
	}
	// Helper to send loadingFinished.
	sendFinished := func(id string) {
		// store minimal state so LoadAndDelete finds it
		m.pendReqMu.Lock()
			m.pendingRequests[id] = networkReqState{method: "GET", url: "https://example.com/" + id}
			m.pendReqMu.Unlock()
		p, _ := json.Marshal(map[string]any{"requestId": id})
		m.handleLoadingFinished(p, "s1")
	}

	// Send 3 requests, then finish them all.
	sendReq("r1")
	sendReq("r2")
	sendReq("r3")

	t0 := time.Now()
	sendFinished("r1")
	sendFinished("r2")
	sendFinished("r3")

	// network_idle should fire ~500ms after the last loadingFinished.
	ev := waitForEvent(t,published, mu, "network_idle", 2*time.Second)
	elapsed := time.Since(t0)
	assert.GreaterOrEqual(t, elapsed.Milliseconds(), int64(400), "network_idle fired too early")
	assert.Equal(t, events.CategoryNetwork, ev.Category)
	assert.Equal(t, events.KindCDP, ev.Source.Kind)
	assert.Equal(t, "", ev.Source.Event)

	// --- Timer reset test: new request within 500ms resets the clock ---
	m2, published2, mu2 := newComputedMonitor(t)
	navParams2, _ := json.Marshal(map[string]any{
		"frame": map[string]any{"id": "f1", "url": "https://example.com"},
	})
	m2.handleFrameNavigated(navParams2, "s1")

	sendReq2 := func(id string) {
		p, _ := json.Marshal(map[string]any{
			"requestId":    id,
			"resourceType": "Document",
			"request":      map[string]any{"method": "GET", "url": "https://example.com/" + id},
		})
		m2.handleNetworkRequest(p, "s1")
	}
	sendFinished2 := func(id string) {
		m2.pendReqMu.Lock()
			m2.pendingRequests[id] = networkReqState{method: "GET", url: "https://example.com/" + id}
			m2.pendReqMu.Unlock()
		p, _ := json.Marshal(map[string]any{"requestId": id})
		m2.handleLoadingFinished(p, "s1")
	}

	sendReq2("a1")
	sendFinished2("a1")
	// 200ms later, a new request starts (timer should reset)
	time.Sleep(200 * time.Millisecond)
	sendReq2("a2")
	t1 := time.Now()
	sendFinished2("a2")

	ev2 := waitForEvent(t,published2, mu2, "network_idle", 2*time.Second)
	elapsed2 := time.Since(t1)
	// Should fire ~500ms after a2 finished, not 500ms after a1
	assert.GreaterOrEqual(t, elapsed2.Milliseconds(), int64(400), "network_idle should reset timer on new request")
	assert.Equal(t, events.CategoryNetwork, ev2.Category)
}

// TestLayoutSettled verifies the 1s debounce for layout_settled.
func TestLayoutSettled(t *testing.T) {
	m, published, mu := newComputedMonitor(t)

	// Navigate to reset state.
	navParams, _ := json.Marshal(map[string]any{
		"frame": map[string]any{"id": "f1", "url": "https://example.com"},
	})
	m.handleFrameNavigated(navParams, "s1")

	// Simulate page_load (Page.loadEventFired).
	// We bypass the ffmpeg screenshot side-effect by keeping screenshotFn nil-safe.
	t0 := time.Now()
	m.handleLoadEventFired(json.RawMessage(`{}`), "s1")

	// layout_settled should fire ~1s after page_load (no layout shifts).
	ev := waitForEvent(t,published, mu, "layout_settled", 3*time.Second)
	elapsed := time.Since(t0)
	assert.GreaterOrEqual(t, elapsed.Milliseconds(), int64(900), "layout_settled fired too early")
	assert.Equal(t, events.CategoryPage, ev.Category)
	assert.Equal(t, events.KindCDP, ev.Source.Kind)
	assert.Equal(t, "", ev.Source.Event)

	// --- Layout shift resets the timer ---
	m2, published2, mu2 := newComputedMonitor(t)
	navParams2, _ := json.Marshal(map[string]any{
		"frame": map[string]any{"id": "f1", "url": "https://example.com"},
	})
	m2.handleFrameNavigated(navParams2, "s1")
	m2.handleLoadEventFired(json.RawMessage(`{}`), "s1")

	// Simulate a native CDP layout shift at 600ms.
	time.Sleep(600 * time.Millisecond)
	shiftParams, _ := json.Marshal(map[string]any{
		"event": map[string]any{"type": "layout-shift"},
	})
	m2.handleTimelineEvent(shiftParams, "s1")
	t1 := time.Now()

	// layout_settled fires ~1s after the shift, not 1s after page_load.
	ev2 := waitForEvent(t,published2, mu2, "layout_settled", 3*time.Second)
	elapsed2 := time.Since(t1)
	assert.GreaterOrEqual(t, elapsed2.Milliseconds(), int64(900), "layout_settled should reset after layout_shift")
	assert.Equal(t, events.CategoryPage, ev2.Category)
}

// TestScrollSettled verifies that a scroll_settled sentinel from JS is passed through.
func TestScrollSettled(t *testing.T) {
	m, published, mu := newComputedMonitor(t)

	// Simulate scroll_settled via Runtime.bindingCalled.
	bindingParams, _ := json.Marshal(map[string]any{
		"name":    "__kernelEvent",
		"payload": `{"type":"scroll_settled"}`,
	})
	m.handleBindingCalled(bindingParams, "s1")

	ev := waitForEvent(t,published, mu, "scroll_settled", 1*time.Second)
	assert.Equal(t, events.CategoryInteraction, ev.Category)
}

// TestNavigationSettled verifies the three-flag gate for navigation_settled.
func TestNavigationSettled(t *testing.T) {
	m, published, mu := newComputedMonitor(t)

	// Navigate to initialise flags.
	navParams, _ := json.Marshal(map[string]any{
		"frame": map[string]any{"id": "f1", "url": "https://example.com"},
	})
	m.handleFrameNavigated(navParams, "s1")

	// Trigger dom_content_loaded.
	m.handleDOMContentLoaded(json.RawMessage(`{}`), "s1")

	// Trigger network_idle via load cycle.
	reqP, _ := json.Marshal(map[string]any{
		"requestId": "r1", "resourceType": "Document",
		"request": map[string]any{"method": "GET", "url": "https://example.com/r1"},
	})
	m.handleNetworkRequest(reqP, "s1")
	m.pendReqMu.Lock()
	m.pendingRequests["r1"] = networkReqState{method: "GET", url: "https://example.com/r1"}
	m.pendReqMu.Unlock()
	finP, _ := json.Marshal(map[string]any{"requestId": "r1"})
	m.handleLoadingFinished(finP, "s1")

	// Trigger layout_settled via page_load (1s timer).
	m.handleLoadEventFired(json.RawMessage(`{}`), "s1")

	// Wait for navigation_settled (all three flags set).
	ev := waitForEvent(t,published, mu, "navigation_settled", 3*time.Second)
	assert.Equal(t, events.CategoryPage, ev.Category)
	assert.Equal(t, events.KindCDP, ev.Source.Kind)
	assert.Equal(t, "", ev.Source.Event)

	// --- Navigation interrupt test ---
	m2, published2, mu2 := newComputedMonitor(t)

	navP1, _ := json.Marshal(map[string]any{
		"frame": map[string]any{"id": "f1", "url": "https://example.com"},
	})
	m2.handleFrameNavigated(navP1, "s1")

	// Start sequence: dom_content_loaded + network_idle.
	m2.handleDOMContentLoaded(json.RawMessage(`{}`), "s1")
	reqP2, _ := json.Marshal(map[string]any{
		"requestId": "r2", "resourceType": "Document",
		"request": map[string]any{"method": "GET", "url": "https://example.com/r2"},
	})
	m2.handleNetworkRequest(reqP2, "s1")
	m2.pendReqMu.Lock()
	m2.pendingRequests["r2"] = networkReqState{method: "GET", url: "https://example.com/r2"}
	m2.pendReqMu.Unlock()
	finP2, _ := json.Marshal(map[string]any{"requestId": "r2"})
	m2.handleLoadingFinished(finP2, "s1")

	// Interrupt with a new navigation before layout_settled fires.
	navP2, _ := json.Marshal(map[string]any{
		"frame": map[string]any{"id": "f1", "url": "https://example.com/page2"},
	})
	m2.handleFrameNavigated(navP2, "s1")

	// navigation_settled should NOT fire for the interrupted sequence.
	noEventWithin(t, published2, mu2, "navigation_settled", 1500*time.Millisecond)
	_ = mu2 // suppress unused warning
}
