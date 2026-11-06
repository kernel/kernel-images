package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/kernel/kernel-images/server/lib/devtoolsproxy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeDevTools serves the CDP surface promoteWindowMaximized needs.
// failDials HTTP requests are rejected outright, reproducing a devtools
// endpoint that refuses early connections while chromium binds its socket.
// When dropFirstConn is set, the first upgraded websocket reads one command
// and then drops without responding, reproducing a mid-promotion transport
// drop.
type fakeDevTools struct {
	mu sync.Mutex

	failDials     int
	dropFirstConn bool
	pageTargetID  string
	windowState   string

	dials         int
	conns         int
	getTargets    int
	getWindow     int
	setMaximized  int
}

func (f *fakeDevTools) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.dials++
	fail := f.dials <= f.failDials
	f.mu.Unlock()
	if fail {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
	if err != nil {
		return
	}
	defer conn.CloseNow()

	f.mu.Lock()
	f.conns++
	drop := f.dropFirstConn && f.conns == 1
	f.mu.Unlock()
	if drop {
		_, _, _ = conn.Read(r.Context())
		return
	}

	for {
		_, msg, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		var req struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		if err := json.Unmarshal(msg, &req); err != nil {
			continue
		}

		var result any
		switch req.Method {
		case "Target.getTargets":
			f.mu.Lock()
			f.getTargets++
			f.mu.Unlock()
			var targets []map[string]string
			if f.pageTargetID != "" {
				targets = append(targets, map[string]string{
					"targetId": f.pageTargetID,
					"type":     "page",
					"url":      "https://example.com/",
				})
			}
			result = map[string]any{"targetInfos": targets}
		case "Browser.getWindowForTarget":
			f.mu.Lock()
			f.getWindow++
			f.mu.Unlock()
			result = map[string]any{
				"windowId": 7,
				"bounds":   map[string]any{"width": 800, "height": 600, "windowState": f.windowState},
			}
		case "Browser.setWindowBounds":
			f.mu.Lock()
			f.setMaximized++
			f.mu.Unlock()
			result = map[string]any{}
		default:
			result = map[string]any{}
		}

		resp := map[string]any{"id": req.ID, "result": result}
		b, _ := json.Marshal(resp)
		if err := conn.Write(r.Context(), websocket.MessageText, b); err != nil {
			return
		}
	}
}

func (f *fakeDevTools) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

// newPromotionService builds an ApiService whose upstream manager has
// discovered the given devtools URL, ready for promoteWindowMaximized.
func newPromotionService(t *testing.T, url string) *ApiService {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "chromium.log")
	line := "2026-11-05T00:00:00 chromium DevTools listening on " + url + "\n"
	require.NoError(t, os.WriteFile(logPath, []byte(line), 0o644))
	return newPromotionServiceForLog(t, logPath, url)
}

// newPromotionServiceForLog is newPromotionService for a caller that keeps
// appending to the log, which is how a chromium restart announces itself.
func newPromotionServiceForLog(t *testing.T, logPath, wantURL string) *ApiService {
	t.Helper()
	um := devtoolsproxy.NewUpstreamManager(logPath, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	um.Start(ctx)
	t.Cleanup(cancel)
	t.Cleanup(um.Stop)

	_, err := um.WaitForInitial(10 * time.Second)
	require.NoError(t, err, "upstream manager never discovered the fake url")
	require.Equal(t, wantURL, um.Current())

	return &ApiService{lifecycleCtx: ctx, upstreamMgr: um}
}

// maximizedCalls reports how many times the fake has been asked to maximize.
func (f *fakeDevTools) maximizedCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.setMaximized
}

// A chromium restart is a new devtools URL, and chromium opens a --window-size
// window in normal state, so each start needs its own promotion rather than one
// at boot. This is the path a profile load and a crash both take.
func TestPromoteWindowsLoopPromotesOnEveryChromiumStart(t *testing.T) {
	first := &fakeDevTools{pageTargetID: "target-1", windowState: "normal"}
	firstURL := first.start(t)
	second := &fakeDevTools{pageTargetID: "target-2", windowState: "normal"}
	secondURL := second.start(t)

	logPath := filepath.Join(t.TempDir(), "chromium.log")
	require.NoError(t, os.WriteFile(logPath, []byte("2026-11-05T00:00:00 chromium DevTools listening on "+firstURL+"\n"), 0o644))
	s := newPromotionServiceForLog(t, logPath, firstURL)

	ch, unsubscribe := s.upstreamMgr.Subscribe()
	go s.promoteWindowsLoop(ch, unsubscribe)

	// The URL that is already current when the loop starts is promoted without
	// waiting for a notification.
	require.Eventually(t, func() bool { return first.maximizedCalls() == 1 }, 5*time.Second, 10*time.Millisecond)

	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString("2026-11-05T00:05:00 chromium DevTools listening on " + secondURL + "\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	require.Eventually(t, func() bool { return second.maximizedCalls() == 1 }, 5*time.Second, 10*time.Millisecond)
	assert.Never(t, func() bool { return first.maximizedCalls() != 1 || second.maximizedCalls() != 1 }, 250*time.Millisecond, 10*time.Millisecond, "each start is promoted once and the replaced url is not promoted again")
}

func TestPromoteWindowMaximizedHappyPath(t *testing.T) {
	f := &fakeDevTools{pageTargetID: "target-1", windowState: "normal"}
	url := f.start(t)
	s := newPromotionService(t, url)

	s.promoteWindowMaximized(url)

	f.mu.Lock()
	defer f.mu.Unlock()
	assert.Equal(t, 1, f.dials, "dial count")
	assert.Equal(t, 1, f.setMaximized, "setWindowBounds maximized calls")
}

func TestPromoteWindowMaximizedRetriesFailedDial(t *testing.T) {
	// The devtools endpoint refuses the first connection (chromium still
	// binding its socket); the promotion must redial within its deadline
	// instead of giving up on the url.
	f := &fakeDevTools{failDials: 1, pageTargetID: "target-1", windowState: "normal"}
	url := f.start(t)
	s := newPromotionService(t, url)

	s.promoteWindowMaximized(url)

	f.mu.Lock()
	defer f.mu.Unlock()
	assert.Equal(t, 2, f.dials, "the failed dial must be retried")
	assert.Equal(t, 1, f.setMaximized, "setWindowBounds maximized calls")
}

func TestPromoteWindowMaximizedRedialsAfterTransportDrop(t *testing.T) {
	// The first connection accepts, reads one command, then drops without
	// responding. Commands on that client will never succeed; the promotion
	// must redial and complete on a fresh connection.
	f := &fakeDevTools{dropFirstConn: true, pageTargetID: "target-1", windowState: "normal"}
	url := f.start(t)
	s := newPromotionService(t, url)

	s.promoteWindowMaximized(url)

	f.mu.Lock()
	defer f.mu.Unlock()
	assert.Equal(t, 2, f.dials, "the dropped transport must be redialed")
	assert.Equal(t, 1, f.setMaximized, "setWindowBounds maximized calls")
}
