package devtoolsproxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/onkernel/kernel-images/server/lib/scaletozero"
	"github.com/onkernel/kernel-images/server/lib/wsproxy"
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

// WebSocketProxyHandler returns an http.Handler that upgrades incoming connections and
// proxies them to the current upstream websocket URL. It expects only websocket requests.
// If logCDPMessages is true, all CDP messages will be logged with their direction.
func WebSocketProxyHandler(mgr *UpstreamManager, logger *slog.Logger, logCDPMessages bool, ctrl scaletozero.Controller) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCurrent := mgr.Current()
		if upstreamCurrent == "" {
			http.Error(w, "upstream not ready", http.StatusServiceUnavailable)
			return
		}
		parsed, err := url.Parse(upstreamCurrent)
		if err != nil {
			http.Error(w, "invalid upstream", http.StatusInternalServerError)
			return
		}
		upstreamURL := (&url.URL{Scheme: parsed.Scheme, Host: parsed.Host, Path: parsed.Path, RawQuery: parsed.RawQuery}).String()

		var transform wsproxy.MessageTransform
		if logCDPMessages {
			transform = func(direction string, mt websocket.MessageType, msg []byte) []byte {
				logCDPMessage(logger, direction, mt, msg)
				return msg
			}
		}

		acceptOpts := &websocket.AcceptOptions{
			OriginPatterns:  []string{"*"},
			CompressionMode: websocket.CompressionContextTakeover,
		}
		dialOpts := &websocket.DialOptions{
			CompressionMode: websocket.CompressionContextTakeover,
		}
		wsproxy.Proxy(w, r, upstreamURL, wsproxy.ProxyOptions{
			AcceptOptions: acceptOpts,
			DialOptions:   dialOpts,
			Logger:        logger,
			Transform:     transform,
		})
	})
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
