package cdpmonitor

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/kernel/kernel-images/server/lib/events"
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
// WebSocket concurrency: coder/websocket guarantees that one concurrent Read
// and one concurrent Write are safe. The readLoop holds the sole Read; all
// writes go through send, which serialises them with conn.Write's internal
// lock. No external write mutex is needed.
type Monitor struct {
	upstreamMgr UpstreamProvider
	publish     PublishFunc
	displayNum  int
	log         *slog.Logger

	// lifeMu serializes Start, Stop, and restartReadLoop to prevent races on
	// conn, lifecycleCtx, cancel, and done.
	lifeMu sync.Mutex
	conn   *websocket.Conn

	nextID  atomic.Int64
	pendMu  sync.Mutex
	pending map[int64]chan cdpMessage

	sessionsMu    sync.RWMutex
	sessions      map[string]targetInfo // sessionID → targetInfo
	mainSessionID atomic.Value          // string; set on first top-level frameNavigated, cleared on reconnect

	pendReqMu       sync.Mutex
	pendingRequests map[string]networkReqState // requestId → networkReqState

	computedStates map[string]*computedState // sessionID → state machine; guarded by sessionsMu

	lastScreenshotAt   atomic.Int64                                              // unix millis of last capture
	screenshotInFlight atomic.Bool                                               // true while a captureScreenshot goroutine is running
	screenshotFn       func(ctx context.Context, displayNum int) ([]byte, error) // nil → real ffmpeg

	// bindingRateMu guards bindingLastSeen.
	bindingRateMu   sync.Mutex
	bindingLastSeen map[string]time.Time // sessionID → last accepted binding event time

	// asyncWg tracks all goroutines except readLoop (which is tracked via done).
	// subscribeToUpstream and sweepPendingRequests are included so Stop() can
	// wait for them to exit before returning.
	asyncWg   sync.WaitGroup
	restartMu sync.Mutex // serializes handleUpstreamRestart to prevent overlapping reconnects

	lifecycleCtx context.Context // cancelled on Stop()
	cancel       context.CancelFunc
	done         chan struct{}
	readReady    chan struct{} // closed when readLoop has started reading

	running atomic.Bool
}

// New creates a Monitor. displayNum is the X display for ffmpeg screenshots.
func New(upstreamMgr UpstreamProvider, publish PublishFunc, displayNum int, log *slog.Logger) *Monitor {
	m := &Monitor{
		upstreamMgr:     upstreamMgr,
		publish:         publish,
		displayNum:      displayNum,
		log:             log,
		sessions:        make(map[string]targetInfo),
		computedStates:  make(map[string]*computedState),
		pending:         make(map[int64]chan cdpMessage),
		pendingRequests: make(map[string]networkReqState),
		bindingLastSeen: make(map[string]time.Time),
	}
	m.lifecycleCtx = context.Background()
	m.mainSessionID.Store(mainSessionUnset)
	return m
}

// IsRunning reports whether the monitor is actively capturing.
func (m *Monitor) IsRunning() bool {
	return m.running.Load()
}

// Start begins CDP capture. Restarts if already running.
// Not concurrency-safe; callers must serialize Start calls.
func (m *Monitor) Start(ctx context.Context) error {
	m.Stop() // no-op if not running

	devtoolsURL := m.upstreamMgr.Current()
	if devtoolsURL == "" {
		return fmt.Errorf("cdpmonitor: no DevTools URL available")
	}

	ctx, cancel := context.WithCancel(ctx)

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
	m.readReady = make(chan struct{})
	m.lifeMu.Unlock()

	m.running.Store(true)
	m.log.Info("cdpmonitor: started", "url", devtoolsURL)

	go m.readLoop(ctx)
	m.asyncWg.Go(func() { m.subscribeToUpstream(ctx) })
	m.asyncWg.Go(func() { m.sweepPendingRequests(ctx) })
	m.asyncWg.Go(func() { m.initSession(ctx) })

	return nil
}

// Stop cancels the context and waits for goroutines to exit.
func (m *Monitor) Stop() {
	if !m.running.Swap(false) {
		m.lifeMu.Lock()
		cancel := m.cancel
		m.lifeMu.Unlock()
		if cancel != nil {
			cancel()
		}
		m.asyncWg.Wait()
		return
	}
	m.log.Info("cdpmonitor: stopping")

	m.lifeMu.Lock()
	if m.cancel != nil {
		m.cancel()
	}
	done := m.done
	m.lifeMu.Unlock()

	if done != nil {
		<-done
	}

	// Wait for all in-flight async goroutines (fetchResponseBody, enableDomains,
	// screenshots) to finish before closing the connection they may be writing to.
	m.asyncWg.Wait()

	m.lifeMu.Lock()
	if m.conn != nil {
		_ = m.conn.Close(websocket.StatusNormalClosure, "stopped")
		m.conn = nil
	}
	m.lifeMu.Unlock()

	m.clearState()
	m.log.Info("cdpmonitor: stopped")
}

// clearState resets sessions, pending requests, and computed state.
// It also fails all in-flight send() calls so their goroutines are unblocked.
func (m *Monitor) clearState() {
	m.sessionsMu.Lock()
	prev := m.computedStates
	m.sessions = make(map[string]targetInfo)
	m.computedStates = make(map[string]*computedState)
	m.sessionsMu.Unlock()
	for _, cs := range prev {
		cs.stop()
	}
	m.mainSessionID.Store(mainSessionUnset)

	m.pendReqMu.Lock()
	m.pendingRequests = make(map[string]networkReqState)
	m.pendReqMu.Unlock()

	m.bindingRateMu.Lock()
	m.bindingLastSeen = make(map[string]time.Time)
	m.bindingRateMu.Unlock()

	m.failPendingCommands()
}

const pendingRequestTTL = 5 * time.Minute
const sweepInterval = 1 * time.Minute

// sweepPendingRequests periodically evicts networkReqState entries that have
// been in the map for longer than pendingRequestTTL. This bounds map growth on
// long-lived SPAs where loadingFinished never arrives (e.g. tabs closed mid-flight).
// It exits when ctx is cancelled (Stop/reconnect).
func (m *Monitor) sweepPendingRequests(ctx context.Context) {
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			var toSweep []networkReqState
			m.pendReqMu.Lock()
			for id, state := range m.pendingRequests {
				if now.Sub(state.addedAt) > pendingRequestTTL {
					delete(m.pendingRequests, id)
					toSweep = append(toSweep, state)
				}
			}
			m.pendReqMu.Unlock()
			for _, state := range toSweep {
				if cs := m.computedFor(state.sessionID); cs != nil {
					cs.onLoadingFinished()
				}
			}
		}
	}
}

// computedFor returns the computedState for the given sessionID, or nil if none exists.
func (m *Monitor) computedFor(sessionID string) *computedState {
	m.sessionsMu.RLock()
	cs := m.computedStates[sessionID]
	m.sessionsMu.RUnlock()
	return cs
}

// failPendingCommands unblocks all in-flight send() calls by delivering an
// error response. This prevents goroutine leaks when the connection is torn
// down during reconnect.
func (m *Monitor) failPendingCommands() {
	m.pendMu.Lock()
	old := m.pending
	m.pending = make(map[int64]chan cdpMessage)
	m.pendMu.Unlock()

	disconnectErr := &cdpError{Code: -1, Message: "connection closed"}
	for _, ch := range old {
		select {
		case ch <- cdpMessage{Error: disconnectErr}:
		default:
		}
	}
}

// readLoop reads CDP messages, routing responses to pending callers and dispatching events.
// On read error (WS drop) this goroutine returns. Reconnection is driven by
// subscribeToUpstream: the UpstreamProvider always pushes a fresh devtools URL
// when the browser process restarts, so same-URL redial is not needed here.
func (m *Monitor) readLoop(ctx context.Context) {
	m.lifeMu.Lock()
	done := m.done
	conn := m.conn
	readReady := m.readReady
	m.lifeMu.Unlock()
	defer close(done)

	if conn == nil {
		return
	}

	// Signal that readLoop is ready to receive responses.
	close(readReady)

	for {
		_, b, err := conn.Read(ctx)
		if err != nil {
			if ctx.Err() == nil {
				m.log.Warn("cdpmonitor: read loop exiting on unexpected error", "err", err)
			}
			return
		}

		var msg cdpMessage
		if err := json.Unmarshal(b, &msg); err != nil {
			m.log.Warn("cdpmonitor: dropping malformed CDP message", "err", err)
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

const sendTimeout = 30 * time.Second

// send issues a CDP command and blocks until the response arrives.
// A 30 s deadline is applied so a non-responsive Chrome cannot stall
// callers indefinitely; the caller's own deadline (if shorter) wins.
func (m *Monitor) send(ctx context.Context, method string, params any, sessionID string) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()

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
// It waits for readLoop to be ready before sending any commands.
func (m *Monitor) initSession(ctx context.Context) {
	m.lifeMu.Lock()
	readReady := m.readReady
	m.lifeMu.Unlock()
	select {
	case <-readReady:
	case <-ctx.Done():
		return
	}

	if _, err := m.send(ctx, "Target.setAutoAttach", map[string]any{
		"autoAttach":             true,
		"waitForDebuggerOnStart": false,
		"flatten":                true,
	}, ""); err != nil && ctx.Err() == nil {
		// Without auto-attach the monitor will never see new targets: treat as fatal.
		m.log.Error("cdpmonitor: Target.setAutoAttach failed — monitor will not observe new targets", "err", err)
		m.publish(events.Event{
			Ts:       time.Now().UnixMicro(),
			Type:     EventMonitorInitFailed,
			Category: events.CategorySystem,
			Source:   events.Source{Kind: events.KindLocalProcess},
			Data:     json.RawMessage(`{"step":"Target.setAutoAttach"}`),
		})
		return
	}

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
		TargetInfos []cdpTargetTargetInfo `json:"targetInfos"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		return
	}
	m.sessionsMu.RLock()
	attached := make(map[string]bool, len(m.sessions))
	for _, info := range m.sessions {
		attached[info.targetID] = true
	}
	m.sessionsMu.RUnlock()

	for _, ti := range resp.TargetInfos {
		if ti.Type != targetTypePage || attached[ti.TargetID] {
			continue
		}
		targetID := ti.TargetID
		m.asyncWg.Go(func() {
			_, _ = m.send(ctx, "Target.attachToTarget", map[string]any{
				"targetId": targetID,
				"flatten":  true,
			}, "")
		})
	}
}

// restartReadLoop waits for the current readLoop to exit, then starts a new one.
// Returns false if the context was cancelled before the restart completed.
func (m *Monitor) restartReadLoop(ctx context.Context) bool {
	m.lifeMu.Lock()
	done := m.done
	m.lifeMu.Unlock()

	// Wait for old readLoop, but bail if context is cancelled (e.g. Stop called).
	select {
	case <-done:
	case <-ctx.Done():
		return false
	}

	m.lifeMu.Lock()
	m.done = make(chan struct{})
	m.readReady = make(chan struct{})
	m.lifeMu.Unlock()

	go m.readLoop(ctx)
	return true
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
// and re-initializes the CDP session. Serialized by restartMu to prevent
// overlapping reconnects from rapid successive Chrome restarts.
func (m *Monitor) handleUpstreamRestart(ctx context.Context, newURL string) {
	m.restartMu.Lock()
	defer m.restartMu.Unlock()

	if ctx.Err() != nil {
		return
	}
	m.publish(events.Event{
		Ts:       time.Now().UnixMicro(),
		Type:     EventMonitorDisconnected,
		Category: events.CategorySystem,
		Source:   events.Source{Kind: events.KindLocalProcess},
		Data:     json.RawMessage(`{"reason":"` + ReasonChromeRestarted + `"}`),
	})

	startReconnect := time.Now()

	m.lifeMu.Lock()
	if m.conn != nil {
		_ = m.conn.Close(websocket.StatusNormalClosure, "reconnecting")
		m.conn = nil
	}
	m.lifeMu.Unlock()

	if !m.reconnectWithBackoff(ctx, newURL) {
		// Context cancelled means Stop() was called, not a failure.
		if ctx.Err() == nil {
			// Cancel the lifecycle context before setting running=false so that
			// goroutines blocked on ctx.Done() begin exiting. If we set
			// running=false first, a concurrent Stop() call returns immediately
			// without cancelling, permanently orphaning those goroutines in asyncWg.
			m.lifeMu.Lock()
			if m.cancel != nil {
				m.cancel()
			}
			m.lifeMu.Unlock()
			m.clearState()
			m.running.Store(false)
			m.publish(events.Event{
				Ts:       time.Now().UnixMicro(),
				Type:     EventMonitorReconnectFailed,
				Category: events.CategorySystem,
				Source:   events.Source{Kind: events.KindLocalProcess},
				Data:     json.RawMessage(`{"reason":"` + ReasonReconnectExhausted + `"}`),
			})
		}
		return
	}

	// restartReadLoop waits for the old readLoop to exit before returning,
	// so clearState runs only after the old loop has stopped touching shared state.
	if !m.restartReadLoop(ctx) {
		return
	}
	m.clearState()

	m.asyncWg.Go(func() { m.initSession(ctx) })
	reconnectDurationMs := time.Since(startReconnect).Milliseconds()
	m.log.Info("cdpmonitor: reconnected", "url", newURL, "duration_ms", reconnectDurationMs)

	m.publish(events.Event{
		Ts:       time.Now().UnixMicro(),
		Type:     EventMonitorReconnected,
		Category: events.CategorySystem,
		Source:   events.Source{Kind: events.KindLocalProcess},
		Data:     json.RawMessage(fmt.Sprintf(`{"reconnect_duration_ms":%d}`, reconnectDurationMs)),
	})
}

const maxReconnectAttempts = 10

var reconnectBackoffs = []time.Duration{
	250 * time.Millisecond,
	500 * time.Millisecond,
	1 * time.Second,
	2 * time.Second,
}

// reconnectWithBackoff attempts to dial newURL up to maxReconnectAttempts times with exponential backoff.
func (m *Monitor) reconnectWithBackoff(ctx context.Context, newURL string) bool {
	for attempt := range maxReconnectAttempts {
		if ctx.Err() != nil {
			return false
		}

		if attempt > 0 {
			idx := min(attempt-1, len(reconnectBackoffs)-1)
			select {
			case <-ctx.Done():
				return false
			case <-time.After(reconnectBackoffs[idx]):
			}
		}

		conn, _, err := websocket.Dial(ctx, newURL, nil)
		if err != nil {
			m.log.Warn("cdpmonitor: reconnect attempt failed", "attempt", attempt+1, "max_attempts", maxReconnectAttempts, "url", newURL, "err", err)
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
