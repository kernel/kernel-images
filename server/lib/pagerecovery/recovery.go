// Package pagerecovery replays a top-level navigation that the site refused,
// so the caller driving the browser does not have to.
//
// The unit of work is one Fetch.requestPaused notification at the response
// stage for a main-frame document. When the response is a throttle or a dead
// connection, the recoverer answers it with a 307 back to the same URL instead
// of letting it through. Chromium treats that as a redirect inside the
// navigation that is already in flight, so the client's Page.navigate resolves
// once on the page it asked for, having waited out the retries, and never sees
// the refusal at all. Cookies the refusal set are still applied: the network
// stack processes them before the request is paused.
//
// What this does not cover: a block that answers 200 and puts an interstitial
// in the document, where the evidence is the rendered page rather than the
// status line. Recognising those is the anti-bot extension's reading, and
// acting on one means a real reload after the document has run, not a redirect
// before it. This package deliberately handles only the half that can be
// decided from the response itself.
package pagerecovery

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kernel/kernel-images/server/lib/browsersurface"
	"github.com/kernel/kernel-images/server/lib/cdpclient"
)

const (
	dialTimeout    = 5 * time.Second
	commandTimeout = 10 * time.Second
	// maxInFlight bounds how many replays may be waiting out a backoff at once.
	// A tab can only have one navigation in flight, so this bounds tabs rather
	// than requests; past it a refusal goes through instead of queueing behind
	// the others, because the caller is waiting either way.
	maxInFlight = 64
)

// UpstreamProvider abstracts *devtoolsproxy.UpstreamManager, matching how
// cdpmonitor takes it.
type UpstreamProvider interface {
	Current() string
	Subscribe() (<-chan string, func())
}

// Snapshot is the recoverer's contribution to /metrics.
type Snapshot struct {
	Retries   uint64
	Recovered uint64
	Exhausted uint64
	Up        bool
}

type connection struct {
	protocol *cdpclient.Client
	surface  *browsersurface.Tracker
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	ready    atomic.Bool
}

// Recoverer owns its own CDP connection and surface tracker. It shares no state
// with cdpmonitor or WebMCP: interception has to run whether or not customer
// telemetry is on, and a connection of its own is what keeps the two lifecycles
// from having to agree.
type Recoverer struct {
	upstream UpstreamProvider
	log      *slog.Logger

	ledger *ledger
	slots  chan struct{}

	retries   atomic.Uint64
	recovered atomic.Uint64
	exhausted atomic.Uint64

	controlMu sync.Mutex
	lifeMu    sync.Mutex
	conn      *connection

	sessionsMu sync.RWMutex
	mainFrames map[string]string // CDP session ID → main frame ID

	workWg  sync.WaitGroup
	asyncWg sync.WaitGroup
	cancel  context.CancelFunc
}

func New(upstream UpstreamProvider, cfg Config, log *slog.Logger) *Recoverer {
	if log == nil {
		log = slog.Default()
	}
	return &Recoverer{
		upstream:   upstream,
		log:        log,
		ledger:     newLedger(cfg, time.Now),
		slots:      make(chan struct{}, maxInFlight),
		mainFrames: make(map[string]string),
	}
}

// trackedSessions reports how many page sessions have interception installed.
func (r *Recoverer) trackedSessions() int {
	r.sessionsMu.RLock()
	defer r.sessionsMu.RUnlock()
	return len(r.mainFrames)
}

func (r *Recoverer) SnapshotMetrics() Snapshot {
	r.lifeMu.Lock()
	conn := r.conn
	r.lifeMu.Unlock()
	return Snapshot{
		Retries:   r.retries.Load(),
		Recovered: r.recovered.Load(),
		Exhausted: r.exhausted.Load(),
		Up:        conn != nil && conn.ready.Load() && conn.ctx.Err() == nil,
	}
}

// Start begins the lifecycle whether or not Chrome is up yet; the supervisor
// acquires the connection when it appears and replaces it when it goes.
func (r *Recoverer) Start(ctx context.Context) error {
	r.controlMu.Lock()
	defer r.controlMu.Unlock()
	r.stop()
	if err := ctx.Err(); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	r.lifeMu.Lock()
	r.cancel = cancel
	r.lifeMu.Unlock()
	// Subscribe before reading Current so a restart during dialing is not lost.
	updates, unsubscribe := r.upstream.Subscribe()
	if url := r.upstream.Current(); url != "" {
		if err := r.openConnection(ctx, url); err != nil {
			r.log.Warn("pagerecovery: initial connection failed", "err", err)
		}
	}
	r.asyncWg.Go(func() { defer unsubscribe(); r.supervise(ctx, updates) })
	return nil
}

func (r *Recoverer) Stop() {
	r.controlMu.Lock()
	defer r.controlMu.Unlock()
	r.stop()
}

// stop is idempotent and does not consult whether the lifecycle is still
// running: the supervisor can have exited on its own with a connection still
// open, and that connection still has to be closed.
func (r *Recoverer) stop() {
	r.lifeMu.Lock()
	cancel := r.cancel
	r.cancel = nil
	r.lifeMu.Unlock()
	if cancel != nil {
		cancel()
	}
	r.asyncWg.Wait()
	r.closeConnection()
}

func (r *Recoverer) openConnection(ctx context.Context, devtoolsURL string) error {
	dialCtx, dialCancel := context.WithTimeout(ctx, dialTimeout)
	defer dialCancel()
	protocol, err := cdpclient.DialWithEvents(dialCtx, devtoolsURL)
	if err != nil {
		return fmt.Errorf("pagerecovery: dial %s: %w", devtoolsURL, err)
	}
	connCtx, cancel := context.WithCancel(ctx)
	conn := &connection{
		protocol: protocol,
		// Pages only. An OOPIF's document is not what the caller navigated to,
		// and workers do not carry navigations at all.
		surface: browsersurface.New(protocol, browsersurface.WithoutLocations()),
		ctx:     connCtx,
		cancel:  cancel,
		done:    make(chan struct{}),
	}
	events, unsubscribe := conn.surface.Subscribe()
	r.lifeMu.Lock()
	r.conn = conn
	r.lifeMu.Unlock()

	go func() {
		defer close(conn.done)
		defer unsubscribe()
		defer cancel()
		for {
			select {
			case <-connCtx.Done():
				return
			case event, ok := <-events:
				if !ok {
					return
				}
				r.handleSurfaceEvent(conn, event)
			}
		}
	}()

	initCtx, initCancel := context.WithTimeout(connCtx, commandTimeout)
	defer initCancel()
	if err := conn.surface.Start(initCtx); err != nil {
		cancel()
		return fmt.Errorf("pagerecovery: browser surface discovery: %w", err)
	}
	conn.ready.Store(connCtx.Err() == nil)
	return nil
}

func (r *Recoverer) closeConnection() {
	r.lifeMu.Lock()
	conn := r.conn
	r.conn = nil
	r.lifeMu.Unlock()
	if conn != nil {
		conn.cancel()
		_ = conn.protocol.Close()
		<-conn.done
		<-conn.surface.Done()
	}
	// Retries in backoff hold a reference to the dead connection; draining them
	// before the next one opens keeps a reply from landing on a stale session.
	r.workWg.Wait()
	r.sessionsMu.Lock()
	clear(r.mainFrames)
	r.sessionsMu.Unlock()
}

// supervise replaces the connection on socket loss, upstream notification, or a
// silent failure caught by the probe. Retries continue for the process
// lifetime: a browser VM outliving its recoverer would silently lose the
// feature with nothing to say so.
func (r *Recoverer) supervise(ctx context.Context, updates <-chan string) {
	backoff := 250 * time.Millisecond
	probe := time.NewTicker(5 * time.Second)
	defer probe.Stop()
	for ctx.Err() == nil {
		r.lifeMu.Lock()
		conn := r.conn
		r.lifeMu.Unlock()
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
				probeCtx, cancel := context.WithTimeout(ctx, dialTimeout)
				_, err := conn.protocol.GetBrowserVersion(probeCtx)
				cancel()
				if err == nil {
					backoff = 250 * time.Millisecond
					continue
				}
			}
			if ctx.Err() != nil {
				return
			}
			conn.cancel()
		}
		r.closeConnection()
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, 5*time.Second)
		url := r.upstream.Current()
		if url == "" {
			continue
		}
		if err := r.openConnection(ctx, url); err != nil {
			r.log.Warn("pagerecovery: reconnect failed", "err", err)
		}
	}
}

func (r *Recoverer) handleSurfaceEvent(conn *connection, event browsersurface.Event) {
	switch event.Kind {
	case browsersurface.EventDiscoveryFailed:
		conn.cancel()
	case browsersurface.EventSessionAttached:
		if event.Target.Type != "page" {
			return
		}
		r.workWg.Go(func() { r.enableInterception(conn, event.SessionID) })
	case browsersurface.EventSessionRemoved:
		r.sessionsMu.Lock()
		delete(r.mainFrames, event.SessionID)
		r.sessionsMu.Unlock()
		r.ledger.forget(event.SessionID)
	case browsersurface.EventProtocol:
		if event.Message.Method != "Fetch.requestPaused" || event.Message.SessionID == "" {
			return
		}
		var paused cdpFetchRequestPaused
		if err := json.Unmarshal(event.Message.Params, &paused); err != nil {
			r.log.Warn("pagerecovery: undecodable Fetch.requestPaused", "err", err)
			return
		}
		// Off the event goroutine: every branch below sends a CDP command, and
		// the client's event channel is bounded, so waiting for a reply here
		// would eventually stall the read loop that has to deliver it.
		r.workWg.Go(func() { r.dispatch(conn, event.Message.SessionID, paused) })
	}
}

// enableInterception subscribes to main-frame documents at the response stage.
// The main frame ID is read once per session: Chromium keeps it for the life of
// the target, and reading it here means the hot path compares two strings
// instead of asking the browser which frame paused.
func (r *Recoverer) enableInterception(conn *connection, sessionID string) {
	ctx, cancel := context.WithTimeout(conn.ctx, commandTimeout)
	defer cancel()

	raw, err := conn.protocol.Send(ctx, "Page.getFrameTree", nil, sessionID)
	if err != nil {
		if conn.ctx.Err() == nil {
			r.log.Warn("pagerecovery: frame tree lookup failed", "session", sessionID, "err", err)
		}
		return
	}
	var tree cdpPageGetFrameTreeResult
	if err := json.Unmarshal(raw, &tree); err != nil || tree.FrameTree.Frame.ID == "" {
		r.log.Warn("pagerecovery: undecodable frame tree", "session", sessionID, "err", err)
		return
	}
	r.sessionsMu.Lock()
	r.mainFrames[sessionID] = tree.FrameTree.Frame.ID
	r.sessionsMu.Unlock()

	if _, err := conn.protocol.Send(ctx, "Fetch.enable", map[string]any{
		"patterns": []map[string]any{{
			"urlPattern":   "*",
			"requestStage": "Response",
			"resourceType": "Document",
		}},
	}, sessionID); err != nil && conn.ctx.Err() == nil {
		r.log.Warn("pagerecovery: Fetch.enable failed", "session", sessionID, "err", err)
	}
}

// dispatch answers a paused response, on a goroutine of its own. A paused
// request is one main-frame document load, so the goroutines are bounded by how
// fast the browser navigates, and all but the replays last one round trip.
func (r *Recoverer) dispatch(conn *connection, sessionID string, paused cdpFetchRequestPaused) {
	r.sessionsMu.RLock()
	mainFrame, tracked := r.mainFrames[sessionID]
	r.sessionsMu.RUnlock()

	response := pausedResponse{
		method:       strings.ToUpper(paused.Request.Method),
		status:       paused.ResponseStatusCode,
		errorReason:  paused.ResponseErrorReason,
		headers:      lowercaseHeaders(paused.ResponseHeaders),
		isTopLevel:   tracked && paused.FrameID == mainFrame,
		isNavigation: paused.ResourceType == "Document",
	}
	if !response.retryable() {
		if response.isTopLevel && response.isNavigation {
			r.settle(sessionID, paused)
		}
		r.continueResponse(conn, sessionID, paused.RequestID)
		return
	}

	// The slot is taken before the attempt is spent, so a replay refused for
	// want of a waiting slot does not also cost the navigation one of its tries.
	select {
	case r.slots <- struct{}{}:
	default:
		// Over the in-flight bound the honest answer is the site's, not a
		// queued one: the caller is waiting either way.
		r.continueResponse(conn, sessionID, paused.RequestID)
		return
	}
	defer func() { <-r.slots }()

	requested, hasRequested := response.retryAfter(time.Now())
	wait, ok := r.ledger.reserve(sessionID, paused.Request.URL, requested, hasRequested)
	if !ok {
		r.exhausted.Add(1)
		r.log.Info("pagerecovery: retry budget spent, passing refusal through",
			"status", paused.ResponseStatusCode, "error_reason", paused.ResponseErrorReason)
		r.continueResponse(conn, sessionID, paused.RequestID)
		return
	}
	r.retries.Add(1)
	r.replay(conn, sessionID, paused, wait)
}

// settle closes out a navigation that answered with something we did not
// retry. A status below 400 after at least one replay is the outcome the
// feature exists for; a transport failure carries no status at all, so it is
// not one of them.
func (r *Recoverer) settle(sessionID string, paused cdpFetchRequestPaused) {
	attempts := r.ledger.settle(sessionID, paused.Request.URL)
	if attempts == 0 || paused.ResponseStatusCode <= 0 || paused.ResponseStatusCode >= 400 {
		return
	}
	r.recovered.Add(1)
	r.log.Info("pagerecovery: navigation recovered",
		"attempts", attempts, "status", paused.ResponseStatusCode)
}

func (r *Recoverer) replay(conn *connection, sessionID string, paused cdpFetchRequestPaused, wait time.Duration) {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-conn.ctx.Done():
		return
	case <-timer.C:
	}

	ctx, cancel := context.WithTimeout(conn.ctx, commandTimeout)
	defer cancel()
	// A 307 keeps the navigation the caller is waiting on alive as one
	// navigation with one more hop, rather than starting a second one it would
	// see as its first being interrupted.
	_, err := conn.protocol.Send(ctx, "Fetch.fulfillRequest", map[string]any{
		"requestId":    paused.RequestID,
		"responseCode": 307,
		"responseHeaders": []map[string]string{
			{"name": "Location", "value": paused.Request.URL},
			{"name": "Cache-Control", "value": "no-store"},
		},
		// Chromium treats a fulfillment without a body as no fulfillment at
		// all and lets the original response through; a redirect has no body
		// to send, so it has to be sent as an empty one.
		"body": "",
	}, sessionID)
	if err != nil && conn.ctx.Err() == nil {
		r.log.Warn("pagerecovery: replay failed", "session", sessionID, "err", err)
	}
	r.log.Info("pagerecovery: replaying refused navigation",
		"status", paused.ResponseStatusCode, "error_reason", paused.ResponseErrorReason,
		"waited_ms", wait.Milliseconds())
}

// continueResponse hands the response to the renderer unchanged. A failure here
// is not recoverable from this side: the request is already paused, so the only
// alternative to logging it is leaving the navigation hanging.
func (r *Recoverer) continueResponse(conn *connection, sessionID, requestID string) {
	ctx, cancel := context.WithTimeout(conn.ctx, commandTimeout)
	defer cancel()
	if _, err := conn.protocol.Send(ctx, "Fetch.continueResponse", map[string]any{
		"requestId": requestID,
	}, sessionID); err != nil && conn.ctx.Err() == nil {
		r.log.Warn("pagerecovery: continueResponse failed", "session", sessionID, "err", err)
	}
}

func lowercaseHeaders(headers []cdpHeaderEntry) map[string]string {
	if len(headers) == 0 {
		return nil
	}
	out := make(map[string]string, len(headers))
	for _, header := range headers {
		out[strings.ToLower(header.Name)] = header.Value
	}
	return out
}
