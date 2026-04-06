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

const wsReadLimit = 8 * 1024 * 1024

// Monitor manages a CDP WebSocket connection with auto-attach session fan-out.
type Monitor struct {
	upstreamMgr UpstreamProvider
	publish     PublishFunc
	displayNum  int

	// lifeMu serializes Start, Stop, and restartReadLoop to prevent races on
	// conn, lifecycleCtx, cancel, and done.
	lifeMu sync.Mutex
	conn   *websocket.Conn

	nextID  atomic.Int64
	pendMu  sync.Mutex
	pending map[int64]chan cdpMessage

	sessionsMu sync.RWMutex
	sessions   map[string]targetInfo // sessionID → targetInfo

	pendReqMu       sync.Mutex
	pendingRequests map[string]networkReqState // requestId → networkReqState

	currentURL atomic.Value // last URL from Page.frameNavigated

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

// getLifecycleCtx returns the current lifecycle context under lifeMu.
func (m *Monitor) getLifecycleCtx() context.Context {
	m.lifeMu.Lock()
	ctx := m.lifecycleCtx
	m.lifeMu.Unlock()
	return ctx
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

	// Use background context so the monitor outlives the caller's request context.
	ctx, cancel := context.WithCancel(context.Background())

	conn, _, err := websocket.Dial(ctx, devtoolsURL, nil)
	if err != nil {
		cancel()
		return fmt.Errorf("cdpmonitor: dial %s: %w", devtoolsURL, err)
	}
	conn.SetReadLimit(wsReadLimit)

	m.lifeMu.Lock()
	m.conn = conn
	m.lifecycleCtx = ctx
	m.cancel = cancel
	m.done = make(chan struct{})
	m.lifeMu.Unlock()

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

	m.lifeMu.Lock()
	if m.cancel != nil {
		m.cancel()
	}
	done := m.done
	m.lifeMu.Unlock()

	if done != nil {
		<-done
	}

	m.lifeMu.Lock()
	if m.conn != nil {
		_ = m.conn.Close(websocket.StatusNormalClosure, "stopped")
		m.conn = nil
	}
	m.lifeMu.Unlock()

	m.clearState()
}

// clearState resets sessions, pending requests, and computed state.
func (m *Monitor) clearState() {
	m.currentURL.Store("")

	m.sessionsMu.Lock()
	m.sessions = make(map[string]targetInfo)
	m.sessionsMu.Unlock()

	m.pendReqMu.Lock()
	m.pendingRequests = make(map[string]networkReqState)
	m.pendReqMu.Unlock()

	m.computed.resetOnNavigation()
}

// readLoop reads CDP messages, routing responses to pending callers and dispatching events.
func (m *Monitor) readLoop(ctx context.Context) {
	m.lifeMu.Lock()
	done := m.done
	conn := m.conn
	m.lifeMu.Unlock()
	defer close(done)

	if conn == nil {
		return
	}

	for {
		_, b, err := conn.Read(ctx)
		if err != nil {
			return
		}

		var msg cdpMessage
		if err := json.Unmarshal(b, &msg); err != nil {
			continue
		}

		if msg.ID != nil {
			m.pendMu.Lock()
			ch, ok := m.pending[*msg.ID]
			m.pendMu.Unlock()
			if ok {
				select {
				case ch <- msg:
				default:
					// send() already timed out and deregistered; discard.
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

	req := cdpMessage{ID: &id, Method: method, Params: rawParams, SessionID: sessionID}
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

	m.lifeMu.Lock()
	conn := m.conn
	m.lifeMu.Unlock()
	if conn == nil {
		return nil, fmt.Errorf("cdpmonitor: connection not open")
	}

	// coder/websocket allows concurrent Read + Write on the same Conn.
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

// initSession enables CDP domains, injects the interaction-tracking script,
// and manually attaches to any targets already open when the monitor started.
func (m *Monitor) initSession(ctx context.Context) {
	_, _ = m.send(ctx, "Target.setAutoAttach", map[string]any{
		"autoAttach":             true,
		"waitForDebuggerOnStart": false,
		"flatten":                true,
	}, "")
	m.enableDomains(ctx, "")
	_ = m.injectScript(ctx, "")
	m.attachExistingTargets(ctx)
}

// attachExistingTargets fetches all open targets and attaches to any that are
// not already tracked. This catches pages that were open before Start() was called.
func (m *Monitor) attachExistingTargets(ctx context.Context) {
	result, err := m.send(ctx, "Target.getTargets", nil, "")
	if err != nil {
		return
	}
	var resp struct {
		TargetInfos []cdpTargetInfo `json:"targetInfos"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		return
	}
	for _, ti := range resp.TargetInfos {
		if ti.Type != "page" {
			continue
		}
		m.sessionsMu.RLock()
		alreadyAttached := false
		for _, info := range m.sessions {
			if info.targetID == ti.TargetID {
				alreadyAttached = true
				break
			}
		}
		m.sessionsMu.RUnlock()
		if alreadyAttached {
			continue
		}
		go func(targetID string) {
			res, err := m.send(ctx, "Target.attachToTarget", map[string]any{
				"targetId": targetID,
				"flatten":  true,
			}, "")
			if err != nil {
				return
			}
			var attached struct {
				SessionID string `json:"sessionId"`
			}
			if json.Unmarshal(res, &attached) == nil && attached.SessionID != "" {
				m.enableDomains(ctx, attached.SessionID)
				_ = m.injectScript(ctx, attached.SessionID)
			}
		}(ti.TargetID)
	}
}

// restartReadLoop waits for the current readLoop to exit, then starts a new one.
func (m *Monitor) restartReadLoop(ctx context.Context) {
	m.lifeMu.Lock()
	done := m.done
	m.lifeMu.Unlock()

	<-done

	m.lifeMu.Lock()
	m.done = make(chan struct{})
	m.lifeMu.Unlock()

	go m.readLoop(ctx)
}

// subscribeToUpstream reconnects with backoff on Chrome restarts, publishing disconnect/reconnect events.
func (m *Monitor) subscribeToUpstream(ctx context.Context) {
	ch, cancel := m.upstreamMgr.Subscribe()
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			return
		case newURL, ok := <-ch:
			if !ok {
				return
			}
			m.handleUpstreamRestart(ctx, newURL)
		}
	}
}

// handleUpstreamRestart tears down the old connection, reconnects with backoff,
// and re-initializes the CDP session.
func (m *Monitor) handleUpstreamRestart(ctx context.Context, newURL string) {
	m.publish(events.Event{
		Ts:          time.Now().UnixMilli(),
		Type:        EventMonitorDisconnected,
		Category:    events.CategorySystem,
		Source:      events.Source{Kind: events.KindLocalProcess},
		DetailLevel: events.DetailMinimal,
		Data:        json.RawMessage(`{"reason":"chrome_restarted"}`),
	})

	startReconnect := time.Now()

	m.lifeMu.Lock()
	if m.conn != nil {
		_ = m.conn.Close(websocket.StatusNormalClosure, "reconnecting")
		m.conn = nil
	}
	m.lifeMu.Unlock()

	m.clearState()

	if !m.reconnectWithBackoff(ctx, newURL) {
		return
	}

	m.restartReadLoop(ctx)
	go m.initSession(ctx)

	m.publish(events.Event{
		Ts:          time.Now().UnixMilli(),
		Type:        EventMonitorReconnected,
		Category:    events.CategorySystem,
		Source:      events.Source{Kind: events.KindLocalProcess},
		DetailLevel: events.DetailMinimal,
		Data: json.RawMessage(fmt.Sprintf(
			`{"reconnect_duration_ms":%d}`,
			time.Since(startReconnect).Milliseconds(),
		)),
	})
}

var reconnectBackoffs = []time.Duration{
	250 * time.Millisecond,
	500 * time.Millisecond,
	1 * time.Second,
	2 * time.Second,
}

// reconnectWithBackoff attempts to dial newURL up to 10 times with exponential backoff.
func (m *Monitor) reconnectWithBackoff(ctx context.Context, newURL string) bool {
	for attempt := range 10 {
		if ctx.Err() != nil {
			return false
		}

		idx := min(attempt, len(reconnectBackoffs)-1)
		select {
		case <-ctx.Done():
			return false
		case <-time.After(reconnectBackoffs[idx]):
		}

		conn, _, err := websocket.Dial(ctx, newURL, nil)
		if err != nil {
			continue
		}
		conn.SetReadLimit(wsReadLimit)

		m.lifeMu.Lock()
		m.conn = conn
		m.lifeMu.Unlock()
		return true
	}
	return false
}
