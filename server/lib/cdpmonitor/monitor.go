// Package cdpmonitor opens an isolated DevTools WebSocket connection to Chrome
// and subscribes to all CDP domains, publishing BrowserEvent structs to the
// event pipeline.
package cdpmonitor

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/onkernel/kernel-images/server/lib/events"
)

// UpstreamProvider is satisfied by *devtoolsproxy.UpstreamManager.
// Extracted as an interface so tests can supply a fake.
type UpstreamProvider interface {
	Current() string
	Subscribe() (<-chan string, func())
}

// PublishFunc is a function that publishes a BrowserEvent to the pipeline.
// Using a callback rather than a direct *events.Pipeline dependency makes
// unit tests straightforward — tests supply a closure.
type PublishFunc func(ev events.BrowserEvent)

// targetInfo holds metadata about an attached CDP target/session.
type targetInfo struct {
	targetID   string
	url        string
	targetType string
}

// cdpError is the JSON-RPC error object returned by Chrome.
type cdpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *cdpError) Error() string {
	return fmt.Sprintf("CDP error %d: %s", e.Code, e.Message)
}

// cdpMessage is the JSON-RPC message envelope used by Chrome's DevTools Protocol.
type cdpMessage struct {
	ID        int64           `json:"id,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *cdpError       `json:"error,omitempty"`
}

// Monitor opens its own isolated WebSocket connection to Chrome and manages
// CDP session fan-out via Target.setAutoAttach with flatten:true.
type Monitor struct {
	upstreamMgr UpstreamProvider
	publish     PublishFunc
	displayNum  int

	conn   *websocket.Conn
	connMu sync.Mutex

	nextID  atomic.Int64
	pending sync.Map // int64 → chan cdpMessage

	sessionsMu sync.RWMutex
	sessions   map[string]targetInfo // sessionID → targetInfo

	pendingRequests sync.Map // requestId string → networkReqState

	computed *computedState

	lastScreenshotAt atomic.Int64 // unix millis of last screenshot capture
	// screenshotFn is a testable seam for screenshot capture.
	// If nil, captureScreenshot falls through to the real ffmpeg command.
	screenshotFn func(ctx context.Context, displayNum int) ([]byte, error)

	cancel context.CancelFunc
	done   chan struct{}

	running atomic.Bool
}

// New creates a new Monitor.
// upstreamMgr provides the current Chrome DevTools URL and restart notifications.
// publish is called for every BrowserEvent the monitor emits.
// displayNum is the X display number used for ffmpeg x11grab screenshots.
func New(upstreamMgr UpstreamProvider, publish PublishFunc, displayNum int) *Monitor {
	m := &Monitor{
		upstreamMgr: upstreamMgr,
		publish:     publish,
		displayNum:  displayNum,
		sessions:    make(map[string]targetInfo),
	}
	m.computed = newComputedState(publish)
	return m
}

// IsRunning reports whether the monitor is currently capturing.
func (m *Monitor) IsRunning() bool {
	return m.running.Load()
}

// Start begins CDP capture. If already running, Stop is called first
// (stop+restart semantics).
func (m *Monitor) Start(parentCtx context.Context) error {
	if m.running.Load() {
		m.Stop()
	}

	devtoolsURL := m.upstreamMgr.Current()
	if devtoolsURL == "" {
		return fmt.Errorf("cdpmonitor: no DevTools URL available")
	}

	conn, _, err := websocket.Dial(parentCtx, devtoolsURL, nil)
	if err != nil {
		return fmt.Errorf("cdpmonitor: dial %s: %w", devtoolsURL, err)
	}
	conn.SetReadLimit(8 * 1024 * 1024)

	m.connMu.Lock()
	m.conn = conn
	m.connMu.Unlock()

	ctx, cancel := context.WithCancel(parentCtx)
	m.cancel = cancel
	m.done = make(chan struct{})

	m.running.Store(true)

	go m.readLoop(ctx)
	go m.subscribeToUpstream(ctx)
	// initSession must run after readLoop is started so responses can be routed.
	go m.initSession(ctx)

	return nil
}

// Stop cancels the context and waits for all goroutines to exit.
func (m *Monitor) Stop() {
	if !m.running.Swap(false) {
		return
	}
	if m.cancel != nil {
		m.cancel()
	}
	if m.done != nil {
		<-m.done
	}
	m.connMu.Lock()
	if m.conn != nil {
		_ = m.conn.Close(websocket.StatusNormalClosure, "stopped")
		m.conn = nil
	}
	m.connMu.Unlock()

	m.sessionsMu.Lock()
	m.sessions = make(map[string]targetInfo)
	m.sessionsMu.Unlock()

	// Reset computed state to stop any in-flight timers.
	m.computed.resetOnNavigation()
}

// readLoop is the single goroutine that reads from the CDP WebSocket.
// It routes responses to pending callers and dispatches events.
func (m *Monitor) readLoop(ctx context.Context) {
	defer close(m.done)

	for {
		m.connMu.Lock()
		conn := m.conn
		m.connMu.Unlock()
		if conn == nil {
			return
		}

		_, b, err := conn.Read(ctx)
		if err != nil {
			return
		}

		var msg cdpMessage
		if err := json.Unmarshal(b, &msg); err != nil {
			continue
		}

		if msg.ID != 0 {
			// Route response to the pending send() caller.
			if ch, ok := m.pending.Load(msg.ID); ok {
				select {
				case ch.(chan cdpMessage) <- msg:
				default:
				}
			}
			continue
		}

		m.dispatchEvent(msg)
	}
}

// send sends a CDP command and waits for the response.
// It registers the response channel BEFORE writing the request.
func (m *Monitor) send(ctx context.Context, method string, params any, sessionID string) (json.RawMessage, error) {
	id := m.nextID.Add(1)

	var rawParams json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("marshal params: %w", err)
		}
		rawParams = b
	}

	req := cdpMessage{ID: id, Method: method, Params: rawParams, SessionID: sessionID}
	reqBytes, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	ch := make(chan cdpMessage, 1)
	m.pending.Store(id, ch)
	defer m.pending.Delete(id)

	m.connMu.Lock()
	conn := m.conn
	m.connMu.Unlock()
	if conn == nil {
		return nil, fmt.Errorf("cdpmonitor: connection not open")
	}

	if err := conn.Write(ctx, websocket.MessageText, reqBytes); err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}

	select {
	case resp := <-ch:
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// initSession enables CDP domains on a fresh connection. It is always called
// asynchronously so the readLoop is already running to route responses.
func (m *Monitor) initSession(ctx context.Context) {
	_, _ = m.send(ctx, "Target.setAutoAttach", map[string]any{
		"autoAttach":             true,
		"waitForDebuggerOnStart": false,
		"flatten":                true,
	}, "")
	m.enableDomains(ctx, "")
}

// subscribeToUpstream watches UpstreamManager for Chrome restart notifications.
// On a new URL it emits monitor_disconnected, reconnects with backoff, then
// emits monitor_reconnected.
func (m *Monitor) subscribeToUpstream(ctx context.Context) {
	ch, cancel := m.upstreamMgr.Subscribe()
	defer cancel()

	backoffs := []time.Duration{
		250 * time.Millisecond,
		500 * time.Millisecond,
		1 * time.Second,
		2 * time.Second,
	}

	for {
		select {
		case <-ctx.Done():
			return
		case newURL, ok := <-ch:
			if !ok {
				return
			}
			// Publish disconnect event.
			m.publish(events.BrowserEvent{
				Ts:          time.Now().UnixMilli(),
				Type:        "monitor_disconnected",
				Category:    events.CategorySystem,
				Source:      events.SourceLocalProcess,
				DetailLevel: events.DetailMinimal,
				Data:        json.RawMessage(`{"reason":"chrome_restarted"}`),
			})

			startReconnect := time.Now()

			// Close old connection.
			m.connMu.Lock()
			if m.conn != nil {
				_ = m.conn.Close(websocket.StatusNormalClosure, "reconnecting")
				m.conn = nil
			}
			m.connMu.Unlock()

			// Reconnect with backoff.
			var reconnErr error
			for attempt := range 10 {
				if ctx.Err() != nil {
					return
				}

				idx := min(attempt, len(backoffs)-1)
				select {
				case <-ctx.Done():
					return
				case <-time.After(backoffs[idx]):
				}

				conn, _, err := websocket.Dial(ctx, newURL, nil)
				if err != nil {
					reconnErr = err
					continue
				}
				conn.SetReadLimit(8 * 1024 * 1024)

				m.connMu.Lock()
				m.conn = conn
				m.connMu.Unlock()

				reconnErr = nil
				break
			}

			if reconnErr != nil {
				// All reconnect attempts failed; stay disconnected.
				return
			}

			// Re-initialize asynchronously so reconnected event is published
			// before waiting for setAutoAttach / domain enable responses.
			go m.initSession(ctx)

			// Publish reconnected event.
			m.publish(events.BrowserEvent{
				Ts:          time.Now().UnixMilli(),
				Type:        "monitor_reconnected",
				Category:    events.CategorySystem,
				Source:      events.SourceLocalProcess,
				DetailLevel: events.DetailMinimal,
				Data: json.RawMessage(fmt.Sprintf(
					`{"reconnect_duration_ms":%d}`,
					time.Since(startReconnect).Milliseconds(),
				)),
			})
		}
	}
}
