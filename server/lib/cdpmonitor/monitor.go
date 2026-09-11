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
	ready    atomic.Bool
}

// Monitor owns its CDP connection and browser-surface tracker independently of
// other consumers such as WebMCP. A new pair is created on each reconnect.
type Monitor struct {
	upstreamMgr UpstreamProvider
	publish     PublishFunc
	displayNum  int
	log         *slog.Logger

	controlMu          sync.Mutex    // serializes Start and Stop
	desiredMu          sync.RWMutex  // fences publication against desired-state changes; no CDP work
	desiredTelemetry   atomic.Uint64 // revision in high bits, enabled in low bit
	appliedTelemetry   atomic.Uint64
	telemetryChanged   chan struct{}
	telemetryMu        sync.RWMutex
	telemetryEnabled   bool
	telemetryChanging  atomic.Bool
	telemetryCtx       context.Context
	telemetryCancel    context.CancelFunc
	telemetryWg        sync.WaitGroup
	computedPublishMu  sync.Mutex
	optionalSessions   map[string]string   // session -> injected script identifier
	interactionTargets map[string]struct{} // cleanup obligations by target ID; survives reconnect; sessionsMu
	network            *networkCounters
	networkReady       map[string]bool // sessionsMu

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
		telemetryEnabled:   true,
		telemetryCtx:       context.Background(),
		optionalSessions:   make(map[string]string),
		interactionTargets: make(map[string]struct{}),
		telemetryChanged:   make(chan struct{}, 1),
		network:            newNetworkCounters(),
		networkReady:       make(map[string]bool),
		upstreamMgr:        upstreamMgr,
		displayNum:         displayNum,
		log:                log,
		screenshotEnabled:  screenshotEnabled,
		sessions:           make(map[string]targetInfo),
		computedStates:     make(map[string]*computedState),
		pendingRequests:    make(map[networkRequestKey]networkReqState),
		bindingLastSeen:    make(map[string]time.Time),
		proxyLastEmit:      make(map[string]time.Time),
		lifecycleCtx:       context.Background(),
	}
	m.desiredTelemetry.Store(1)
	m.appliedTelemetry.Store(1)
	m.publish = func(ev events.Event) (events.Envelope, bool) {
		m.desiredMu.RLock()
		defer m.desiredMu.RUnlock()
		if ev.Category != events.Monitor && !m.captureEnabled() {
			return events.Envelope{}, false
		}
		return publish(ev)
	}
	m.mainSessionID.Store(mainSessionUnset)
	return m
}

// IsRunning reports whether the monitor lifecycle is running (including retries).
func (m *Monitor) IsRunning() bool {
	return m.running.Load()
}

// Start starts the lifecycle even if Chrome is not available yet. Capture
// readiness is reported separately by NetworkSnapshot().Up.
func (m *Monitor) Start(ctx context.Context) error {
	m.controlMu.Lock()
	defer m.controlMu.Unlock()
	m.stop()
	if err := ctx.Err(); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	m.lifeMu.Lock()
	m.lifecycleCtx, m.cancel = ctx, cancel
	m.lifeMu.Unlock()
	// Subscribe before reading Current so a restart during dialing cannot be lost.
	ch, unsubscribe := m.upstreamMgr.Subscribe()
	url := m.upstreamMgr.Current()
	if url != "" {
		if err := m.openConnection(ctx, url); err != nil {
			m.log.Warn("cdpmonitor: initial connection failed", "err", err)
		}
	}
	m.running.Store(true)
	m.asyncWg.Go(func() { defer unsubscribe(); m.supervise(ctx, ch) })
	m.asyncWg.Go(func() { m.sweepPendingRequests(ctx) })
	m.asyncWg.Go(func() { m.reconcileTelemetry(ctx) })
	return nil
}

// Stop cancels the lifecycle and waits for both connection and capture work.
func (m *Monitor) Stop() {
	m.controlMu.Lock()
	defer m.controlMu.Unlock()
	m.stop()
}

func (m *Monitor) stop() {
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
	dialCtx, dialCancel := context.WithTimeout(ctx, 5*time.Second)
	defer dialCancel()
	protocol, err := cdpclient.DialWithEvents(dialCtx, devtoolsURL)
	if err != nil {
		return fmt.Errorf("cdpmonitor: dial %s: %w", devtoolsURL, err)
	}
	if err := m.pruneInteractionTargets(ctx, protocol); err != nil {
		_ = protocol.Close()
		return err
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
	m.telemetryMu.Lock()
	state := m.desiredTelemetry.Load()
	m.telemetryEnabled = state&1 != 0
	m.appliedTelemetry.Store(state)
	m.telemetryCtx, m.telemetryCancel = context.WithCancel(ctx)
	m.telemetryMu.Unlock()
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
		initCtx, initCancel := context.WithTimeout(ctx, sendTimeout)
		defer initCancel()
		if err := conn.surface.Start(initCtx); err != nil && ctx.Err() == nil {
			m.log.Error("cdpmonitor: browser surface discovery failed", "err", err)
			data, _ := json.Marshal(oapi.BrowserMonitorInitFailedEventData{Step: "browsersurface.Start"})
			m.publish(events.Event{
				Ts: time.Now().UnixMicro(), Type: EventMonitorInitFailed, Category: events.Monitor,
				Source: oapi.BrowserEventSource{Kind: oapi.LocalProcess}, Data: data,
			})
			conn.cancel()
			return
		}
		conn.ready.Store(ctx.Err() == nil)
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
	m.telemetryWg.Wait()
	m.lifeMu.Lock()
	m.conn = nil
	m.lifeMu.Unlock()
}

func (m *Monitor) handleSurfaceEvent(conn *monitorConnection, event browsersurface.Event) {
	switch event.Kind {
	case browsersurface.EventDiscoveryFailed:
		conn.cancel()
	case browsersurface.EventSessionAttached:
		if !conn.surface.SessionExists(event.SessionID) {
			return
		}
		m.handleAttachedToTarget(conn.ctx, cdpTargetAttachedToTargetParams{
			SessionID: event.SessionID,
			TargetInfo: cdpTargetTargetInfo{
				TargetID: event.Target.ID, Type: event.Target.Type,
				URL: event.Target.URL, Title: event.Target.Title, OpenerID: event.Target.OpenerID,
				ParentFrameID: event.Target.ParentFrameID,
			},
		})
	case browsersurface.EventSessionRemoved:
		m.handleDetachedFromTarget(cdpTargetDetachedFromTargetParams{SessionID: event.SessionID})
	case browsersurface.EventProtocol:
		if event.Message.Method == "Target.targetDestroyed" {
			var p struct {
				TargetID string `json:"targetId"`
			}
			if json.Unmarshal(event.Message.Params, &p) == nil {
				m.sessionsMu.Lock()
				delete(m.interactionTargets, p.TargetID)
				m.sessionsMu.Unlock()
			}
		}
		// Attachment lifecycle is emitted once by the tracker, including targets
		// discovered through enumeration whose attach response arrived first.
		if event.Message.Method == "Target.attachedToTarget" || event.Message.Method == "Target.detachedFromTarget" {
			return
		}
		// Preserve crash reporting even if Chrome has already detached the session.
		if event.Message.SessionID != "" && event.Message.Method != "Inspector.targetCrashed" {
			m.sessionsMu.RLock()
			_, tracked := m.sessions[event.Message.SessionID]
			m.sessionsMu.RUnlock()
			if !tracked {
				return
			}
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
	m.networkReady = make(map[string]bool)
	m.computedStates = make(map[string]*computedState)
	clear(m.optionalSessions)
	m.sessionsMu.Unlock()
	for _, cs := range prev {
		cs.stop()
	}
	m.computedPublishMu.Lock()
	m.computedPublishMu.Unlock()
	m.network.newGeneration()
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

func (m *Monitor) supervise(ctx context.Context, updates <-chan string) {
	backoff := 250 * time.Millisecond
	var disconnectedAt time.Time
	probe := time.NewTicker(5 * time.Second)
	defer probe.Stop()
	defer m.running.Store(false)
	defer func() {
		m.restartMu.Lock()
		defer m.restartMu.Unlock()
		m.closeConnection()
		m.clearState()
	}()
	for ctx.Err() == nil {
		m.lifeMu.Lock()
		conn := m.conn
		m.lifeMu.Unlock()
		if conn != nil {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-updates:
				if !ok {
					updates = nil
					continue
				}
			case <-conn.ctx.Done():
			case <-conn.protocol.Done():
			case <-probe.C:
				// A bounded round trip also catches half-open sockets after suspend.
				probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				_, err := conn.protocol.GetBrowserVersion(probeCtx)
				cancel()
				if err == nil {
					if m.NetworkSnapshot().Up {
						backoff = 250 * time.Millisecond
					}
					continue
				}
			}
			if ctx.Err() != nil {
				return
			}
			// Invalidate health and unblock capture before waiting for restartMu.
			conn.cancel()
			data, _ := json.Marshal(oapi.BrowserMonitorDisconnectedEventData{Reason: oapi.ChromeRestarted})
			m.publish(events.Event{
				Ts: time.Now().UnixMicro(), Type: EventMonitorDisconnected, Category: events.Monitor,
				Source: oapi.BrowserEventSource{Kind: oapi.LocalProcess}, Data: data,
			})
		}
		if disconnectedAt.IsZero() {
			disconnectedAt = time.Now()
		}
		m.restartMu.Lock()
		m.closeConnection()
		m.clearState()
		m.restartMu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, 5*time.Second)
		// Always reread, including after failed dials and ordinary socket loss.
		url := m.upstreamMgr.Current()
		if url == "" {
			continue
		}
		m.restartMu.Lock()
		err := m.openConnection(ctx, url)
		m.restartMu.Unlock()
		if err != nil {
			m.log.Warn("cdpmonitor: reconnect failed", "err", err)
			continue
		}
		data, _ := json.Marshal(oapi.BrowserMonitorReconnectedEventData{ReconnectDurationMs: time.Since(disconnectedAt).Milliseconds()})
		m.publish(events.Event{
			Ts: time.Now().UnixMicro(), Type: EventMonitorReconnected, Category: events.Monitor,
			Source: oapi.BrowserEventSource{Kind: oapi.LocalProcess}, Data: data,
		})
		disconnectedAt = time.Time{}
	}
}
