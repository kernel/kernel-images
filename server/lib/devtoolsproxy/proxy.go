package devtoolsproxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/kernel/kernel-images/server/lib/events"
	oapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/kernel/kernel-images/server/lib/scaletozero"
	"github.com/kernel/kernel-images/server/lib/wsdrain"
	"github.com/kernel/kernel-images/server/lib/wsproxy"
	"github.com/nrednav/cuid2"
)

var devtoolsListeningRegexp = regexp.MustCompile(`DevTools listening on (ws://\S+)`)

// UpstreamManager tails the Chromium supervisord log and extracts the current DevTools
// websocket URL, updating it whenever Chromium restarts and emits a new line.
type UpstreamManager struct {
	logFilePath string
	logger      *slog.Logger

	currentURL atomic.Value // string

	startOnce  sync.Once
	stopOnce   sync.Once
	cancelTail context.CancelFunc

	subsMu sync.RWMutex
	subs   map[chan string]struct{}
}

func NewUpstreamManager(logFilePath string, logger *slog.Logger) *UpstreamManager {
	um := &UpstreamManager{logFilePath: logFilePath, logger: logger}
	um.currentURL.Store("")
	return um
}

// Start begins background tailing and updating the upstream URL until ctx is done.
func (u *UpstreamManager) Start(ctx context.Context) {
	u.startOnce.Do(func() {
		ctx, cancel := context.WithCancel(ctx)
		u.cancelTail = cancel
		go u.tailLoop(ctx)
	})
}

// Stop cancels the background tailer.
func (u *UpstreamManager) Stop() {
	u.stopOnce.Do(func() {
		if u.cancelTail != nil {
			u.cancelTail()
		}
	})
}

// WaitForInitial blocks until an initial upstream URL has been discovered or the timeout elapses.
func (u *UpstreamManager) WaitForInitial(timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		if url := u.Current(); url != "" {
			return url, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("devtools upstream not found within %s", timeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Current returns the current upstream websocket URL if known, or empty string.
func (u *UpstreamManager) Current() string {
	val, _ := u.currentURL.Load().(string)
	return val
}

func (u *UpstreamManager) setCurrent(url string) {
	prev := u.Current()
	if url != "" && url != prev {
		u.logger.Info("devtools upstream updated", slog.String("url", url))
		u.currentURL.Store(url)
		// Broadcast update to subscribers without blocking. If a subscriber's
		// channel buffer (size 1) is full, replace the buffered value with the
		// latest update to avoid dropping notifications entirely.
		u.subsMu.RLock()
		for ch := range u.subs {
			select {
			case ch <- url:
				// sent successfully
			default:
				// channel is full; drop one stale value if present and try again
				select {
				case <-ch:
				default:
				}
				select {
				case ch <- url:
				default:
					// still full; give up to remain non-blocking
				}
			}
		}
		u.subsMu.RUnlock()
	}
}

// Subscribe returns a channel that receives new upstream URLs as they are discovered.
// The returned cancel function should be called to unsubscribe and release resources.
func (u *UpstreamManager) Subscribe() (<-chan string, func()) {
	// use channel size 1 to avoid setCurrent blocking/stalling on slow subscribers
	// also provides "latest-wins" semantics: only one notification can sit in the channel
	ch := make(chan string, 1)
	u.subsMu.Lock()
	if u.subs == nil {
		u.subs = make(map[chan string]struct{})
	}
	u.subs[ch] = struct{}{}
	u.subsMu.Unlock()
	cancel := func() {
		u.subsMu.Lock()
		if _, ok := u.subs[ch]; ok {
			delete(u.subs, ch)
			close(ch)
		}
		u.subsMu.Unlock()
	}
	return ch, cancel
}

func (u *UpstreamManager) tailLoop(ctx context.Context) {
	backoff := 250 * time.Millisecond
	for {
		if ctx.Err() != nil {
			return
		}
		// Run one tail session. If it exits, retry with a small backoff.
		u.runTailOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		// cap backoff to 2s
		if backoff < 2*time.Second {
			backoff *= 2
		}
	}
}

func (u *UpstreamManager) runTailOnce(ctx context.Context) {
	cmd := exec.CommandContext(ctx, "tail", "-f", "-n", "+1", u.logFilePath)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		u.logger.Error("failed to open tail stdout", slog.String("err", err.Error()))
		return
	}
	if err := cmd.Start(); err != nil {
		// Common when file does not exist yet; log at debug level
		if strings.Contains(err.Error(), "No such file or directory") {
			u.logger.Debug("supervisord log not found yet; will retry", slog.String("path", u.logFilePath))
		} else {
			u.logger.Error("failed to start tail", slog.String("err", err.Error()))
		}
		return
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return
		default:
		}
		line := scanner.Text()
		if matches := devtoolsListeningRegexp.FindStringSubmatch(line); len(matches) == 2 {
			u.setCurrent(matches[1])
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, context.Canceled) {
		u.logger.Error("tail scanner error", slog.String("err", err.Error()))
	}
}

func dialUpstreamWithRetry(ctx context.Context, mgr *UpstreamManager, urlCh <-chan string, initialUpstreamURL string, dialOpts *websocket.DialOptions, logger *slog.Logger) (*websocket.Conn, string, error) {
	upstreamURL := normalizeUpstreamURL(initialUpstreamURL)
	if upstreamURL == "" {
		return nil, "", fmt.Errorf("upstream not ready")
	}

	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()

	for {
		upstreamConn, _, err := websocket.Dial(ctx, upstreamURL, dialOpts)
		if err == nil {
			return upstreamConn, upstreamURL, nil
		}

		logger.Warn("dial upstream failed, checking for newer URL",
			slog.String("err", err.Error()), slog.String("url", upstreamURL))

		latestURL := normalizeUpstreamURL(mgr.Current())
		if latestURL != "" && latestURL != upstreamURL {
			upstreamURL = latestURL
			continue
		}

		select {
		case newURL, ok := <-urlCh:
			if !ok {
				return nil, "", fmt.Errorf("upstream unavailable")
			}
			newURL = normalizeUpstreamURL(newURL)
			if newURL == "" || newURL == upstreamURL {
				continue
			}
			upstreamURL = newURL
		case <-deadline.C:
			return nil, "", fmt.Errorf("timed out waiting for new upstream URL")
		case <-ctx.Done():
			return nil, "", ctx.Err()
		}
	}
}

func maybePauseAfterCurrentRead(ctx context.Context, logger *slog.Logger, r *http.Request) {
	if r.URL.Query().Get("devtoolsProxyTestHook") != "1" {
		return
	}

	// Test-only hook used by e2e to widen the window between reading Current
	// and dialing/subscribing so reconnect races can be reproduced reliably.
	rawDelayMs := os.Getenv("DEVTOOLS_PROXY_TEST_POST_CURRENT_DELAY_MS")
	if rawDelayMs != "" {
		delayMs, err := strconv.Atoi(rawDelayMs)
		if err != nil || delayMs <= 0 {
			logger.Warn("ignoring invalid devtools proxy test delay", slog.String("value", rawDelayMs))
		} else {
			timer := time.NewTimer(time.Duration(delayMs) * time.Millisecond)
			defer timer.Stop()

			select {
			case <-timer.C:
			case <-ctx.Done():
				return
			}
		}
	}

	blockPath := os.Getenv("DEVTOOLS_PROXY_TEST_POST_CURRENT_BLOCK_FILE")
	if blockPath == "" {
		return
	}

	readyPath := blockPath + ".ready"
	releasePath := blockPath + ".release"
	if err := os.WriteFile(readyPath, []byte("ready\n"), 0o644); err != nil {
		logger.Warn("failed to write devtools proxy test ready marker",
			slog.String("path", readyPath),
			slog.String("err", err.Error()))
		return
	}

	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		if _, err := os.Stat(releasePath); err == nil {
			return
		} else if !os.IsNotExist(err) {
			logger.Warn("failed to read devtools proxy test release marker",
				slog.String("path", releasePath),
				slog.String("err", err.Error()))
			return
		}

		select {
		case <-ticker.C:
		case <-ctx.Done():
			return
		}
	}
}

// EventPublisher publishes a telemetry event onto the in-VM events
// pipeline. nil disables emission. Signature matches TelemetrySession.Publish
// so it can be wired directly; the proxy ignores the returns.
type EventPublisher func(ev events.Event) (events.Envelope, bool)

// WebSocketProxyHandler returns an http.Handler that upgrades incoming connections and
// proxies them to the current upstream websocket URL. It expects only websocket requests.
// If logCDPMessages is true, all CDP messages will be logged with their direction.
// publish is invoked on accept (cdp_connect) and on teardown (cdp_disconnect); pass
// nil to disable emission. controlEnabled gates cdp_command classification and is
// checked once per forwarded client frame; pass nil to disable it. excludedMethods
// names the control methods configured out of the stream; nil reports them all.
func WebSocketProxyHandler(mgr *UpstreamManager, logger *slog.Logger, logCDPMessages bool, ctrl scaletozero.Controller, publish EventPublisher, controlEnabled ControlEnabledFunc, excludedMethods ExcludedMethodsFunc, reg *wsdrain.Registry) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Names this connection on every event it produces, so a reader can tell
		// two clients driving the same browser apart.
		connectionID := cuid2.Generate()
		// Counts every relayed message so cdp_disconnect can report message_count.
		var msgCount atomic.Int64
		var transform wsproxy.MessageTransform = func(direction string, mt websocket.MessageType, msg []byte) []byte {
			if logCDPMessages {
				logCDPMessage(logger, direction, mt, msg)
			}
			msgCount.Add(1)
			return msg
		}

		acceptOpts := &websocket.AcceptOptions{
			OriginPatterns:  []string{"*"},
			CompressionMode: websocket.CompressionDisabled,
		}
		dialOpts := &websocket.DialOptions{
			CompressionMode: websocket.CompressionDisabled,
		}

		// Subscribe to upstream URL changes so we can tear down stale sessions
		// when Chromium restarts and retry if the current URL is already dead.
		urlCh, unsub := mgr.Subscribe()
		defer unsub()

		upstreamCurrent := mgr.Current()
		if upstreamCurrent == "" {
			http.Error(w, "upstream not ready", http.StatusServiceUnavailable)
			return
		}
		maybePauseAfterCurrentRead(r.Context(), logger, r)

		// Accept the client WebSocket connection.
		clientConn, err := websocket.Accept(w, r, acceptOpts)
		if err != nil {
			logger.Error("websocket accept failed", slog.String("err", err.Error()))
			return
		}
		clientConn.SetReadLimit(100 * 1024 * 1024)

		untrack := reg.Track(clientConn)
		defer untrack()

		publishCdpConnect(publish, connectionID)
		connectedAt := time.Now()

		// Dial upstream. If the URL is stale (Chromium just restarted), first
		// re-check the manager's latest URL in case we missed the notification,
		// then wait briefly for the next update from Subscribe.
		upstreamConn, upstreamURL, err := dialUpstreamWithRetry(r.Context(), mgr, urlCh, upstreamCurrent, dialOpts, logger)
		if err != nil {
			switch {
			case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded), errors.Is(r.Context().Err(), context.Canceled), errors.Is(r.Context().Err(), context.DeadlineExceeded):
				clientConn.Close(websocket.StatusGoingAway, "request cancelled")
				publishCdpDisconnect(publish, connectionID, oapi.ContextCancelled, connectedAt, time.Now(), msgCount.Load(), 0, 0)
			default:
				logger.Error("failed to connect to upstream", slog.String("err", err.Error()))
				clientConn.Close(websocket.StatusInternalError, "upstream unavailable")
				publishCdpDisconnect(publish, connectionID, oapi.UpstreamError, connectedAt, time.Now(), msgCount.Load(), 0, 0)
			}
			return
		}
		upstreamConn.SetReadLimit(100 * 1024 * 1024)

		logger.Debug("proxying websocket", slog.String("url", upstreamURL))

		pumpCtx, pumpCancel := context.WithCancel(r.Context())

		// Classification of client commands runs behind the pump, not inside it:
		// a frame is observed only once Chromium has accepted it.
		observer := newCdpObserver(pumpCtx, connectionID, publish, controlEnabled, excludedMethods, logger)
		var observe wsproxy.Observer
		if observer != nil {
			observe = func(direction string, mt websocket.MessageType, msg []byte, ts int64) {
				// Client-to-upstream only: commands are what the caller drives the
				// browser with, and upstream frames are events and command results.
				if direction == "->" && mt == websocket.MessageText {
					observer.Observe(msg, ts)
				}
			}
		}

		// Force clients off a stale upstream as soon as UpstreamManager
		// publishes a different DevTools URL. Closing upstreamConn (rather
		// than cancelling pumpCtx) makes the pump exit PumpExitUpstream so
		// resolveDisconnectReason classifies the disconnect via mgr.Current().
		go func(currentUpstreamURL string) {
			for {
				select {
				case newURL, ok := <-urlCh:
					if !ok {
						return
					}
					newURL = normalizeUpstreamURL(newURL)
					if newURL == "" || newURL == currentUpstreamURL {
						continue
					}
					logger.Info("upstream URL changed, closing stale proxy session",
						slog.String("old_url", currentUpstreamURL),
						slog.String("new_url", newURL))
					upstreamConn.Close(websocket.StatusGoingAway, "upstream changed")
					return
				case <-pumpCtx.Done():
					return
				}
			}
		}(upstreamURL)

		var once sync.Once
		cleanup := func(cause wsproxy.PumpExitCause) {
			once.Do(func() {
				// Pin disconnectedAt before resolveDisconnectReason so duration_ms
				// reflects actual session length, not the up-to-restartConfirmWait
				// poll. Close conns explicitly before resolving as defense in
				// depth — coder/websocket already closes the client conn as a
				// side effect of pumpCancel, but we shouldn't rely on that.
				disconnectedAt := time.Now()
				pumpCancel()
				upstreamConn.Close(websocket.StatusNormalClosure, "")
				clientConn.Close(websocket.StatusNormalClosure, "")
				reason := resolveDisconnectReason(cause, r.Context(), mgr, upstreamURL, getRestartConfirmWait(), logger)
				// Let the worker finish the queue before the disconnect, so the
				// client's last commands land ahead of it and telemetry_dropped
				// is final.
				observer.WaitDrained(cdpObserverDrainWait)
				publishCdpDisconnect(publish, connectionID, reason, connectedAt, disconnectedAt, msgCount.Load(), observer.Dropped(), observer.Excluded())
			})
		}

		wsproxy.Pump(pumpCtx, clientConn, upstreamConn, cleanup, logger, transform, observe)
	})
}

// restartConfirmWait is how long cleanup waits for a new upstream URL after
// the upstream side of the pump dies before classifying the disconnect as
// upstream_error vs upstream_changed. Sized for Chromium's typical cold
// restart (~5-8s on Unikraft Cloud) with headroom. Atomic rather than a plain
// var because tests shrink it while other handlers are still reading it.
var restartConfirmWait atomic.Int64

func init() { setRestartConfirmWait(10 * time.Second) }

func getRestartConfirmWait() time.Duration {
	return time.Duration(restartConfirmWait.Load())
}

func setRestartConfirmWait(d time.Duration) {
	restartConfirmWait.Store(int64(d))
}

// resolveDisconnectReason picks the cdp_disconnect reason from which side
// caused the pump to exit. On upstream cause it polls mgr.Current() for up
// to restartWait: a different URL within the window means Chromium restarted
// (upstream_changed); timeout means the upstream broke without a restart
// (upstream_error). Polling rather than reading urlCh avoids competing with
// the URL watcher and works because setCurrent updates Current() before
// broadcasting.
func resolveDisconnectReason(cause wsproxy.PumpExitCause, reqCtx context.Context, mgr *UpstreamManager, dialedURL string, restartWait time.Duration, logger *slog.Logger) oapi.BrowserCdpDisconnectEventDataReason {
	if reqCtx.Err() != nil {
		return oapi.ContextCancelled
	}
	switch cause {
	case wsproxy.PumpExitClient:
		return oapi.ClientClose
	case wsproxy.PumpExitContext:
		return oapi.ContextCancelled
	}

	deadline := time.Now().Add(restartWait)
	for {
		if newest := normalizeUpstreamURL(mgr.Current()); newest != "" && newest != dialedURL {
			logger.Info("upstream restart detected after disconnect",
				slog.String("old_url", dialedURL), slog.String("new_url", newest))
			return oapi.UpstreamChanged
		}
		if !time.Now().Before(deadline) {
			return oapi.UpstreamError
		}
		select {
		case <-time.After(100 * time.Millisecond):
		case <-reqCtx.Done():
			return oapi.ContextCancelled
		}
	}
}

func publishCdpConnect(publish EventPublisher, connectionID string) {
	if publish == nil {
		return
	}
	data, _ := json.Marshal(oapi.BrowserCdpConnectEventData{ConnectionId: &connectionID})
	publish(events.Event{
		Ts:       time.Now().UnixMicro(),
		Type:     "cdp_connect",
		Category: events.Connection,
		Source:   oapi.BrowserEventSource{Kind: oapi.KernelApi},
		Data:     data,
	})
}

func publishCdpDisconnect(publish EventPublisher, connectionID string, reason oapi.BrowserCdpDisconnectEventDataReason, connectedAt, disconnectedAt time.Time, msgCount, telemetryDropped, telemetryExcluded int64) {
	if publish == nil {
		return
	}
	// Optional in the schema so an event from an image that predates the field
	// still validates, but always set here: absent means "not reported", which
	// is not the same as zero.
	dropped, excluded := int(telemetryDropped), int(telemetryExcluded)
	data, _ := json.Marshal(oapi.BrowserCdpDisconnectEventData{
		ConnectionId:      &connectionID,
		DurationMs:        float32(disconnectedAt.Sub(connectedAt).Microseconds()) / 1000.0,
		MessageCount:      int(msgCount),
		TelemetryDropped:  &dropped,
		TelemetryExcluded: &excluded,
		Reason:            reason,
	})
	publish(events.Event{
		Ts:       disconnectedAt.UnixMicro(),
		Type:     "cdp_disconnect",
		Category: events.Connection,
		Source:   oapi.BrowserEventSource{Kind: oapi.KernelApi},
		Data:     data,
	})
}

// normalizeUpstreamURL parses a raw DevTools URL and returns a clean form.
func normalizeUpstreamURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return (&url.URL{Scheme: parsed.Scheme, Host: parsed.Host, Path: parsed.Path, RawQuery: parsed.RawQuery}).String()
}

// logCDPMessage logs a CDP message with its direction if logging is enabled
func logCDPMessage(logger *slog.Logger, direction string, mt websocket.MessageType, msg []byte) {
	if mt != websocket.MessageText {
		return // Only log text messages (CDP messages)
	}

	// Extract fields using regex from raw message
	rawMsg := string(msg)

	// Regex patterns to match "key":"val" or "key": "val" for string values
	extractStringField := func(key string) string {
		pattern := fmt.Sprintf(`"%s"\s*:\s*"([^"]*)"`, key)
		re := regexp.MustCompile(pattern)
		matches := re.FindStringSubmatch(rawMsg)
		if len(matches) > 1 {
			return matches[1]
		}
		return ""
	}

	// Regex pattern to match "key": number for numeric id
	extractNumberField := func(key string) interface{} {
		pattern := fmt.Sprintf(`"%s"\s*:\s*(\d+)`, key)
		re := regexp.MustCompile(pattern)
		matches := re.FindStringSubmatch(rawMsg)
		if len(matches) > 1 {
			// Try to parse as int first
			if val, err := strconv.Atoi(matches[1]); err == nil {
				return val
			}
			// Fall back to float64
			if val, err := strconv.ParseFloat(matches[1], 64); err == nil {
				return val
			}
		}
		return nil
	}

	// Extract fields using regex
	method := extractStringField("method")
	id := extractNumberField("id")
	sessionId := extractStringField("sessionId")
	targetId := extractStringField("targetId")
	frameId := extractStringField("frameId")

	// Build log attributes, only including non-empty values
	attrs := []slog.Attr{
		slog.String("dir", direction),
	}

	if sessionId != "" {
		attrs = append(attrs, slog.String("sessionId", sessionId))
	}
	if targetId != "" {
		attrs = append(attrs, slog.String("targetId", targetId))
	}
	if id != nil {
		attrs = append(attrs, slog.Any("id", id))
	}
	if frameId != "" {
		attrs = append(attrs, slog.String("frameId", frameId))
	}

	if method != "" {
		attrs = append(attrs, slog.String("method", method))
	}

	attrs = append(attrs, slog.Int("raw_length", len(msg)))

	// Convert attrs to individual slog.Attr arguments
	args := make([]any, len(attrs))
	for i, attr := range attrs {
		args[i] = attr
	}

	logger.Info("cdp", args...)
}
