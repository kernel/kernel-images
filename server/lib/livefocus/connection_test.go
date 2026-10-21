package livefocus

import (
	"context"
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
// main frame is gone from the frame tree after the first scan. With oopif, the
// tab has an out-of-process iframe that its frame tree lists too.
type fakeBrowser struct {
	mu         sync.Mutex
	failWorlds int
	frameGone  bool
	oopif      bool
	// the first report has a focused text field
	focused bool
	methods map[string]int
	worlds  map[string]int
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
				Params    struct {
					TargetID string `json:"targetId"`
					FrameID  string `json:"frameId"`
					ObjectID string `json:"objectId"`
				} `json:"params"`
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
			if request.Method == "Page.createIsolatedWorld" {
				f.worlds[request.SessionID+"/"+request.Params.FrameID]++
			}
			// the parent's session cannot reach a frame in another process
			remoteFrame := request.SessionID == "session" && request.Params.FrameID == "oopif"
			oopif := f.oopif
			frameGone := f.frameGone && f.methods["Page.getFrameTree"] > 1
			f.mu.Unlock()

			switch request.Method {
			case "Target.setDiscoverTargets":
				respond(map[string]any{})
			case "Target.getTargets":
				targets := []map[string]any{{"targetId": "tab", "type": "page", "url": "https://example.com/"}}
				if oopif {
					targets = append(targets, map[string]any{"targetId": "oopif", "type": "iframe", "url": "https://other.example/"})
				}
				respond(map[string]any{"targetInfos": targets})
			case "Target.attachToTarget":
				if request.Params.TargetID == "oopif" {
					respond(map[string]any{"sessionId": "oopif-session"})
				} else {
					respond(map[string]any{"sessionId": "session"})
				}
			case "Page.getFrameTree":
				switch {
				case request.SessionID == "oopif-session":
					respond(map[string]any{"frameTree": map[string]any{"frame": map[string]any{"id": "oopif"}}})
				case frameGone:
					respond(map[string]any{"frameTree": map[string]any{"frame": map[string]any{"id": "replacement"}}})
				case oopif:
					respond(map[string]any{"frameTree": map[string]any{
						"frame": map[string]any{"id": "main"}, "childFrames": []any{map[string]any{"frame": map[string]any{"id": "oopif"}}},
					}})
				default:
					respond(map[string]any{"frameTree": map[string]any{"frame": map[string]any{"id": "main"}}})
				}
			case "Page.createIsolatedWorld":
				if failWorld || remoteFrame {
					fail("Cannot create isolated world")
				} else {
					respond(map[string]any{"executionContextId": 7})
				}
			case "Runtime.evaluate":
				respond(map[string]any{"result": map[string]any{"type": "object", "objectId": "watcher"}})
			case "Runtime.callFunctionOn":
				switch {
				case strings.Contains(string(data), "this.blur("):
					f.mu.Lock()
					f.methods["blur"]++
					f.methods["blur "+request.SessionID+" "+request.Params.ObjectID]++
					f.mu.Unlock()
					respond(map[string]any{"result": map[string]any{"type": "boolean", "value": true}})
				// the first poll reports; later ones wait for a change that never comes
				case strings.Contains(string(data), `"value":0`):
					report := map[string]any{
						"v": 1, "top": true, "visible": true, "focused": true, "frames": 0,
						"viewport": map[string]any{"w": 390, "h": 757, "dpr": 1, "screenW": 390, "screenH": 844},
						"fields":   []any{},
					}
					if f.focused {
						report["focus"] = map[string]any{"type": "text"}
					}
					respond(map[string]any{"result": map[string]any{"type": "object", "value": report}})
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

func newFakeBrowser() *fakeBrowser {
	return &fakeBrowser{methods: make(map[string]int), worlds: make(map[string]int)}
}

func (f *fakeBrowser) worldsFor(session, frame string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.worlds[session+"/"+frame]
}

func (f *fakeBrowser) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.methods[method]
}

func TestAFrameThatFailsForAWhileIsWatchedOnceItWorks(t *testing.T) {
	browser := newFakeBrowser()
	browser.failWorlds = 2*injectAttempts + 1
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
	browser := newFakeBrowser()
	browser.failWorlds = 1 << 20
	browser.frameGone = true
	w := New(&staticUpstream{url: browser.serve(t)}, slog.Default())
	defer w.Close()
	_, unsubscribe := w.Subscribe()
	defer unsubscribe()

	require.Eventually(t, func() bool { return browser.count("Page.createIsolatedWorld") == injectAttempts }, 5*time.Second, 10*time.Millisecond)
	time.Sleep(4 * retryDelay)
	require.Equal(t, injectAttempts, browser.count("Page.createIsolatedWorld"))
}

func TestAnOutOfProcessFrameIsWatchedThroughItsOwnSession(t *testing.T) {
	browser := newFakeBrowser()
	browser.oopif = true
	w := New(&staticUpstream{url: browser.serve(t)}, slog.Default())
	defer w.Close()
	_, unsubscribe := w.Subscribe()
	defer unsubscribe()

	require.Eventually(t, func() bool { return browser.worldsFor("oopif-session", "oopif") > 0 }, 5*time.Second, 10*time.Millisecond)
	time.Sleep(10 * retryDelay)
	require.LessOrEqual(t, browser.worldsFor("session", "oopif"), injectAttempts)
}

func TestBlurGoesToTheFocusedFramesWatcher(t *testing.T) {
	browser := newFakeBrowser()
	browser.focused = true
	w := New(&staticUpstream{url: browser.serve(t)}, slog.Default())
	defer w.Close()
	states, unsubscribe := w.Subscribe()
	defer unsubscribe()
	waitFor(t, states, "the focused field", func(s State) bool { return s.Focus != nil })

	require.NoError(t, w.Blur(context.Background(), 0))
	require.Equal(t, 1, browser.count("blur session watcher"))
}

func TestBlurWithNothingFocusedSendsNothing(t *testing.T) {
	browser := newFakeBrowser()
	w := New(&staticUpstream{url: browser.serve(t)}, slog.Default())
	defer w.Close()
	states, unsubscribe := w.Subscribe()
	defer unsubscribe()
	waitFor(t, states, "the page", func(s State) bool { return s.Viewport != nil })

	require.NoError(t, w.Blur(context.Background(), 0))
	require.Zero(t, browser.count("blur"))
}

func TestAnEndedConnectionPublishesNothing(t *testing.T) {
	browser := newFakeBrowser()
	browser.focused = true
	w := New(&staticUpstream{url: browser.serve(t)}, slog.Default())
	defer w.Close()
	states, unsubscribe := w.Subscribe()
	defer unsubscribe()
	waitFor(t, states, "the focused field", func(s State) bool { return s.Focus != nil })

	w.mu.Lock()
	c := w.conn
	w.mu.Unlock()
	require.NotNil(t, c)
	// the connection drops; its last frame states are still around
	_ = c.protocol.Close()
	waitFor(t, states, "the empty state", func(s State) bool { return s.Focus == nil && s.Viewport == nil })
	// a frame poll still draining reports its last state
	c.mu.Lock()
	c.frames[frameKey{session: "session", frame: "main"}] = &watchedFrame{state: frameState{
		seq:    1,
		report: &frameReport{Focused: true, Focus: &Field{Type: "text"}},
	}}
	c.mu.Unlock()
	c.emit()
	w.mu.Lock()
	latest := string(w.latest)
	w.mu.Unlock()
	require.NotContains(t, latest, `"focus":{`)
}
