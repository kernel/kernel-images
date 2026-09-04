package cdpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

type cdpRequest struct {
	ID        int64           `json:"id"`
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
}

// ErrOutcomeUnknown means a CDP connection closed while a command was in
// flight, so the caller cannot know whether Chromium applied it.
var ErrOutcomeUnknown = errors.New("CDP command outcome is unknown")

// Message is a response or event received from Chromium.
type Message struct {
	ID        int64           `json:"id,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *Error          `json:"error,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
}

// Error is a protocol error returned by Chromium.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("CDP error %d: %s", e.Code, e.Message)
}

type commandResult struct {
	result json.RawMessage
	err    error
}

// Client maintains one browser-level DevTools connection. Commands may be
// sent concurrently. Clients created by DialWithEvents also expose protocol
// events through Events.
type Client struct {
	conn   *websocket.Conn
	ctx    context.Context
	cancel context.CancelFunc

	nextID  atomic.Int64
	writeMu sync.Mutex

	pendingMu sync.Mutex
	pending   map[int64]chan commandResult
	events    chan Message

	closed    chan struct{}
	closeOnce sync.Once
}

// BrowserWebSocketURL reads Chrome's browser-level DevTools WebSocket URL.
func BrowserWebSocketURL(ctx context.Context, versionURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, versionURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("get DevTools version: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("get DevTools version: %s", resp.Status)
	}
	var version struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&version); err != nil {
		return "", fmt.Errorf("decode DevTools version: %w", err)
	}
	if version.WebSocketDebuggerURL == "" {
		return "", fmt.Errorf("DevTools version response has no browser WebSocket URL")
	}
	return version.WebSocketDebuggerURL, nil
}

// Dial opens a command-only DevTools connection. Protocol events are
// discarded, preserving the behavior expected by existing short-lived users.
func Dial(ctx context.Context, devtoolsURL string) (*Client, error) {
	return dial(ctx, devtoolsURL, false)
}

// DialWithEvents opens a DevTools connection that delivers protocol events in
// receive order through Events.
func DialWithEvents(ctx context.Context, devtoolsURL string) (*Client, error) {
	return dial(ctx, devtoolsURL, true)
}

func dial(ctx context.Context, devtoolsURL string, withEvents bool) (*Client, error) {
	conn, _, err := websocket.Dial(ctx, devtoolsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("dial devtools: %w", err)
	}
	readLimit := int64(4 * 1024 * 1024)
	var events chan Message
	if withEvents {
		readLimit = 8 * 1024 * 1024
		events = make(chan Message, 256)
	}
	conn.SetReadLimit(readLimit)
	clientCtx, cancel := context.WithCancel(context.Background())
	client := &Client{
		conn:    conn,
		ctx:     clientCtx,
		cancel:  cancel,
		pending: make(map[int64]chan commandResult),
		events:  events,
		closed:  make(chan struct{}),
	}
	go client.readLoop()
	return client, nil
}

// Close shuts down the WebSocket connection and unblocks pending commands.
func (c *Client) Close() error {
	c.shutdown()
	c.conn.CloseNow()
	return nil
}

func (c *Client) shutdown() {
	c.closeOnce.Do(func() {
		c.cancel()
		close(c.closed)
		c.failPending(ErrOutcomeUnknown)
	})
}

// Send sends a CDP command and waits for its matching response.
func (c *Client) Send(ctx context.Context, method string, params any, sessionID string) (json.RawMessage, error) {
	id := c.nextID.Add(1)

	var rawParams json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("marshal params: %w", err)
		}
		rawParams = b
	}

	reqBytes, err := json.Marshal(cdpRequest{ID: id, Method: method, Params: rawParams, SessionID: sessionID})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	responseCh := make(chan commandResult, 1)
	c.pendingMu.Lock()
	if c.IsClosed() {
		c.pendingMu.Unlock()
		return nil, fmt.Errorf("read: %w", ErrOutcomeUnknown)
	}
	c.pending[id] = responseCh
	c.pendingMu.Unlock()

	c.writeMu.Lock()
	err = c.conn.Write(ctx, websocket.MessageText, reqBytes)
	c.writeMu.Unlock()
	if err != nil {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return nil, fmt.Errorf("write: %w", err)
	}

	select {
	case response := <-responseCh:
		return response.result, response.err
	case <-ctx.Done():
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		select {
		case response := <-responseCh:
			return response.result, response.err
		default:
			return nil, fmt.Errorf("read: %w", ctx.Err())
		}
	case <-c.closed:
		select {
		case response := <-responseCh:
			return response.result, response.err
		default:
			return nil, fmt.Errorf("read: %w", ErrOutcomeUnknown)
		}
	}
}

// Events returns the event stream for clients created by DialWithEvents. It
// returns nil for command-only clients.
func (c *Client) Events() <-chan Message {
	return c.events
}

// Done closes when the connection shuts down.
func (c *Client) Done() <-chan struct{} {
	return c.closed
}

// IsClosed reports whether the connection has shut down.
func (c *Client) IsClosed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

func (c *Client) readLoop() {
	if c.events != nil {
		defer close(c.events)
	}
	for {
		_, payload, err := c.conn.Read(c.ctx)
		if err != nil {
			c.shutdown()
			return
		}
		var message Message
		if json.Unmarshal(payload, &message) != nil {
			continue
		}
		if message.ID == 0 {
			if c.events == nil {
				continue
			}
			select {
			case c.events <- message:
			case <-c.closed:
				return
			}
			continue
		}

		c.pendingMu.Lock()
		responseCh := c.pending[message.ID]
		if responseCh != nil {
			if message.Error != nil {
				responseCh <- commandResult{err: message.Error}
			} else {
				responseCh <- commandResult{result: message.Result}
			}
			delete(c.pending, message.ID)
		}
		c.pendingMu.Unlock()
	}
}

func (c *Client) failPending(err error) {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	for id, responseCh := range c.pending {
		responseCh <- commandResult{err: err}
		delete(c.pending, id)
	}
}

// BrowserVersion is the result of a Browser.getVersion CDP call.
//
// We use this struct only to confirm a successful round-trip; callers that
// just need a liveness probe can ignore the fields. The protocol-version
// fields are populated for convenience.
type BrowserVersion struct {
	ProtocolVersion string `json:"protocolVersion"`
	Product         string `json:"product"`
	Revision        string `json:"revision"`
	UserAgent       string `json:"userAgent"`
	JsVersion       string `json:"jsVersion"`
}

// GetBrowserVersion sends Browser.getVersion on the browser-level DevTools
// endpoint. It is a cheap CDP round-trip that proves the WebSocket is
// connected to a live, CDP-responsive Chromium browser process.
//
// Callers should use this after Dial as a readiness gate: a successful
// websocket.Dial alone is not enough because a dial can complete against
// a half-open socket of a killed Chromium, or against a freshly bound TCP
// listener of a Chromium that has not yet wired up its CDP routes. A
// Browser.getVersion round-trip rules out both cases.
func (c *Client) GetBrowserVersion(ctx context.Context) (*BrowserVersion, error) {
	raw, err := c.Send(ctx, "Browser.getVersion", nil, "")
	if err != nil {
		return nil, fmt.Errorf("Browser.getVersion: %w", err)
	}
	var v BrowserVersion
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("unmarshal Browser.getVersion: %w", err)
	}
	return &v, nil
}

// LoadUnpackedExtension installs an unpacked extension from an absolute path
// visible to Chromium and returns its extension ID.
func (c *Client) LoadUnpackedExtension(ctx context.Context, path string) (string, error) {
	raw, err := c.Send(ctx, "Extensions.loadUnpacked", map[string]string{"path": path}, "")
	if err != nil {
		return "", fmt.Errorf("Extensions.loadUnpacked: %w", err)
	}
	var result struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", fmt.Errorf("unmarshal Extensions.loadUnpacked: %w", err)
	}
	if result.ID == "" {
		return "", fmt.Errorf("Extensions.loadUnpacked returned no extension ID")
	}
	return result.ID, nil
}

// ExtensionInfo describes an unpacked extension known to Chromium.
type ExtensionInfo struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Version string `json:"version"`
	Path    string `json:"path"`
	Enabled bool   `json:"enabled"`
}

// GetExtensions returns all unpacked extensions known to Chromium.
func (c *Client) GetExtensions(ctx context.Context) ([]ExtensionInfo, error) {
	raw, err := c.Send(ctx, "Extensions.getExtensions", nil, "")
	if err != nil {
		return nil, fmt.Errorf("Extensions.getExtensions: %w", err)
	}
	var result struct {
		Extensions []ExtensionInfo `json:"extensions"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("unmarshal Extensions.getExtensions: %w", err)
	}
	return result.Extensions, nil
}

// Histogram is a snapshot of a Chrome UMA histogram as returned by
// Browser.getHistograms. Values are cumulative since browser start and the
// units follow the UMA definition of the histogram (PageLoad timings are
// milliseconds). Only buckets with at least one sample are present.
type Histogram struct {
	Name    string            `json:"name"`
	Sum     int64             `json:"sum"`
	Count   int64             `json:"count"`
	Buckets []HistogramBucket `json:"buckets"`
}

// HistogramBucket is a [Low, High) bucket of a Histogram.
type HistogramBucket struct {
	Low   int64 `json:"low"`
	High  int64 `json:"high"`
	Count int64 `json:"count"`
}

// GetHistograms sends Browser.getHistograms, a browser-level command that
// reads Chrome's in-memory UMA histograms without attaching to any page.
// query is a substring filter on the histogram name; empty returns all.
func (c *Client) GetHistograms(ctx context.Context, query string) ([]Histogram, error) {
	raw, err := c.Send(ctx, "Browser.getHistograms", map[string]any{"query": query}, "")
	if err != nil {
		return nil, fmt.Errorf("Browser.getHistograms: %w", err)
	}
	var result struct {
		Histograms []Histogram `json:"histograms"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("unmarshal Browser.getHistograms: %w", err)
	}
	return result.Histograms, nil
}

// CountPageTargets returns the number of open page targets.
func (c *Client) CountPageTargets(ctx context.Context) (int, error) {
	targetsResult, err := c.Send(ctx, "Target.getTargets", nil, "")
	if err != nil {
		return 0, fmt.Errorf("Target.getTargets: %w", err)
	}
	var targets struct {
		TargetInfos []struct {
			Type string `json:"type"`
		} `json:"targetInfos"`
	}
	if err := json.Unmarshal(targetsResult, &targets); err != nil {
		return 0, fmt.Errorf("unmarshal targets: %w", err)
	}
	n := 0
	for _, t := range targets.TargetInfos {
		if t.Type == "page" {
			n++
		}
	}
	return n, nil
}

// DispatchStartURL closes extra page targets and dispatches a navigation on the
// first page target. It does not wait for lifecycle events; Chrome owns the
// eventual navigation result.
func DispatchStartURL(ctx context.Context, devtoolsURL, url string) error {
	c, err := Dial(ctx, devtoolsURL)
	if err != nil {
		return fmt.Errorf("dial devtools: %w", err)
	}
	defer c.Close()

	targetsResult, err := c.Send(ctx, "Target.getTargets", nil, "")
	if err != nil {
		return fmt.Errorf("Target.getTargets: %w", err)
	}

	var targets struct {
		TargetInfos []struct {
			TargetID string `json:"targetId"`
			Type     string `json:"type"`
		} `json:"targetInfos"`
	}
	if err := json.Unmarshal(targetsResult, &targets); err != nil {
		return fmt.Errorf("unmarshal targets: %w", err)
	}

	var pageTargetID string
	for _, t := range targets.TargetInfos {
		if t.Type != "page" {
			continue
		}
		if pageTargetID == "" {
			pageTargetID = t.TargetID
			continue
		}
		_, _ = c.Send(ctx, "Target.closeTarget", map[string]any{
			"targetId": t.TargetID,
		}, "")
	}
	if pageTargetID == "" {
		createResult, err := c.Send(ctx, "Target.createTarget", map[string]any{
			"url": "about:blank",
		}, "")
		if err != nil {
			return fmt.Errorf("Target.createTarget: %w", err)
		}
		var created struct {
			TargetID string `json:"targetId"`
		}
		if err := json.Unmarshal(createResult, &created); err != nil {
			return fmt.Errorf("unmarshal create target: %w", err)
		}
		pageTargetID = created.TargetID
	}

	attachResult, err := c.Send(ctx, "Target.attachToTarget", map[string]any{
		"targetId": pageTargetID,
		"flatten":  true,
	}, "")
	if err != nil {
		return fmt.Errorf("Target.attachToTarget: %w", err)
	}

	var attach struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(attachResult, &attach); err != nil {
		return fmt.Errorf("unmarshal attach: %w", err)
	}
	defer func() {
		detachCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = c.Send(detachCtx, "Target.detachFromTarget", map[string]any{
			"sessionId": attach.SessionID,
		}, "")
	}()

	if _, err := c.Send(ctx, "Page.navigate", map[string]any{"url": url}, attach.SessionID); err != nil {
		return fmt.Errorf("Page.navigate: %w", err)
	}
	return nil
}

// DispatchStartURLAndWait navigates through navigationURL and waits for
// destination to load without resolving to Chrome's network error page.
func DispatchStartURLAndWait(ctx context.Context, devtoolsURL, navigationURL, destination string) error {
	if err := DispatchStartURL(ctx, devtoolsURL, navigationURL); err != nil {
		return err
	}

	want, err := url.Parse(destination)
	if err != nil {
		return fmt.Errorf("parse destination: %w", err)
	}
	c, err := Dial(ctx, devtoolsURL)
	if err != nil {
		return err
	}
	defer c.Close()

	targetID, err := c.firstPageTargetID(ctx)
	if err != nil {
		return err
	}
	attachResult, err := c.Send(ctx, "Target.attachToTarget", map[string]any{
		"targetId": targetID,
		"flatten":  true,
	}, "")
	if err != nil {
		return fmt.Errorf("Target.attachToTarget: %w", err)
	}
	var attach struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(attachResult, &attach); err != nil {
		return fmt.Errorf("unmarshal attach: %w", err)
	}
	defer func() {
		detachCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = c.Send(detachCtx, "Target.detachFromTarget", map[string]any{
			"sessionId": attach.SessionID,
		}, "")
	}()

	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	var lastState struct {
		URL        string `json:"url"`
		ReadyState string `json:"readyState"`
	}
	lastNavigate := time.Now()
	for {
		raw, evalErr := c.Send(ctx, "Runtime.evaluate", map[string]any{
			"expression":    `JSON.stringify({url: location.href, readyState: document.readyState})`,
			"returnByValue": true,
		}, attach.SessionID)
		if evalErr == nil {
			var evaluated struct {
				Result struct {
					Value string `json:"value"`
				} `json:"result"`
			}
			if err := json.Unmarshal(raw, &evaluated); err == nil {
				_ = json.Unmarshal([]byte(evaluated.Result.Value), &lastState)
				current, parseErr := url.Parse(lastState.URL)
				if parseErr == nil && relatedHosts(current.Hostname(), want.Hostname()) && lastState.ReadyState == "complete" {
					return nil
				}
				if strings.HasPrefix(lastState.URL, "chrome-error://") && time.Since(lastNavigate) >= 250*time.Millisecond {
					_, _ = c.Send(ctx, "Page.navigate", map[string]any{"url": navigationURL}, attach.SessionID)
					lastNavigate = time.Now()
				}
			}
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for %s to load (last URL %q, state %q): %w", destination, lastState.URL, lastState.ReadyState, ctx.Err())
		case <-ticker.C:
		}
	}
}

func relatedHosts(a, b string) bool {
	a = strings.ToLower(strings.TrimSuffix(a, "."))
	b = strings.ToLower(strings.TrimSuffix(b, "."))
	return a == b || strings.HasSuffix(a, "."+b) || strings.HasSuffix(b, "."+a)
}

// firstPageTargetID returns the targetId of the first page target reported
// by Target.getTargets. Callers that need to operate on the user-facing
// browser window (Emulation, Browser.* window bounds) use this to find it.
func (c *Client) firstPageTargetID(ctx context.Context) (string, error) {
	targetsResult, err := c.Send(ctx, "Target.getTargets", nil, "")
	if err != nil {
		return "", fmt.Errorf("Target.getTargets: %w", err)
	}
	var targets struct {
		TargetInfos []struct {
			TargetID string `json:"targetId"`
			Type     string `json:"type"`
		} `json:"targetInfos"`
	}
	if err := json.Unmarshal(targetsResult, &targets); err != nil {
		return "", fmt.Errorf("unmarshal targets: %w", err)
	}
	for _, t := range targets.TargetInfos {
		if t.Type == "page" {
			return t.TargetID, nil
		}
	}
	return "", fmt.Errorf("no page target found")
}

// SetWindowBoundsMaximized puts the OS window backing the first page target
// into the maximized state via Browser.setWindowBounds. It is idempotent —
// invoking it on a window already in maximized state is a no-op.
//
// A mutter-managed window in maximized state auto-tracks RANDR resizes
// (the WM reflows it to fill the new root). So after a display resize the
// server only has to make sure the window is in maximized state; mutter
// does the rest. This replaces the prior approach of restarting chromium
// so it could re-apply --start-maximized.
//
// We intentionally avoid the explicit-bounds form of setWindowBounds
// ({left, top, width, height} with windowState:"normal"): once a window is
// in normal state it stops auto-tracking subsequent RANDR events.
func (c *Client) SetWindowBoundsMaximized(ctx context.Context) error {
	bounds, err := c.GetWindowBounds(ctx)
	if err != nil {
		return err
	}
	// Both "maximized" and "fullscreen" cause mutter to reflow the window
	// to fill the new X root on RANDR — that's the only invariant we
	// need. Demoting a kiosk fullscreen window to maximized would break
	// kiosk mode, so leave fullscreen alone.
	if bounds.WindowState == "maximized" || bounds.WindowState == "fullscreen" {
		return nil
	}

	if _, err := c.Send(ctx, "Browser.setWindowBounds", map[string]any{
		"windowId": bounds.WindowID,
		"bounds":   map[string]any{"windowState": "maximized"},
	}, ""); err != nil {
		return fmt.Errorf("Browser.setWindowBounds maximized: %w", err)
	}
	return nil
}

// WindowBounds is the subset of Browser.getWindowBounds CDP returns that
// callers care about. For maximized/fullscreen windows the width/height
// fields reflect the live window size (which the WM aligns with the X
// root); for normal-state windows they reflect the saved-restore bounds.
type WindowBounds struct {
	WindowID    int
	Width       int
	Height      int
	WindowState string
}

// GetWindowBounds queries the OS window bounds for the first page target
// via Browser.getWindowForTarget. It's a one-shot read; callers that need
// to wait for the WM to settle should poll this.
func (c *Client) GetWindowBounds(ctx context.Context) (WindowBounds, error) {
	pageTargetID, err := c.firstPageTargetID(ctx)
	if err != nil {
		return WindowBounds{}, err
	}

	winRaw, err := c.Send(ctx, "Browser.getWindowForTarget", map[string]any{"targetId": pageTargetID}, "")
	if err != nil {
		return WindowBounds{}, fmt.Errorf("Browser.getWindowForTarget: %w", err)
	}
	var winResp struct {
		WindowID int `json:"windowId"`
		Bounds   struct {
			Width       int    `json:"width"`
			Height      int    `json:"height"`
			WindowState string `json:"windowState"`
		} `json:"bounds"`
	}
	if err := json.Unmarshal(winRaw, &winResp); err != nil {
		return WindowBounds{}, fmt.Errorf("unmarshal window: %w", err)
	}
	return WindowBounds{
		WindowID:    winResp.WindowID,
		Width:       winResp.Bounds.Width,
		Height:      winResp.Bounds.Height,
		WindowState: winResp.Bounds.WindowState,
	}, nil
}

// SetDeviceMetricsOverride sets the viewport dimensions on the first page
// target found in the browser. It attaches to the target with a flattened
// session, sends Emulation.setDeviceMetricsOverride, then detaches.
func (c *Client) SetDeviceMetricsOverride(ctx context.Context, width, height int) error {
	pageTargetID, err := c.firstPageTargetID(ctx)
	if err != nil {
		return err
	}

	attachResult, err := c.Send(ctx, "Target.attachToTarget", map[string]any{
		"targetId": pageTargetID,
		"flatten":  true,
	}, "")
	if err != nil {
		return fmt.Errorf("Target.attachToTarget: %w", err)
	}

	var attach struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(attachResult, &attach); err != nil {
		return fmt.Errorf("unmarshal attach: %w", err)
	}

	_, err = c.Send(ctx, "Emulation.setDeviceMetricsOverride", map[string]any{
		"width":             width,
		"height":            height,
		"deviceScaleFactor": 1,
		"mobile":            false,
	}, attach.SessionID)
	if err != nil {
		return fmt.Errorf("Emulation.setDeviceMetricsOverride: %w", err)
	}

	_, _ = c.Send(ctx, "Target.detachFromTarget", map[string]any{
		"sessionId": attach.SessionID,
	}, "")

	return nil
}
