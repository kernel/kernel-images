package livefocus

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/kernel/kernel-images/server/lib/cdpclient"
	"github.com/stretchr/testify/require"
)

const formPage = `<!doctype html>
<style>* { box-sizing: border-box }</style>
<body style="margin:0">
<input id="search" type="search" style="position:absolute;left:10px;top:10px;width:200px;height:30px">
<input id="email" type="email" autocomplete="username" style="position:absolute;left:10px;top:50px;width:200px;height:30px">
<input id="password" type="password" style="position:absolute;left:10px;top:90px;width:200px;height:30px">
<input id="code" inputmode="numeric" autocomplete="one-time-code" style="position:absolute;left:10px;top:130px;width:200px;height:30px">
<input id="digit" maxlength="1" style="position:absolute;left:10px;top:170px;width:30px;height:30px">
<textarea id="chat" enterkeyhint="send" autocapitalize="sentences" style="position:absolute;left:10px;top:210px;width:200px;height:60px"></textarea>
<div id="editor" contenteditable="true" style="position:absolute;left:10px;top:280px;width:200px;height:30px"><b>bold</b></div>
<button id="button" style="position:absolute;left:10px;top:320px;width:200px;height:30px">Go</button>
<input id="covered" style="position:absolute;left:300px;top:10px;width:200px;height:30px">
<div style="position:absolute;left:290px;top:0;width:300px;height:60px;background:white"></div>
<input id="disabled" disabled style="position:absolute;left:300px;top:90px;width:200px;height:30px">
<input id="below" style="position:absolute;left:10px;top:5000px;width:200px;height:30px">
<iframe id="inner" srcdoc="<body style='margin:0'><input id=nested style='box-sizing:border-box;position:absolute;left:5px;top:5px;width:100px;height:20px'></body>"
  style="position:absolute;left:300px;top:200px;width:200px;height:100px;border:2px solid black;padding:3px"></iframe>
</body>`

// Opt-in: set KERNEL_LIVEFOCUS_CHROME_E2E=1 with a chromium binary on PATH.
func TestWatcherReportsFocusAndFields(t *testing.T) {
	if os.Getenv("KERNEL_LIVEFOCUS_CHROME_E2E") == "" {
		t.Skip("set KERNEL_LIVEFOCUS_CHROME_E2E=1 to run the real-Chromium focus watcher test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	cross := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, `<body style="margin:0"><input id="remote" type="tel" style="width:100px;height:20px"></body>`)
	}))
	defer cross.Close()
	crossURL := strings.Replace(cross.URL, "127.0.0.1", "localhost", 1)
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		switch r.URL.Path {
		case "/form":
			_, _ = io.WriteString(w, formPage)
		case "/oopif":
			_, _ = fmt.Fprintf(w, `<body style="margin:0"><iframe id="frame" src="%s" style="position:absolute;left:0;top:0;width:300px;height:100px;border:0"></iframe></body>`, crossURL)
		default:
			_, _ = io.WriteString(w, `<body><p>nothing to type into</p></body>`)
		}
	}))
	defer site.Close()

	browserURL := launchChromium(t, ctx, "--site-per-process", "--window-size=800,600")
	recorder := newRecordingProxy(t, browserURL)

	driver, err := cdpclient.Dial(ctx, browserURL)
	require.NoError(t, err)
	defer driver.Close()
	page := newPage(t, ctx, driver, site.URL+"/form")
	before := page.eval(t, ctx, "Object.getOwnPropertyNames(window).sort().join(',')")

	watcher := New(&staticUpstream{url: recorder.url}, slog.Default())
	defer watcher.Close()
	states, unsubscribe := watcher.Subscribe()
	defer unsubscribe()

	state := waitFor(t, states, "the page's fields", func(s State) bool { return s.Viewport != nil && len(s.Fields) >= 7 })
	require.Nil(t, state.Focus)
	// headless Chromium draws no browser UI but still reports a window around the page
	page.matchViewport(t, ctx, driver, *state.Viewport)
	at := func(x, y, w, h float64) Rect {
		return Rect{X: state.Viewport.X + x, Y: state.Viewport.Y + y, W: w, H: h}
	}
	byPosition := map[Rect]Field{}
	for _, field := range state.Fields {
		byPosition[*field.Rect] = field
	}
	require.Equal(t, "search", byPosition[at(10, 10, 200, 30)].Type)
	require.Equal(t, "password", byPosition[at(10, 90, 200, 30)].Type)
	require.True(t, byPosition[at(10, 130, 200, 30)].Code)
	require.True(t, byPosition[at(10, 170, 30, 30)].Code, "a one-character box is a code digit")
	require.Equal(t, "textarea", byPosition[at(10, 210, 200, 60)].Type)
	require.Equal(t, "contenteditable", byPosition[at(10, 280, 200, 30)].Type)
	// the same-origin iframe's field, inside its border and padding
	require.Equal(t, "text", byPosition[at(310, 210, 100, 20)].Type)
	require.Len(t, state.Fields, 8, "the button, covered, disabled and off-screen fields are left out")

	page.click(t, ctx, 110, 65)
	state = waitFor(t, states, "focus on the email field", func(s State) bool { return s.Focus != nil })
	require.Equal(t, "email", state.Focus.Type)
	require.Equal(t, "username", state.Focus.Autocomplete)
	require.Equal(t, at(10, 50, 200, 30), *state.Focus.Rect)

	// a blur sent before a later tap on the same field is dropped
	seen := state.Focus.Seq
	page.click(t, ctx, 110, 65)
	state = waitFor(t, states, "the tap's report", func(s State) bool { return s.Focus != nil && s.Focus.Seq > seen })
	require.NoError(t, watcher.Blur(ctx, seen))
	require.Equal(t, "email", page.eval(t, ctx, "document.activeElement.id"))
	// the keyboard's Done blurs the field from the watcher's world
	require.NoError(t, watcher.Blur(ctx, state.Focus.Seq))
	waitFor(t, states, "no focus after the blur", func(s State) bool { return s.Focus == nil && s.PageFocused })
	require.Equal(t, "BODY", page.eval(t, ctx, "document.activeElement.tagName"))

	page.click(t, ctx, 110, 235)
	state = waitFor(t, states, "focus on the textarea", func(s State) bool { return s.Focus != nil && s.Focus.Type == "textarea" })
	require.Equal(t, "send", state.Focus.EnterKeyHint)
	require.Equal(t, "sentences", *state.Focus.Autocapitalize)

	page.click(t, ctx, 340, 220)
	state = waitFor(t, states, "focus in the same-origin iframe", func(s State) bool { return s.Focus != nil && s.Focus.Type == "text" })
	require.Equal(t, at(310, 210, 100, 20), *state.Focus.Rect)

	page.click(t, ctx, 110, 335)
	waitFor(t, states, "no focus after clicking the button", func(s State) bool { return s.Focus == nil })

	page.click(t, ctx, 110, 145)
	waitFor(t, states, "focus on the code field", func(s State) bool { return s.Focus != nil && s.Focus.Code })
	page.navigate(t, ctx, site.URL+"/text")
	state = waitFor(t, states, "no focus or fields after navigating", func(s State) bool {
		return s.Focus == nil && s.Viewport != nil && len(s.Fields) == 0
	})

	page.navigate(t, ctx, site.URL+"/oopif")
	waitFor(t, states, "the cross-origin frame's watcher", func(s State) bool { return s.Viewport != nil })
	time.Sleep(time.Second)
	page.click(t, ctx, 50, 10)
	state = waitFor(t, states, "focus in the cross-origin frame", func(s State) bool { return s.Focus != nil })
	require.Equal(t, "tel", state.Focus.Type)
	require.Nil(t, state.Focus.Rect, "a cross-origin frame's position is unknown")
	// The cross-origin frame is watched through its own session. Its parent's
	// frame tree does not list it, so nothing retries it there.
	worlds := recorder.count("Page.createIsolatedWorld")
	time.Sleep(2 * time.Second)
	require.Equal(t, worlds, recorder.count("Page.createIsolatedWorld"))

	page.navigate(t, ctx, site.URL+"/form")
	waitFor(t, states, "the form again", func(s State) bool { return len(s.Fields) >= 7 })
	require.Equal(t, before, page.eval(t, ctx, "Object.getOwnPropertyNames(window).sort().join(',')"),
		"the page's main world is untouched")
	for _, method := range []string{"Runtime.enable", "Page.enable", "DOM.enable", "Page.addScriptToEvaluateOnNewDocument", "Runtime.addBinding"} {
		require.Zero(t, recorder.count(method), method)
	}
}

func waitFor(t *testing.T, states <-chan []byte, what string, ok func(State) bool) State {
	t.Helper()
	deadline := time.After(10 * time.Second)
	var last []byte
	for {
		select {
		case data := <-states:
			last = data
			var state State
			require.NoError(t, json.Unmarshal(data, &state))
			if ok(state) {
				return state
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s; last state: %s", what, last)
		}
	}
}

type testPage struct {
	driver    *cdpclient.Client
	sessionID string
}

func newPage(t *testing.T, ctx context.Context, driver *cdpclient.Client, url string) *testPage {
	t.Helper()
	raw, err := driver.Send(ctx, "Target.createTarget", map[string]any{"url": "about:blank"}, "")
	require.NoError(t, err)
	var target struct {
		TargetID string `json:"targetId"`
	}
	require.NoError(t, json.Unmarshal(raw, &target))
	raw, err = driver.Send(ctx, "Target.attachToTarget", map[string]any{"targetId": target.TargetID, "flatten": true}, "")
	require.NoError(t, err)
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	require.NoError(t, json.Unmarshal(raw, &attached))
	// a headless window has no focus of its own
	_, err = driver.Send(ctx, "Emulation.setFocusEmulationEnabled", map[string]any{"enabled": true}, attached.SessionID)
	require.NoError(t, err)
	p := &testPage{driver: driver, sessionID: attached.SessionID}
	p.navigate(t, ctx, url)
	return p
}

func (p *testPage) navigate(t *testing.T, ctx context.Context, url string) {
	t.Helper()
	_, err := p.driver.Send(ctx, "Page.navigate", map[string]any{"url": url}, p.sessionID)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return p.eval(t, ctx, "document.readyState") == "complete" }, 10*time.Second, 50*time.Millisecond)
}

func (p *testPage) eval(t *testing.T, ctx context.Context, expression string) string {
	t.Helper()
	raw, err := p.driver.Send(ctx, "Runtime.evaluate", map[string]any{"expression": expression, "returnByValue": true}, p.sessionID)
	require.NoError(t, err)
	var result struct {
		Result struct {
			Value any `json:"value"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(raw, &result))
	return fmt.Sprint(result.Result.Value)
}

// matchViewport checks the reported page area against the window Chromium
// reports and the page's own size.
func (p *testPage) matchViewport(t *testing.T, ctx context.Context, driver *cdpclient.Client, viewport Rect) {
	t.Helper()
	raw, err := driver.Send(ctx, "Target.getTargetInfo", nil, p.sessionID)
	require.NoError(t, err)
	var info struct {
		TargetInfo struct {
			TargetID string `json:"targetId"`
		} `json:"targetInfo"`
	}
	require.NoError(t, json.Unmarshal(raw, &info))
	raw, err = driver.Send(ctx, "Browser.getWindowForTarget", map[string]any{"targetId": info.TargetInfo.TargetID}, "")
	require.NoError(t, err)
	var window struct {
		Bounds windowBounds `json:"bounds"`
	}
	require.NoError(t, json.Unmarshal(raw, &window))
	width := p.eval(t, ctx, "innerWidth")
	height := p.eval(t, ctx, "innerHeight")
	require.Equal(t, fmt.Sprint(viewport.W), width)
	require.Equal(t, fmt.Sprint(viewport.H), height)
	require.Equal(t, window.Bounds.Left, viewport.X)
	require.Equal(t, window.Bounds.Top+window.Bounds.Height-viewport.H, viewport.Y)
}

func (p *testPage) click(t *testing.T, ctx context.Context, x, y float64) {
	t.Helper()
	for _, kind := range []string{"mousePressed", "mouseReleased"} {
		_, err := p.driver.Send(ctx, "Input.dispatchMouseEvent", map[string]any{
			"type": kind, "x": x, "y": y, "button": "left", "clickCount": 1,
		}, p.sessionID)
		require.NoError(t, err)
	}
}

type staticUpstream struct{ url string }

func (u *staticUpstream) Current() string { return u.url }

// recordingProxy relays a DevTools connection and counts the methods its
// client sends.
type recordingProxy struct {
	url     string
	mu      sync.Mutex
	methods map[string]int
}

func newRecordingProxy(t *testing.T, upstream string) *recordingProxy {
	t.Helper()
	p := &recordingProxy{methods: make(map[string]int)}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		client, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		client.SetReadLimit(-1)
		browser, _, err := websocket.Dial(r.Context(), upstream, nil)
		if err != nil {
			client.CloseNow()
			return
		}
		browser.SetReadLimit(-1)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		relay := func(from, to *websocket.Conn, record bool) {
			defer cancel()
			for {
				kind, data, err := from.Read(ctx)
				if err != nil {
					return
				}
				if record {
					var message struct {
						Method string `json:"method"`
					}
					if json.Unmarshal(data, &message) == nil {
						p.mu.Lock()
						p.methods[message.Method]++
						p.mu.Unlock()
					}
				}
				if to.Write(ctx, kind, data) != nil {
					return
				}
			}
		}
		go relay(browser, client, false)
		relay(client, browser, true)
		client.CloseNow()
		browser.CloseNow()
	}))
	t.Cleanup(server.Close)
	p.url = "ws" + strings.TrimPrefix(server.URL, "http")
	return p
}

func (p *recordingProxy) count(method string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.methods[method]
}

func launchChromium(t *testing.T, ctx context.Context, extraArgs ...string) string {
	t.Helper()
	var chrome string
	for _, name := range []string{"chromium", "chromium-browser", "google-chrome", "chrome"} {
		if path, err := exec.LookPath(name); err == nil {
			chrome = path
			break
		}
	}
	if chrome == "" {
		t.Skip("no chromium binary found on PATH")
	}
	args := append([]string{
		"--headless=new", "--no-sandbox", "--disable-gpu",
		"--remote-debugging-port=0", "--user-data-dir=" + t.TempDir(),
	}, extraArgs...)
	cmd := exec.CommandContext(ctx, chrome, append(args, "about:blank")...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stderr, err := cmd.StderrPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	found := make(chan string, 1)
	go func() {
		buf := make([]byte, 4096)
		var out []byte
		for {
			n, err := stderr.Read(buf)
			out = append(out, buf[:n]...)
			if i := strings.Index(string(out), "ws://"); i >= 0 {
				if end := strings.IndexAny(string(out[i:]), "\r\n "); end >= 0 {
					found <- string(out[i : i+end])
					_, _ = io.Copy(io.Discard, stderr)
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	select {
	case url := <-found:
		return url
	case <-time.After(time.Minute):
		t.Fatal("chromium did not report a DevTools URL")
		return ""
	}
}
