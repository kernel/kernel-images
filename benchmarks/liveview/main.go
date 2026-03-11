package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// ---------------------------------------------------------------------------
// CDP client
// ---------------------------------------------------------------------------

type cdpMsg struct {
	ID        int             `json:"id"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Error     *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type cdp struct {
	ws        *websocket.Conn
	nextID    int
	sessionID string // set for session-mode (proxy) connections
	log       *slog.Logger
}

func discoverPageWSURL(baseURL string) (string, error) {
	resp, err := http.Get(baseURL + "/json/list")
	if err != nil {
		return "", fmt.Errorf("GET /json/list: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var targets []struct {
		Type                 string `json:"type"`
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.Unmarshal(body, &targets); err != nil {
		return "", fmt.Errorf("parse /json/list: %w", err)
	}
	for _, t := range targets {
		if t.Type == "page" {
			return t.WebSocketDebuggerURL, nil
		}
	}
	return "", fmt.Errorf("no page targets found in %d targets", len(targets))
}

func dialCDP(ctx context.Context, url string, log *slog.Logger) (*cdp, error) {
	ws, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", url, err)
	}
	ws.SetReadLimit(64 * 1024 * 1024)
	return &cdp{ws: ws, nextID: 1, log: log}, nil
}

func (c *cdp) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.nextID
	c.nextID++

	var rawParams json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		rawParams = b
	}

	outMsg := cdpMsg{ID: id, Method: method, Params: rawParams, SessionID: c.sessionID}
	msg, _ := json.Marshal(outMsg)
	if err := c.ws.Write(ctx, websocket.MessageText, msg); err != nil {
		return nil, fmt.Errorf("write %s: %w", method, err)
	}

	for {
		_, data, err := c.ws.Read(ctx)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", method, err)
		}
		var resp cdpMsg
		if err := json.Unmarshal(data, &resp); err != nil {
			continue
		}
		if resp.ID == id {
			if resp.Error != nil {
				return nil, fmt.Errorf("CDP %s: code=%d msg=%s", method, resp.Error.Code, resp.Error.Message)
			}
			return resp.Result, nil
		}
	}
}

func (c *cdp) close() {
	c.ws.Close(websocket.StatusNormalClosure, "")
}

// ---------------------------------------------------------------------------
// Stats
// ---------------------------------------------------------------------------

type stats struct {
	samples []time.Duration
}

func (s *stats) add(d time.Duration) {
	s.samples = append(s.samples, d)
}

func (s *stats) compute() (min, max, median, p95, mean time.Duration) {
	n := len(s.samples)
	if n == 0 {
		return
	}
	sorted := make([]time.Duration, n)
	copy(sorted, s.samples)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	min = sorted[0]
	max = sorted[n-1]
	median = sorted[n/2]
	p95 = sorted[int(math.Ceil(float64(n)*0.95))-1]

	var total time.Duration
	for _, d := range sorted {
		total += d
	}
	mean = total / time.Duration(n)
	return
}

func fmtDur(d time.Duration) string {
	if d < time.Millisecond {
		return fmt.Sprintf("%.0fµs", float64(d.Microseconds()))
	}
	if d < time.Second {
		return fmt.Sprintf("%.1fms", float64(d.Microseconds())/1000)
	}
	return fmt.Sprintf("%.2fs", d.Seconds())
}

// ---------------------------------------------------------------------------
// Test sites — diverse mix of page weight, JS complexity, layout
// ---------------------------------------------------------------------------

type testSite struct {
	URL      string
	Category string // "static", "content", "spa", "media", "complex"
}

var testSites = []testSite{
	{"https://example.com", "static"},
	{"https://news.ycombinator.com", "content"},
	{"https://en.wikipedia.org/wiki/Web_browser", "content"},
	{"https://github.com/nicknisi/dotfiles", "spa"},
	{"https://www.bbc.com/news", "media"},
}

// ---------------------------------------------------------------------------
// Individual benchmark operations
// ---------------------------------------------------------------------------

type benchOp struct {
	Name string
	Fn   func(ctx context.Context, c *cdp) (time.Duration, error)
}

func navigateAndWait(ctx context.Context, c *cdp, url string) (time.Duration, error) {
	start := time.Now()
	_, err := c.call(ctx, "Page.navigate", map[string]string{"url": url})
	if err != nil {
		return 0, err
	}
	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		_, data, err := c.ws.Read(waitCtx)
		if err != nil {
			return time.Since(start), fmt.Errorf("timeout waiting for load: %w", err)
		}
		var ev cdpMsg
		json.Unmarshal(data, &ev)
		if ev.Method == "Page.loadEventFired" {
			break
		}
	}
	return time.Since(start), nil
}

func timeCall(ctx context.Context, c *cdp, method string, params any) (time.Duration, error) {
	start := time.Now()
	_, err := c.call(ctx, method, params)
	return time.Since(start), err
}

func timeCallResult(ctx context.Context, c *cdp, method string, params any) (json.RawMessage, time.Duration, error) {
	start := time.Now()
	result, err := c.call(ctx, method, params)
	return result, time.Since(start), err
}

func screenshotOps() []benchOp {
	return []benchOp{
		{"Screenshot.JPEG.q80", func(ctx context.Context, c *cdp) (time.Duration, error) {
			return timeCall(ctx, c, "Page.captureScreenshot", map[string]any{
				"format": "jpeg", "quality": 80,
			})
		}},
		{"Screenshot.PNG", func(ctx context.Context, c *cdp) (time.Duration, error) {
			return timeCall(ctx, c, "Page.captureScreenshot", map[string]any{
				"format": "png",
			})
		}},
		{"Screenshot.FullPage", func(ctx context.Context, c *cdp) (time.Duration, error) {
			return timeCall(ctx, c, "Page.captureScreenshot", map[string]any{
				"format": "jpeg", "quality": 80, "captureBeyondViewport": true,
			})
		}},
		{"Screenshot.ClipRegion", func(ctx context.Context, c *cdp) (time.Duration, error) {
			return timeCall(ctx, c, "Page.captureScreenshot", map[string]any{
				"format": "jpeg", "quality": 80,
				"clip": map[string]any{"x": 0, "y": 0, "width": 400, "height": 300, "scale": 1},
			})
		}},
	}
}

func jsEvalOps() []benchOp {
	return []benchOp{
		{"Eval.Trivial", func(ctx context.Context, c *cdp) (time.Duration, error) {
			return timeCall(ctx, c, "Runtime.evaluate", map[string]any{
				"expression": "1 + 1", "returnByValue": true,
			})
		}},
		{"Eval.QuerySelectorAll", func(ctx context.Context, c *cdp) (time.Duration, error) {
			return timeCall(ctx, c, "Runtime.evaluate", map[string]any{
				"expression": "document.querySelectorAll('*').length", "returnByValue": true,
			})
		}},
		{"Eval.InnerText", func(ctx context.Context, c *cdp) (time.Duration, error) {
			return timeCall(ctx, c, "Runtime.evaluate", map[string]any{
				"expression": "document.body.innerText.length", "returnByValue": true,
			})
		}},
		{"Eval.GetComputedStyle", func(ctx context.Context, c *cdp) (time.Duration, error) {
			return timeCall(ctx, c, "Runtime.evaluate", map[string]any{
				"expression":  "JSON.stringify(window.getComputedStyle(document.body))",
				"returnByValue": true,
			})
		}},
		{"Eval.ScrollToBottom", func(ctx context.Context, c *cdp) (time.Duration, error) {
			return timeCall(ctx, c, "Runtime.evaluate", map[string]any{
				"expression": "window.scrollTo(0, document.body.scrollHeight); window.scrollY",
				"returnByValue": true,
			})
		}},
		{"Eval.DOMManipulation", func(ctx context.Context, c *cdp) (time.Duration, error) {
			return timeCall(ctx, c, "Runtime.evaluate", map[string]any{
				"expression": `(() => {
					const div = document.createElement('div');
					div.id = 'bench-test';
					div.innerHTML = '<p>benchmark</p>'.repeat(100);
					document.body.appendChild(div);
					const result = div.children.length;
					div.remove();
					return result;
				})()`,
				"returnByValue": true,
			})
		}},
		{"Eval.BoundingRects", func(ctx context.Context, c *cdp) (time.Duration, error) {
			return timeCall(ctx, c, "Runtime.evaluate", map[string]any{
				"expression": `(() => {
					const els = document.querySelectorAll('a, button, input, [role="button"]');
					const rects = [];
					for (let i = 0; i < Math.min(els.length, 50); i++) {
						const r = els[i].getBoundingClientRect();
						rects.push({x: r.x, y: r.y, w: r.width, h: r.height});
					}
					return rects.length;
				})()`,
				"returnByValue": true,
			})
		}},
	}
}

func domOps() []benchOp {
	return []benchOp{
		{"DOM.GetDocument.Shallow", func(ctx context.Context, c *cdp) (time.Duration, error) {
			return timeCall(ctx, c, "DOM.getDocument", map[string]any{"depth": 1})
		}},
		{"DOM.GetDocument.Deep", func(ctx context.Context, c *cdp) (time.Duration, error) {
			return timeCall(ctx, c, "DOM.getDocument", map[string]any{"depth": 5})
		}},
		{"DOM.GetDocument.Full", func(ctx context.Context, c *cdp) (time.Duration, error) {
			return timeCall(ctx, c, "DOM.getDocument", map[string]any{"depth": -1})
		}},
		{"DOM.QuerySelector", func(ctx context.Context, c *cdp) (time.Duration, error) {
			result, _, err := timeCallResult(ctx, c, "DOM.getDocument", map[string]any{"depth": 0})
			if err != nil {
				return 0, err
			}
			var doc struct {
				Root struct {
					NodeID int `json:"nodeId"`
				} `json:"root"`
			}
			json.Unmarshal(result, &doc)
			return timeCall(ctx, c, "DOM.querySelector", map[string]any{
				"nodeId": doc.Root.NodeID, "selector": "body",
			})
		}},
		{"DOM.GetOuterHTML", func(ctx context.Context, c *cdp) (time.Duration, error) {
			result, _, err := timeCallResult(ctx, c, "DOM.getDocument", map[string]any{"depth": 0})
			if err != nil {
				return 0, err
			}
			var doc struct {
				Root struct {
					NodeID int `json:"nodeId"`
				} `json:"root"`
			}
			json.Unmarshal(result, &doc)
			return timeCall(ctx, c, "DOM.getOuterHTML", map[string]any{
				"nodeId": doc.Root.NodeID,
			})
		}},
	}
}

func inputOps() []benchOp {
	return []benchOp{
		{"Input.MouseMove", func(ctx context.Context, c *cdp) (time.Duration, error) {
			return timeCall(ctx, c, "Input.dispatchMouseEvent", map[string]any{
				"type": "mouseMoved", "x": 500, "y": 400,
			})
		}},
		{"Input.Click", func(ctx context.Context, c *cdp) (time.Duration, error) {
			start := time.Now()
			c.call(ctx, "Input.dispatchMouseEvent", map[string]any{
				"type": "mousePressed", "x": 500, "y": 400, "button": "left", "clickCount": 1,
			})
			_, err := c.call(ctx, "Input.dispatchMouseEvent", map[string]any{
				"type": "mouseReleased", "x": 500, "y": 400, "button": "left", "clickCount": 1,
			})
			return time.Since(start), err
		}},
		{"Input.TypeText", func(ctx context.Context, c *cdp) (time.Duration, error) {
			text := "hello world benchmark"
			start := time.Now()
			for _, ch := range text {
				s := string(ch)
				c.call(ctx, "Input.dispatchKeyEvent", map[string]any{
					"type": "keyDown", "key": s, "text": s,
				})
				c.call(ctx, "Input.dispatchKeyEvent", map[string]any{
					"type": "keyUp", "key": s,
				})
			}
			return time.Since(start), nil
		}},
		{"Input.Scroll", func(ctx context.Context, c *cdp) (time.Duration, error) {
			start := time.Now()
			for i := 0; i < 5; i++ {
				c.call(ctx, "Input.dispatchMouseEvent", map[string]any{
					"type": "mouseWheel", "x": 500, "y": 400, "deltaX": 0, "deltaY": 300,
				})
			}
			return time.Since(start), nil
		}},
	}
}

func networkOps() []benchOp {
	return []benchOp{
		{"Network.GetCookies", func(ctx context.Context, c *cdp) (time.Duration, error) {
			return timeCall(ctx, c, "Network.getCookies", nil)
		}},
		{"Network.GetResponseBody", func(ctx context.Context, c *cdp) (time.Duration, error) {
			return timeCall(ctx, c, "Runtime.evaluate", map[string]any{
				"expression":    "fetch(location.href).then(r => r.text()).then(t => t.length)",
				"awaitPromise":  true,
				"returnByValue": true,
			})
		}},
	}
}

func pageOps() []benchOp {
	return []benchOp{
		{"Page.Reload", func(ctx context.Context, c *cdp) (time.Duration, error) {
			start := time.Now()
			_, err := c.call(ctx, "Page.reload", nil)
			if err != nil {
				return 0, err
			}
			waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			for {
				_, data, err := c.ws.Read(waitCtx)
				if err != nil {
					return time.Since(start), nil
				}
				var ev cdpMsg
				json.Unmarshal(data, &ev)
				if ev.Method == "Page.loadEventFired" {
					break
				}
			}
			return time.Since(start), nil
		}},
		{"Page.GetNavigationHistory", func(ctx context.Context, c *cdp) (time.Duration, error) {
			return timeCall(ctx, c, "Page.getNavigationHistory", nil)
		}},
		{"Page.GetLayoutMetrics", func(ctx context.Context, c *cdp) (time.Duration, error) {
			return timeCall(ctx, c, "Page.getLayoutMetrics", nil)
		}},
		{"Page.PrintToPDF", func(ctx context.Context, c *cdp) (time.Duration, error) {
			return timeCall(ctx, c, "Page.printToPDF", map[string]any{
				"landscape": false, "printBackground": true,
			})
		}},
	}
}

func emulationOps() []benchOp {
	return []benchOp{
		{"Emulation.SetViewport", func(ctx context.Context, c *cdp) (time.Duration, error) {
			return timeCall(ctx, c, "Emulation.setDeviceMetricsOverride", map[string]any{
				"width": 1920, "height": 1080, "deviceScaleFactor": 1, "mobile": false,
			})
		}},
		{"Emulation.SetMobile", func(ctx context.Context, c *cdp) (time.Duration, error) {
			start := time.Now()
			c.call(ctx, "Emulation.setDeviceMetricsOverride", map[string]any{
				"width": 375, "height": 812, "deviceScaleFactor": 3, "mobile": true,
			})
			// Reset back
			_, err := c.call(ctx, "Emulation.setDeviceMetricsOverride", map[string]any{
				"width": 1920, "height": 1080, "deviceScaleFactor": 1, "mobile": false,
			})
			return time.Since(start), err
		}},
		{"Emulation.SetGeolocation", func(ctx context.Context, c *cdp) (time.Duration, error) {
			return timeCall(ctx, c, "Emulation.setGeolocationOverride", map[string]any{
				"latitude": 37.7749, "longitude": -122.4194, "accuracy": 100,
			})
		}},
	}
}

func targetOps() []benchOp {
	return []benchOp{
		{"Target.GetTargets", func(ctx context.Context, c *cdp) (time.Duration, error) {
			return timeCall(ctx, c, "Target.getTargets", nil)
		}},
		{"Target.CreateAndClose", func(ctx context.Context, c *cdp) (time.Duration, error) {
			start := time.Now()
			result, err := c.call(ctx, "Target.createTarget", map[string]string{
				"url": "about:blank",
			})
			if err != nil {
				return 0, err
			}
			var created struct {
				TargetID string `json:"targetId"`
			}
			json.Unmarshal(result, &created)
			if created.TargetID != "" {
				c.call(ctx, "Target.closeTarget", map[string]string{
					"targetId": created.TargetID,
				})
			}
			return time.Since(start), nil
		}},
	}
}

// Composite scenarios that chain multiple CDP calls like a real automation would.
func compositeOps() []benchOp {
	return []benchOp{
		{"Composite.NavAndScreenshot", func(ctx context.Context, c *cdp) (time.Duration, error) {
			start := time.Now()
			_, err := c.call(ctx, "Page.navigate", map[string]string{"url": "https://example.com"})
			if err != nil {
				return 0, err
			}
			waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			for {
				_, data, err := c.ws.Read(waitCtx)
				if err != nil {
					break
				}
				var ev cdpMsg
				json.Unmarshal(data, &ev)
				if ev.Method == "Page.loadEventFired" {
					break
				}
			}
			c.call(ctx, "Page.captureScreenshot", map[string]any{
				"format": "jpeg", "quality": 80,
			})
			return time.Since(start), nil
		}},
		{"Composite.ScrapeLinks", func(ctx context.Context, c *cdp) (time.Duration, error) {
			start := time.Now()
			c.call(ctx, "Runtime.evaluate", map[string]any{
				"expression": `Array.from(document.querySelectorAll('a[href]')).map(a => ({
					text: a.textContent.trim().slice(0, 100),
					href: a.href
				}))`,
				"returnByValue": true,
			})
			return time.Since(start), nil
		}},
		{"Composite.FillForm", func(ctx context.Context, c *cdp) (time.Duration, error) {
			start := time.Now()
			// Inject a form, fill it, read it back
			c.call(ctx, "Runtime.evaluate", map[string]any{
				"expression": `(() => {
					const form = document.createElement('form');
					form.id = 'bench-form';
					form.innerHTML = '<input name="email" type="email"><input name="pass" type="password"><button type="submit">Go</button>';
					document.body.appendChild(form);
				})()`,
			})
			c.call(ctx, "Runtime.evaluate", map[string]any{
				"expression": `document.querySelector('#bench-form input[name="email"]').value = "test@example.com"`,
			})
			c.call(ctx, "Runtime.evaluate", map[string]any{
				"expression": `document.querySelector('#bench-form input[name="pass"]').value = "hunter2"`,
			})
			_, err := c.call(ctx, "Runtime.evaluate", map[string]any{
				"expression":    `document.querySelector('#bench-form input[name="email"]').value`,
				"returnByValue": true,
			})
			// Cleanup
			c.call(ctx, "Runtime.evaluate", map[string]any{
				"expression": `document.querySelector('#bench-form')?.remove()`,
			})
			return time.Since(start), err
		}},
		{"Composite.ClickAndWait", func(ctx context.Context, c *cdp) (time.Duration, error) {
			start := time.Now()
			// Find first link, click it, wait for navigation
			c.call(ctx, "Runtime.evaluate", map[string]any{
				"expression": `(() => {
					const link = document.querySelector('a[href^="http"]');
					if (link) link.click();
				})()`,
			})
			waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			for {
				_, data, err := c.ws.Read(waitCtx)
				if err != nil {
					break
				}
				var ev cdpMsg
				json.Unmarshal(data, &ev)
				if ev.Method == "Page.loadEventFired" || ev.Method == "Page.frameNavigated" {
					break
				}
			}
			return time.Since(start), nil
		}},
		{"Composite.RapidScreenshots", func(ctx context.Context, c *cdp) (time.Duration, error) {
			start := time.Now()
			for i := 0; i < 10; i++ {
				c.call(ctx, "Page.captureScreenshot", map[string]any{
					"format": "jpeg", "quality": 60,
				})
			}
			return time.Since(start), nil
		}},
		{"Composite.ScrollAndCapture", func(ctx context.Context, c *cdp) (time.Duration, error) {
			start := time.Now()
			for i := 0; i < 5; i++ {
				c.call(ctx, "Runtime.evaluate", map[string]any{
					"expression":    fmt.Sprintf("window.scrollTo(0, %d); window.scrollY", i*500),
					"returnByValue": true,
				})
				time.Sleep(50 * time.Millisecond)
				c.call(ctx, "Page.captureScreenshot", map[string]any{
					"format": "jpeg", "quality": 80,
				})
			}
			return time.Since(start), nil
		}},
	}
}

// ---------------------------------------------------------------------------
// Benchmark runner
// ---------------------------------------------------------------------------

type benchResult struct {
	Category string
	Op       string
	Stats    *stats
}

func allOps() []struct {
	category string
	ops      []benchOp
} {
	return []struct {
		category string
		ops      []benchOp
	}{
		{"Screenshot", screenshotOps()},
		{"JS Evaluation", jsEvalOps()},
		{"DOM", domOps()},
		{"Input", inputOps()},
		{"Network", networkOps()},
		{"Page", pageOps()},
		{"Emulation", emulationOps()},
		{"Target", targetOps()},
		{"Composite", compositeOps()},
	}
}

func enableDomains(ctx context.Context, c *cdp) {
	c.call(ctx, "Page.enable", nil)
	c.call(ctx, "DOM.enable", nil)
	c.call(ctx, "Runtime.enable", nil)
	c.call(ctx, "Network.enable", nil)
}

func runBenchmark(ctx context.Context, c *cdp, iterations int, log *slog.Logger) []benchResult {
	enableDomains(ctx, c)

	var results []benchResult
	categories := allOps()

	for siteIdx := 0; siteIdx < len(testSites); siteIdx++ {
		site := testSites[siteIdx]
		log.Info("navigating to test site", "url", site.URL, "category", site.Category)

		d, err := navigateAndWait(ctx, c, site.URL)
		if err != nil {
			log.Error("navigate failed, skipping site", "url", site.URL, "error", err)
			continue
		}

		navKey := fmt.Sprintf("Navigate[%s]", site.Category)
		found := false
		for i := range results {
			if results[i].Op == navKey {
				results[i].Stats.add(d)
				found = true
				break
			}
		}
		if !found {
			s := &stats{}
			s.add(d)
			results = append(results, benchResult{Category: "Navigation", Op: navKey, Stats: s})
		}

		time.Sleep(500 * time.Millisecond)

		for _, cat := range categories {
			for _, op := range cat.ops {
				key := op.Name
				var st *stats
				for i := range results {
					if results[i].Op == key {
						st = results[i].Stats
						break
					}
				}
				if st == nil {
					st = &stats{}
					results = append(results, benchResult{Category: cat.category, Op: key, Stats: st})
				}

				d, err := op.Fn(ctx, c)
				if err != nil {
					log.Warn("op failed", "op", key, "site", site.URL, "error", err)
					continue
				}
				st.add(d)
			}
		}
	}

	// Run extra iterations cycling through sites
	for iter := 1; iter < iterations; iter++ {
		site := testSites[iter%len(testSites)]
		log.Info("iteration", "i", iter+1, "of", iterations, "url", site.URL)

		d, err := navigateAndWait(ctx, c, site.URL)
		if err != nil {
			log.Error("navigate failed", "error", err)
			continue
		}
		navKey := fmt.Sprintf("Navigate[%s]", site.Category)
		for i := range results {
			if results[i].Op == navKey {
				results[i].Stats.add(d)
				break
			}
		}

		time.Sleep(300 * time.Millisecond)

		for _, cat := range categories {
			for _, op := range cat.ops {
				for i := range results {
					if results[i].Op == op.Name {
						d, err := op.Fn(ctx, c)
						if err != nil {
							continue
						}
						results[i].Stats.add(d)
						break
					}
				}
			}
		}
	}

	return results
}

// ---------------------------------------------------------------------------
// Concurrent load test — fire CDP calls in parallel to stress-test contention
// ---------------------------------------------------------------------------

func runConcurrentBench(ctx context.Context, cdpBase string, sessionMode bool, workers int, duration time.Duration, log *slog.Logger) []benchResult {
	type sample struct {
		op string
		d  time.Duration
	}

	var mu sync.Mutex
	var allSamples []sample
	deadline := time.Now().Add(duration)

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			var c *cdp
			var err error
			if sessionMode {
				c, err = connectViaSession(ctx, cdpBase, log)
			} else {
				c, err = connectToPage(ctx, cdpBase, log)
			}
			if err != nil {
				log.Error("worker dial failed", "worker", workerID, "error", err)
				return
			}
			defer c.close()
			enableDomains(ctx, c)

			for time.Now().Before(deadline) {
				d, err := timeCall(ctx, c, "Page.captureScreenshot", map[string]any{
					"format": "jpeg", "quality": 80,
				})
				if err == nil {
					mu.Lock()
					allSamples = append(allSamples, sample{"Concurrent.Screenshot", d})
					mu.Unlock()
				}

				d, err = timeCall(ctx, c, "Runtime.evaluate", map[string]any{
					"expression": "document.title", "returnByValue": true,
				})
				if err == nil {
					mu.Lock()
					allSamples = append(allSamples, sample{"Concurrent.Evaluate", d})
					mu.Unlock()
				}

				d, err = timeCall(ctx, c, "DOM.getDocument", map[string]any{"depth": 2})
				if err == nil {
					mu.Lock()
					allSamples = append(allSamples, sample{"Concurrent.DOM", d})
					mu.Unlock()
				}
			}
		}(w)
	}
	wg.Wait()

	grouped := map[string]*stats{}
	for _, s := range allSamples {
		if grouped[s.op] == nil {
			grouped[s.op] = &stats{}
		}
		grouped[s.op].add(s.d)
	}

	var results []benchResult
	for op, st := range grouped {
		results = append(results, benchResult{Category: "Concurrent", Op: op, Stats: st})
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Op < results[j].Op })
	return results
}

// ---------------------------------------------------------------------------
// Output
// ---------------------------------------------------------------------------

func printTable(label string, results []benchResult) {
	fmt.Printf("\n## %s\n\n", label)

	currentCat := ""
	fmt.Printf("| %-32s | %9s | %9s | %9s | %9s | %9s | %5s |\n",
		"Operation", "Min", "Median", "Mean", "P95", "Max", "N")
	fmt.Printf("|%s|%s|%s|%s|%s|%s|%s|\n",
		strings.Repeat("-", 34),
		strings.Repeat("-", 11),
		strings.Repeat("-", 11),
		strings.Repeat("-", 11),
		strings.Repeat("-", 11),
		strings.Repeat("-", 11),
		strings.Repeat("-", 7))

	for _, r := range results {
		if r.Category != currentCat {
			currentCat = r.Category
			fmt.Printf("| **%-30s** | %9s | %9s | %9s | %9s | %9s | %5s |\n",
				currentCat, "", "", "", "", "", "")
		}
		s := r.Stats
		if len(s.samples) == 0 {
			continue
		}
		min, max, median, p95, mean := s.compute()
		fmt.Printf("| %-32s | %9s | %9s | %9s | %9s | %9s | %5d |\n",
			r.Op, fmtDur(min), fmtDur(median), fmtDur(mean), fmtDur(p95), fmtDur(max), len(s.samples))
	}
}

type jsonOpResult struct {
	Category string  `json:"category"`
	Op       string  `json:"operation"`
	N        int     `json:"n"`
	MinMs    float64 `json:"min_ms"`
	MedianMs float64 `json:"median_ms"`
	MeanMs   float64 `json:"mean_ms"`
	P95Ms    float64 `json:"p95_ms"`
	MaxMs    float64 `json:"max_ms"`
}

func printJSON(label string, results []benchResult) {
	out := struct {
		Label   string         `json:"label"`
		Results []jsonOpResult `json:"results"`
	}{Label: label}

	for _, r := range results {
		s := r.Stats
		if len(s.samples) == 0 {
			continue
		}
		min, max, median, p95, mean := s.compute()
		out.Results = append(out.Results, jsonOpResult{
			Category: r.Category,
			Op:       r.Op,
			N:        len(s.samples),
			MinMs:    float64(min.Microseconds()) / 1000,
			MedianMs: float64(median.Microseconds()) / 1000,
			MeanMs:   float64(mean.Microseconds()) / 1000,
			P95Ms:    float64(p95.Microseconds()) / 1000,
			MaxMs:    float64(max.Microseconds()) / 1000,
		})
	}

	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func connectToPage(ctx context.Context, baseURL string, log *slog.Logger) (*cdp, error) {
	pageWS, err := discoverPageWSURL(baseURL)
	if err != nil {
		return nil, fmt.Errorf("discover page target: %w", err)
	}
	log.Info("discovered page target", "ws", pageWS)
	return dialCDP(ctx, pageWS, log)
}

// connectViaSession connects to the browser-level WS and uses Target.attachToTarget
// to get a session. This works through the kernel-images-api proxy which doesn't
// route page-level WS connections properly.
func connectViaSession(ctx context.Context, baseURL string, log *slog.Logger) (*cdp, error) {
	resp, err := http.Get(baseURL + "/json/version")
	if err != nil {
		return nil, fmt.Errorf("GET /json/version: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var ver struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.Unmarshal(body, &ver); err != nil {
		return nil, fmt.Errorf("parse /json/version: %w", err)
	}

	// The returned WS URL may reference 127.0.0.1 but we might be connecting
	// via an external TLS endpoint (e.g. KraftCloud). Rewrite the WS URL to
	// use the same host/scheme as baseURL.
	wsURL := ver.WebSocketDebuggerURL
	base, _ := url.Parse(baseURL)
	parsed, _ := url.Parse(wsURL)
	if base != nil && parsed != nil {
		rewrite := false
		if base.Hostname() != parsed.Hostname() {
			parsed.Host = base.Host
			rewrite = true
		}
		if base.Scheme == "https" && parsed.Scheme == "ws" {
			parsed.Scheme = "wss"
			rewrite = true
		}
		if rewrite {
			wsURL = parsed.String()
			log.Info("rewrote WS URL for remote access", "original", ver.WebSocketDebuggerURL, "rewritten", wsURL)
		}
	}

	c, err := dialCDP(ctx, wsURL, log)
	if err != nil {
		return nil, err
	}
	log.Info("connected to browser WS", "url", wsURL)

	// Get page targets
	result, err := c.call(ctx, "Target.getTargets", nil)
	if err != nil {
		c.close()
		return nil, fmt.Errorf("Target.getTargets: %w", err)
	}
	var targetsResp struct {
		TargetInfos []struct {
			TargetID string `json:"targetId"`
			Type     string `json:"type"`
		} `json:"targetInfos"`
	}
	json.Unmarshal(result, &targetsResp)

	var pageTargetID string
	for _, t := range targetsResp.TargetInfos {
		if t.Type == "page" {
			pageTargetID = t.TargetID
			break
		}
	}
	if pageTargetID == "" {
		c.close()
		return nil, fmt.Errorf("no page target found")
	}
	log.Info("found page target", "id", pageTargetID)

	// Attach to target; flatten=true so commands can include sessionId at top level
	result, err = c.call(ctx, "Target.attachToTarget", map[string]any{
		"targetId": pageTargetID,
		"flatten":  true,
	})
	if err != nil {
		c.close()
		return nil, fmt.Errorf("Target.attachToTarget: %w", err)
	}
	var attachResp struct {
		SessionID string `json:"sessionId"`
	}
	json.Unmarshal(result, &attachResp)
	if attachResp.SessionID == "" {
		c.close()
		return nil, fmt.Errorf("no session ID in attach response")
	}

	c.sessionID = attachResp.SessionID
	log.Info("attached to page session", "sessionId", c.sessionID)
	return c, nil
}

func main() {
	cdpBase := flag.String("cdp-url", "http://127.0.0.1:9222", "HTTP base URL for CDP (used to discover page targets)")
	sessionMode := flag.Bool("session-mode", false, "Use Target.attachToTarget session mode (for proxied CDP)")
	iterations := flag.Int("iterations", 3, "Number of full site rotation iterations")
	label := flag.String("label", "benchmark", "Label for this run")
	outputJSON := flag.Bool("json", false, "Output as JSON")
	warmup := flag.Int("warmup", 1, "Warmup iterations (discarded)")
	concurrentWorkers := flag.Int("concurrent-workers", 3, "Number of parallel CDP connections for concurrent test")
	concurrentDuration := flag.Duration("concurrent-duration", 30*time.Second, "Duration of concurrent load test")
	skipConcurrent := flag.Bool("skip-concurrent", false, "Skip the concurrent load test")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	connect := func(ctx context.Context) (*cdp, error) {
		if *sessionMode {
			return connectViaSession(ctx, *cdpBase, log)
		}
		return connectToPage(ctx, *cdpBase, log)
	}

	log.Info("connecting to CDP", "base", *cdpBase, "sessionMode", *sessionMode)
	c, err := connect(ctx)
	if err != nil {
		log.Error("failed to connect", "error", err)
		os.Exit(1)
	}
	defer c.close()
	log.Info("connected", "sites", len(testSites), "iterations", *iterations)

	// Warmup
	if *warmup > 0 {
		log.Info("warmup", "iterations", *warmup)
		runBenchmark(ctx, c, *warmup, log)
		c.close()
		time.Sleep(time.Second)
		c, err = connect(ctx)
		if err != nil {
			log.Error("reconnect failed", "error", err)
			os.Exit(1)
		}
		defer c.close()
	}

	// Sequential benchmark
	log.Info("starting sequential benchmark", "iterations", *iterations, "label", *label)
	results := runBenchmark(ctx, c, *iterations, log)

	// Concurrent benchmark
	if !*skipConcurrent {
		c.call(ctx, "Page.enable", nil)
		navigateAndWait(ctx, c, "https://en.wikipedia.org/wiki/Web_browser")
		time.Sleep(time.Second)

		log.Info("starting concurrent benchmark", "workers", *concurrentWorkers, "duration", *concurrentDuration)
		concResults := runConcurrentBench(ctx, *cdpBase, *sessionMode, *concurrentWorkers, *concurrentDuration, log)
		results = append(results, concResults...)
	}

	if *outputJSON {
		printJSON(*label, results)
	} else {
		printTable(*label, results)
	}
}
