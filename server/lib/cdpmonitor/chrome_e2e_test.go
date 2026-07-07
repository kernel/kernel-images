package cdpmonitor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/stretchr/testify/require"
)

// TestInteractionCaptureOnAlreadyLoadedPage is a real-browser integration test
// covering interaction capture on a page that is already loaded when the monitor
// attaches (equivalent to enabling the interaction category mid-session). Input
// is driven without a re-navigation, and interaction events must still be
// captured.
//
// It exercises the real Monitor against a real headless Chromium, so it covers
// the current-document injection (Runtime.evaluate in injectScript) and the
// keystroke rate-limit exemption end to end.
//
// Opt-in: set KERNEL_CDPMONITOR_CHROME_E2E=1 and have a chromium binary on PATH.
// Skips otherwise so normal unit runs never launch a browser.
func TestInteractionCaptureOnAlreadyLoadedPage(t *testing.T) {
	if os.Getenv("KERNEL_CDPMONITOR_CHROME_E2E") == "" {
		t.Skip("set KERNEL_CDPMONITOR_CHROME_E2E=1 to run the real-Chromium interaction test")
	}
	chrome := findChromium(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	browserWS := launchChromium(t, ctx, chrome)
	cdp := dialCDP(t, ctx, browserWS)
	defer cdp.close()

	// Load a page with an input and a button BEFORE the monitor attaches, so the
	// document is already live when interaction tracking is injected.
	const page = `data:text/html,<html><body><input id=name type=text style="width:300px;height:40px"/><button id=go style="width:200px;height:80px">Go</button></body></html>`
	targetID := cdp.call(t, ctx, "", "Target.createTarget", map[string]any{"url": page}).targetID(t)
	sessionID := cdp.call(t, ctx, "", "Target.attachToTarget", map[string]any{"targetId": targetID, "flatten": true}).sessionID(t)
	cdp.call(t, ctx, sessionID, "Runtime.enable", nil)
	cdp.call(t, ctx, sessionID, "Page.enable", nil)

	// Start the real monitor pointing at the same browser endpoint.
	ec := newEventCollector()
	m := New(&staticUpstream{url: browserWS}, ec.publishFn(), 99, discardLogger, nil)
	require.NoError(t, m.Start(ctx))
	defer m.Stop()

	// Wait for interaction.js to be injected into the already-loaded document.
	require.Eventually(t, func() bool {
		return cdp.evalBool(ctx, sessionID, "window.__kernelEventInjected === true")
	}, 15*time.Second, 100*time.Millisecond, "interaction.js was not injected into the already-loaded document")

	// Focus the input and drive a click plus rapid typing WITHOUT navigating.
	rect := cdp.evalRect(t, ctx, sessionID, "#go")
	cdp.call(t, ctx, sessionID, "Runtime.evaluate", map[string]any{"expression": "document.getElementById('name').focus()"})

	cdp.call(t, ctx, sessionID, "Input.dispatchMouseEvent", map[string]any{"type": "mousePressed", "x": rect.cx(), "y": rect.cy(), "button": "left", "clickCount": 1})
	cdp.call(t, ctx, sessionID, "Input.dispatchMouseEvent", map[string]any{"type": "mouseReleased", "x": rect.cx(), "y": rect.cy(), "button": "left", "clickCount": 1})

	const typed = "hello"
	for _, r := range typed {
		s := string(r)
		cdp.call(t, ctx, sessionID, "Input.dispatchKeyEvent", map[string]any{"type": "keyDown", "text": s, "key": s})
		cdp.call(t, ctx, sessionID, "Input.dispatchKeyEvent", map[string]any{"type": "keyUp", "key": s})
	}

	// The click must be captured (proves listeners are attached to the current doc).
	ec.waitFor(t, EventInteractionClick, 5*time.Second)

	// Every keystroke must be captured (proves keys are not rate-limited).
	require.Eventually(t, func() bool {
		ec.mu.Lock()
		defer ec.mu.Unlock()
		count := 0
		for _, ev := range ec.events {
			if ev.Type == EventInteractionKey {
				count++
			}
		}
		return count >= len(typed)
	}, 5*time.Second, 50*time.Millisecond, "expected all keystrokes to be captured")
}

func findChromium(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"chromium", "chromium-browser", "google-chrome", "chrome"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	t.Skip("no chromium binary found on PATH")
	return ""
}

func launchChromium(t *testing.T, ctx context.Context, chrome string) string {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.CommandContext(ctx, chrome,
		"--headless=new", "--no-sandbox", "--disable-gpu",
		"--remote-debugging-port=0", "--user-data-dir="+dir, "about:blank",
	)
	stderr, err := cmd.StderrPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })

	// Chromium prints "DevTools listening on ws://..." to stderr once ready.
	buf := make([]byte, 4096)
	deadline := time.Now().Add(20 * time.Second)
	var out []byte
	for time.Now().Before(deadline) {
		n, readErr := stderr.Read(buf)
		out = append(out, buf[:n]...)
		if url := extractDevToolsURL(out); url != "" {
			return url
		}
		if readErr != nil {
			break
		}
	}
	t.Fatalf("chromium did not report a DevTools URL; stderr:\n%s", string(out))
	return ""
}

func extractDevToolsURL(b []byte) string {
	i := strings.Index(string(b), "ws://")
	if i < 0 {
		return ""
	}
	rest := string(b)[i:]
	if end := strings.IndexAny(rest, "\r\n "); end >= 0 {
		return rest[:end]
	}
	return ""
}

// staticUpstream is an UpstreamProvider that always returns the same URL.
type staticUpstream struct{ url string }

func (u *staticUpstream) Current() string { return u.url }
func (u *staticUpstream) Subscribe() (<-chan string, func()) {
	ch := make(chan string)
	return ch, func() {}
}

// cdpConn is a minimal CDP client used only by this test to create targets and
// drive input. It is independent of the Monitor's own connection.
type cdpConn struct {
	conn    *websocket.Conn
	nextID  atomic.Int64
	mu      sync.Mutex
	replies sync.Map // id -> chan json.RawMessage
}

func dialCDP(t *testing.T, ctx context.Context, url string) *cdpConn {
	t.Helper()
	conn, _, err := websocket.Dial(ctx, url, nil)
	require.NoError(t, err)
	conn.SetReadLimit(8 * 1024 * 1024)
	c := &cdpConn{conn: conn}
	go func() {
		for {
			_, b, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var msg struct {
				ID *int64 `json:"id"`
			}
			if json.Unmarshal(b, &msg) == nil && msg.ID != nil {
				if ch, ok := c.replies.Load(*msg.ID); ok {
					ch.(chan json.RawMessage) <- b
				}
			}
		}
	}()
	return c
}

func (c *cdpConn) close() { _ = c.conn.Close(websocket.StatusNormalClosure, "") }

type cdpResult struct {
	t   *testing.T
	raw json.RawMessage
}

func (c *cdpConn) call(t *testing.T, ctx context.Context, sessionID, method string, params map[string]any) cdpResult {
	t.Helper()
	id := c.nextID.Add(1)
	ch := make(chan json.RawMessage, 1)
	c.replies.Store(id, ch)
	defer c.replies.Delete(id)

	msg := map[string]any{"id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	if sessionID != "" {
		msg["sessionId"] = sessionID
	}
	c.mu.Lock()
	err := wsjson.Write(ctx, c.conn, msg)
	c.mu.Unlock()
	require.NoError(t, err)

	select {
	case raw := <-ch:
		return cdpResult{t: t, raw: raw}
	case <-ctx.Done():
		t.Fatalf("cdp call %s timed out", method)
		return cdpResult{}
	}
}

func (r cdpResult) targetID(t *testing.T) string {
	var resp struct {
		Result struct {
			TargetID string `json:"targetId"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(r.raw, &resp))
	require.NotEmpty(t, resp.Result.TargetID)
	return resp.Result.TargetID
}

func (r cdpResult) sessionID(t *testing.T) string {
	var resp struct {
		Result struct {
			SessionID string `json:"sessionId"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(r.raw, &resp))
	require.NotEmpty(t, resp.Result.SessionID)
	return resp.Result.SessionID
}

func (c *cdpConn) evalBool(ctx context.Context, sessionID, expr string) bool {
	id := c.nextID.Add(1)
	ch := make(chan json.RawMessage, 1)
	c.replies.Store(id, ch)
	defer c.replies.Delete(id)
	c.mu.Lock()
	_ = wsjson.Write(ctx, c.conn, map[string]any{
		"id": id, "sessionId": sessionID, "method": "Runtime.evaluate",
		"params": map[string]any{"expression": expr, "returnByValue": true},
	})
	c.mu.Unlock()
	select {
	case raw := <-ch:
		var resp struct {
			Result struct {
				Result struct {
					Value bool `json:"value"`
				} `json:"result"`
			} `json:"result"`
		}
		_ = json.Unmarshal(raw, &resp)
		return resp.Result.Result.Value
	case <-ctx.Done():
		return false
	}
}

type domRect struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"width"`
	H float64 `json:"height"`
}

func (r domRect) cx() float64 { return r.X + r.W/2 }
func (r domRect) cy() float64 { return r.Y + r.H/2 }

func (c *cdpConn) evalRect(t *testing.T, ctx context.Context, sessionID, selector string) domRect {
	t.Helper()
	expr := fmt.Sprintf("(function(){var r=document.querySelector(%q).getBoundingClientRect();return JSON.stringify({x:r.x,y:r.y,width:r.width,height:r.height});})()", selector)
	res := c.call(t, ctx, sessionID, "Runtime.evaluate", map[string]any{"expression": expr, "returnByValue": true})
	var resp struct {
		Result struct {
			Result struct {
				Value string `json:"value"`
			} `json:"result"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(res.raw, &resp))
	var rect domRect
	require.NoError(t, json.Unmarshal([]byte(resp.Result.Result.Value), &rect))
	return rect
}
