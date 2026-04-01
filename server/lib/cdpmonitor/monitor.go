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

// UpstreamProvider abstracts *devtoolsproxy.UpstreamManager for testability.
type UpstreamProvider interface {
	Current() string
	Subscribe() (<-chan string, func())
}

// PublishFunc publishes an Event to the pipeline.
type PublishFunc func(ev events.Event)

// Monitor manages a CDP WebSocket connection with auto-attach session fan-out.
type Monitor struct {
	upstreamMgr UpstreamProvider
	publish     PublishFunc
	displayNum  int

	conn   *websocket.Conn
	connMu sync.Mutex

	nextID   atomic.Int64
	pendMu   sync.Mutex
	pending  map[int64]chan cdpMessage

	sessionsMu sync.RWMutex
	sessions   map[string]targetInfo // sessionID → targetInfo

	pendReqMu       sync.Mutex
	pendingRequests map[string]networkReqState // requestId → networkReqState

	computed *computedState

	lastScreenshotAt atomic.Int64 // unix millis of last capture
	screenshotFn     func(ctx context.Context, displayNum int) ([]byte, error) // nil → real ffmpeg

	lifecycleCtx context.Context    // cancelled on Stop()
	cancel       context.CancelFunc
	done         chan struct{}

	running atomic.Bool
}

// New creates a Monitor. displayNum is the X display for ffmpeg screenshots.
func New(upstreamMgr UpstreamProvider, publish PublishFunc, displayNum int) *Monitor {
	m := &Monitor{
		upstreamMgr:     upstreamMgr,
		publish:         publish,
		displayNum:      displayNum,
		sessions:        make(map[string]targetInfo),
		pending:         make(map[int64]chan cdpMessage),
		pendingRequests: make(map[string]networkReqState),
	}
	m.computed = newComputedState(publish)
	m.lifecycleCtx = context.Background()
	return m
}

// IsRunning reports whether the monitor is actively capturing.
func (m *Monitor) IsRunning() bool {
	return m.running.Load()
}

// Start begins CDP capture. Restarts if already running.
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
	m.lifecycleCtx = ctx
	m.cancel = cancel
	m.done = make(chan struct{})

	m.running.Store(true)

	go m.readLoop(ctx)
	go m.subscribeToUpstream(ctx)
	go m.initSession(ctx) // must run after readLoop starts

	return nil
}

// Stop cancels the context and waits for goroutines to exit.
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

	m.pendReqMu.Lock()
	m.pendingRequests = make(map[string]networkReqState)
	m.pendReqMu.Unlock()

	m.computed.resetOnNavigation()
}

// readLoop reads CDP messages, routing responses to pending callers and
// dispatching events. Exits on connection close; respawned on reconnect.
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
			m.pendMu.Lock()
			ch, ok := m.pending[msg.ID]
			m.pendMu.Unlock()
			if ok {
				select {
				case ch <- msg:
				default:
				}
			}
			continue
		}

		m.dispatchEvent(msg)
	}
}

// send issues a CDP command and blocks until the response arrives.
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
	m.pendMu.Lock()
	m.pending[id] = ch
	m.pendMu.Unlock()
	defer func() {
		m.pendMu.Lock()
		delete(m.pending, id)
		m.pendMu.Unlock()
	}()

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

// initSession enables CDP domains and injects the interaction-tracking script
// on a fresh connection (called async).
func (m *Monitor) initSession(ctx context.Context) {
	_, _ = m.send(ctx, "Target.setAutoAttach", map[string]any{
		"autoAttach":             true,
		"waitForDebuggerOnStart": false,
		"flatten":                true,
	}, "")
	m.enableDomains(ctx, "")
	_ = m.injectScript(ctx, "")
}

// restartReadLoop waits for the old readLoop to exit, then spawns a new one.
func (m *Monitor) restartReadLoop(ctx context.Context) {
	<-m.done
	m.done = make(chan struct{})
	go m.readLoop(ctx)
}

// subscribeToUpstream reconnects with backoff on Chrome restarts, emitting
// monitor_disconnected / monitor_reconnected events.
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
			m.publish(events.Event{
				Ts:          time.Now().UnixMilli(),
				Type:        "monitor_disconnected",
				Category:    events.CategorySystem,
				Source:      events.Source{Kind: events.KindLocalProcess},
				DetailLevel: events.DetailMinimal,
				Data:        json.RawMessage(`{"reason":"chrome_restarted"}`),
			})

			startReconnect := time.Now()

			m.connMu.Lock()
			if m.conn != nil {
				_ = m.conn.Close(websocket.StatusNormalClosure, "reconnecting")
				m.conn = nil
			}
			m.connMu.Unlock()

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
				return
			}

			m.restartReadLoop(ctx)
			go m.initSession(ctx)

			m.publish(events.Event{
				Ts:          time.Now().UnixMilli(),
				Type:        "monitor_reconnected",
				Category:    events.CategorySystem,
				Source:      events.Source{Kind: events.KindLocalProcess},
				DetailLevel: events.DetailMinimal,
				Data: json.RawMessage(fmt.Sprintf(
					`{"reconnect_duration_ms":%d}`,
					time.Since(startReconnect).Milliseconds(),
				)),
			})
		}
	}
}
