package cdpmonitor

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kernel/kernel-images/server/lib/browsersurface"
	"github.com/kernel/kernel-images/server/lib/cdpclient"
	"github.com/kernel/kernel-images/server/lib/events"
	oapi "github.com/kernel/kernel-images/server/lib/oapi"
)

// UpstreamProvider abstracts *devtoolsproxy.UpstreamManager for testability.
type UpstreamProvider interface {
	Current() string
	Subscribe() (<-chan string, func())
}

// PublishFunc publishes an Event to the pipeline. Production callers wire
// this to TelemetrySession.Publish; cdpmonitor itself ignores the returns.
type PublishFunc func(ev events.Event) (events.Envelope, bool)

type monitorConnection struct {
	protocol *cdpclient.Client
	surface  *browsersurface.Tracker
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
}

// Monitor owns its CDP connection and browser-surface tracker independently of
// other consumers such as WebMCP. A new pair is created on each reconnect.
type Monitor struct {
	upstreamMgr UpstreamProvider
	publish     PublishFunc
	displayNum  int
	log         *slog.Logger

	lifeMu sync.Mutex
	conn   *monitorConnection

	sessionsMu    sync.RWMutex
	sessions      map[string]targetInfo // sessionID → targetInfo
	mainSessionID atomic.Value          // string; set on first top-level frameNavigated, cleared on reconnect

	pendReqMu       sync.Mutex
	pendingRequests map[networkRequestKey]networkReqState

	computedStates map[string]*computedState // sessionID → state machine; guarded by sessionsMu

	lastScreenshotAt   atomic.Int64
	screenshotInFlight atomic.Bool
	screenshotFn       func(ctx context.Context, displayNum int) ([]byte, error)
	screenshotEnabled  func() bool

	bindingRateMu   sync.Mutex
	bindingLastSeen map[string]time.Time

	proxyRateMu   sync.Mutex
	proxyLastEmit map[string]time.Time

	// Lifecycle workers survive reconnects. Capture work is drained before
	// replacing a connection so stale sessions cannot write into the next one.
	asyncWg   sync.WaitGroup
	captureWg sync.WaitGroup
	restartMu sync.Mutex

	lifecycleCtx context.Context
	cancel       context.CancelFunc
	running      atomic.Bool
}

// New creates a Monitor. displayNum is the X display for ffmpeg screenshots.
// screenshotEnabled gates screenshot capture; a nil predicate always captures.
func New(upstreamMgr UpstreamProvider, publish PublishFunc, displayNum int, log *slog.Logger, screenshotEnabled func() bool) *Monitor {
	m := &Monitor{
		upstreamMgr:       upstreamMgr,
		publish:           publish,
		displayNum:        displayNum,
		log:               log,
		screenshotEnabled: screenshotEnabled,
		sessions:          make(map[string]targetInfo),
		computedStates:    make(map[string]*computedState),
		pendingRequests:   make(map[networkRequestKey]networkReqState),
		bindingLastSeen:   make(map[string]time.Time),
		proxyLastEmit:     make(map[string]time.Time),
		lifecycleCtx:      context.Background(),
	}
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
	m.Stop()
	devtoolsURL := m.upstreamMgr.Current()
	if devtoolsURL == "" {
		return fmt.Errorf("cdpmonitor: no DevTools URL available")
	}
	ctx, cancel := context.WithCancel(ctx)
	m.lifeMu.Lock()
	m.lifecycleCtx, m.cancel = ctx, cancel
	m.lifeMu.Unlock()
	if err := m.openConnection(ctx, devtoolsURL); err != nil {
		cancel()
		return err
	}
	m.running.Store(true)
	m.log.Info("cdpmonitor: started", "url", devtoolsURL)
	m.asyncWg.Go(func() { m.subscribeToUpstream(ctx) })
	m.asyncWg.Go(func() { m.sweepPendingRequests(ctx) })
	return nil
}

// Stop cancels the lifecycle and waits for both connection and capture work.
func (m *Monitor) Stop() {
	wasRunning := m.running.Swap(false)
	if wasRunning {
		m.log.Info("cdpmonitor: stopping")
	}
	m.lifeMu.Lock()
	if m.cancel != nil {
		m.cancel()
	}
	m.lifeMu.Unlock()
	m.asyncWg.Wait()
	m.restartMu.Lock()
	defer m.restartMu.Unlock()
	m.closeConnection()
	m.clearState()
	if wasRunning {
		m.log.Info("cdpmonitor: stopped")
	}
}

func (m *Monitor) openConnection(ctx context.Context, devtoolsURL string) error {
	protocol, err := cdpclient.DialWithEvents(ctx, devtoolsURL)
	if err != nil {
		return fmt.Errorf("cdpmonitor: dial %s: %w", devtoolsURL, err)
	}
	ctx, cancel := context.WithCancel(ctx)
	conn := &monitorConnection{
		protocol: protocol,
		surface: browsersurface.New(protocol,
			browsersurface.WithAdditionalTargets("worker", "shared_worker", "service_worker", "background_page"),
			browsersurface.WithoutLocations(),
		),
		ctx: ctx, cancel: cancel, done: make(chan struct{}),
	}
	eventCh, unsubscribe := conn.surface.Subscribe()
	m.lifeMu.Lock()
	m.conn = conn
	m.lifeMu.Unlock()
	go func() {
		defer close(conn.done)
		defer unsubscribe()
		defer cancel()
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-eventCh:
				if !ok {
					return
				}
				m.handleSurfaceEvent(conn, event)
			}
		}
	}()
	m.captureWg.Go(func() {
		if err := conn.surface.Start(ctx); err != nil && ctx.Err() == nil {
			m.log.Error("cdpmonitor: browser surface discovery failed", "err", err)
			data, _ := json.Marshal(oapi.BrowserMonitorInitFailedEventData{Step: "browsersurface.Start"})
			m.publish(events.Event{
				Ts: time.Now().UnixMicro(), Type: EventMonitorInitFailed, Category: events.Monitor,
				Source: oapi.BrowserEventSource{Kind: oapi.LocalProcess}, Data: data,
			})
		}
	})
	return nil
}

func (m *Monitor) closeConnection() {
	m.lifeMu.Lock()
	conn := m.conn
	m.lifeMu.Unlock()
	if conn != nil {
		conn.cancel()
		_ = conn.protocol.Close()
		<-conn.done
		<-conn.surface.Done()
	}
	m.captureWg.Wait()
	m.lifeMu.Lock()
	m.conn = nil
	m.lifeMu.Unlock()
}

func (m *Monitor) handleSurfaceEvent(conn *monitorConnection, event browsersurface.Event) {
	switch event.Kind {
	case browsersurface.EventSessionAttached:
		if !conn.surface.SessionExists(event.SessionID) {
			return
		}
		m.handleAttachedToTarget(conn.ctx, cdpTargetAttachedToTargetParams{
			SessionID: event.SessionID,
			TargetInfo: cdpTargetTargetInfo{
				TargetID: event.Target.ID, Type: event.Target.Type,
				URL: event.Target.URL, Title: event.Target.Title, OpenerID: event.Target.OpenerID,
			},
		})
	case browsersurface.EventSessionRemoved:
		m.handleDetachedFromTarget(cdpTargetDetachedFromTargetParams{SessionID: event.SessionID})
	case browsersurface.EventProtocol:
		// Attachment lifecycle is emitted once by the tracker, including targets
		// discovered through enumeration whose attach response arrived first.
		if event.Message.Method == "Target.attachedToTarget" || event.Message.Method == "Target.detachedFromTarget" {
			return
		}
		m.dispatchEvent(cdpMessage{
			Method: event.Message.Method, Params: event.Message.Params, SessionID: event.Message.SessionID,
		})
	}
}

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
	m.pendingRequests = make(map[networkRequestKey]networkReqState)
	m.pendReqMu.Unlock()
	m.bindingRateMu.Lock()
	m.bindingLastSeen = make(map[string]time.Time)
	m.bindingRateMu.Unlock()
	m.proxyRateMu.Lock()
	m.proxyLastEmit = make(map[string]time.Time)
	m.proxyRateMu.Unlock()
}

const pendingRequestTTL = 5 * time.Minute
const sweepInterval = 1 * time.Minute

// sweepPendingRequests bounds requests whose terminal event never arrives.
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

func (m *Monitor) computedFor(sessionID string) *computedState {
	m.sessionsMu.RLock()
	cs := m.computedStates[sessionID]
	m.sessionsMu.RUnlock()
	return cs
}

const sendTimeout = 30 * time.Second

func (m *Monitor) send(ctx context.Context, method string, params any, sessionID string) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.lifeMu.Lock()
	conn := m.conn
	m.lifeMu.Unlock()
	if conn == nil {
		return nil, fmt.Errorf("cdpmonitor: connection not open")
	}
	return conn.protocol.Send(ctx, method, params, sessionID)
}

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

func (m *Monitor) handleUpstreamRestart(ctx context.Context, newURL string) {
	m.restartMu.Lock()
	defer m.restartMu.Unlock()
	if ctx.Err() != nil {
		return
	}
	data, _ := json.Marshal(oapi.BrowserMonitorDisconnectedEventData{Reason: oapi.ChromeRestarted})
	m.publish(events.Event{
		Ts: time.Now().UnixMicro(), Type: EventMonitorDisconnected, Category: events.Monitor,
		Source: oapi.BrowserEventSource{Kind: oapi.LocalProcess}, Data: data,
	})
	startReconnect := time.Now()
	m.closeConnection()
	m.clearState()
	if !m.reconnectWithBackoff(ctx, newURL) {
		if ctx.Err() == nil {
			m.lifeMu.Lock()
			m.cancel()
			m.lifeMu.Unlock()
			m.running.Store(false)
			data, _ := json.Marshal(oapi.BrowserMonitorReconnectFailedEventData{Reason: oapi.ReconnectExhausted})
			m.publish(events.Event{
				Ts: time.Now().UnixMicro(), Type: EventMonitorReconnectFailed, Category: events.Monitor,
				Source: oapi.BrowserEventSource{Kind: oapi.LocalProcess}, Data: data,
			})
		}
		return
	}
	durationMs := time.Since(startReconnect).Milliseconds()
	m.log.Info("cdpmonitor: reconnected", "url", newURL, "duration_ms", durationMs)
	data, _ = json.Marshal(oapi.BrowserMonitorReconnectedEventData{ReconnectDurationMs: durationMs})
	m.publish(events.Event{
		Ts: time.Now().UnixMicro(), Type: EventMonitorReconnected, Category: events.Monitor,
		Source: oapi.BrowserEventSource{Kind: oapi.LocalProcess}, Data: data,
	})
}

const maxReconnectAttempts = 10

var reconnectBackoffs = []time.Duration{
	250 * time.Millisecond,
	500 * time.Millisecond,
	1 * time.Second,
	2 * time.Second,
}

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
		if err := m.openConnection(ctx, newURL); err != nil {
			m.log.Warn("cdpmonitor: reconnect attempt failed", "attempt", attempt+1, "max_attempts", maxReconnectAttempts, "url", newURL, "err", err)
			continue
		}
		return true
	}
	return false
}
