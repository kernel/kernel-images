package livefocus

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

// fakeBrowser answers the commands the watcher sends for one tab. Creating
// the isolated world fails while failWorlds is above zero. With frameGone, the
// main frame is gone from the frame tree after the first scan.
type fakeBrowser struct {
	mu         sync.Mutex
	failWorlds int
	frameGone  bool
	methods    map[string]int
}

func (f *fakeBrowser) serve(t *testing.T) string {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx := r.Context()
		var writeMu sync.Mutex
		write := func(message any) {
			data, _ := json.Marshal(message)
			writeMu.Lock()
			defer writeMu.Unlock()
			_ = conn.Write(ctx, websocket.MessageText, data)
		}
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var request struct {
				ID        int64  `json:"id"`
				Method    string `json:"method"`
				SessionID string `json:"sessionId"`
			}
			require.NoError(t, json.Unmarshal(data, &request))
			respond := func(result any) { write(map[string]any{"id": request.ID, "result": result}) }
			fail := func(message string) {
				write(map[string]any{"id": request.ID, "error": map[string]any{"code": -32000, "message": message}})
			}
			f.mu.Lock()
			f.methods[request.Method]++
			failWorld := request.Method == "Page.createIsolatedWorld" && f.failWorlds > 0
			if failWorld {
				f.failWorlds--
			}
			frameGone := f.frameGone && f.methods["Page.getFrameTree"] > 1
			f.mu.Unlock()

			switch request.Method {
			case "Target.setDiscoverTargets":
				respond(map[string]any{})
			case "Target.getTargets":
				respond(map[string]any{"targetInfos": []map[string]any{{"targetId": "tab", "type": "page", "url": "https://example.com/"}}})
			case "Target.attachToTarget":
				respond(map[string]any{"sessionId": "session"})
			case "Page.getFrameTree":
				if frameGone {
					respond(map[string]any{"frameTree": map[string]any{"frame": map[string]any{"id": "replacement"}}})
				} else {
					respond(map[string]any{"frameTree": map[string]any{"frame": map[string]any{"id": "main"}}})
				}
			case "Page.createIsolatedWorld":
				if failWorld {
					fail("Cannot create isolated world")
				} else {
					respond(map[string]any{"executionContextId": 7})
				}
			case "Runtime.evaluate":
				respond(map[string]any{"result": map[string]any{"type": "object", "objectId": "watcher"}})
			case "Runtime.callFunctionOn":
				// the first poll reports; later ones wait for a change that never comes
				if strings.Contains(string(data), `"value":0`) {
					respond(map[string]any{"result": map[string]any{"type": "object", "value": map[string]any{
						"v": 1, "top": true, "visible": true, "focused": true, "frames": 0,
						"viewport": map[string]any{"w": 390, "h": 757, "dpr": 1, "screenW": 390, "screenH": 844},
						"fields":   []any{},
					}}})
				}
			case "Browser.getWindowForTarget":
				respond(map[string]any{"windowId": 1, "bounds": map[string]any{"left": 0, "top": 0, "width": 390, "height": 844}})
			default:
				fail("unexpected method " + request.Method)
			}
		}
	}))
	t.Cleanup(server.Close)
	return "ws" + strings.TrimPrefix(server.URL, "http")
}

func (f *fakeBrowser) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.methods[method]
}

func TestAFrameThatFailsForAWhileIsWatchedOnceItWorks(t *testing.T) {
	browser := &fakeBrowser{failWorlds: 2*injectAttempts + 1, methods: make(map[string]int)}
	w := New(&staticUpstream{url: browser.serve(t)}, slog.Default())
	defer w.Close()
	states, unsubscribe := w.Subscribe()
	defer unsubscribe()

	state := waitFor(t, states, "the page once its world can be created", func(s State) bool { return s.Viewport != nil })
	require.True(t, state.PageFocused)
	require.Equal(t, 2*injectAttempts+2, browser.count("Page.createIsolatedWorld"))
	for _, method := range []string{"Runtime.enable", "Page.enable", "DOM.enable"} {
		require.Zero(t, browser.count(method), method)
	}
}

func TestAFrameThatIsGoneIsDropped(t *testing.T) {
	browser := &fakeBrowser{failWorlds: 1 << 20, frameGone: true, methods: make(map[string]int)}
	w := New(&staticUpstream{url: browser.serve(t)}, slog.Default())
	defer w.Close()
	_, unsubscribe := w.Subscribe()
	defer unsubscribe()

	require.Eventually(t, func() bool { return browser.count("Page.createIsolatedWorld") == injectAttempts }, 5*time.Second, 10*time.Millisecond)
	time.Sleep(4 * retryDelay)
	require.Equal(t, injectAttempts, browser.count("Page.createIsolatedWorld"))
}
