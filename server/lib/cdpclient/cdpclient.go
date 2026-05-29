package cdpclient

import (
	"context"
	"encoding/json"
	"fmt"
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

type cdpResponse struct {
	ID     int64           `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *cdpError       `json:"error,omitempty"`
}

type cdpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *cdpError) Error() string {
	return fmt.Sprintf("CDP error %d: %s", e.Code, e.Message)
}

// Client is a minimal CDP client that communicates over a browser-level
// DevTools WebSocket connection.
type Client struct {
	conn   *websocket.Conn
	nextID atomic.Int64
}

// Dial opens a WebSocket connection to the given DevTools URL.
func Dial(ctx context.Context, devtoolsURL string) (*Client, error) {
	conn, _, err := websocket.Dial(ctx, devtoolsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("dial devtools: %w", err)
	}
	conn.SetReadLimit(4 * 1024 * 1024)
	return &Client{conn: conn}, nil
}

// Close shuts down the WebSocket connection.
func (c *Client) Close() error {
	return c.conn.Close(websocket.StatusNormalClosure, "done")
}

// send sends a CDP command and waits for the matching response, discarding
// any intermediate events. This is safe for short-lived connections where the
// caller controls the full message sequence.
func (c *Client) send(ctx context.Context, method string, params any, sessionID string) (json.RawMessage, error) {
	id := c.nextID.Add(1)

	var rawParams json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("marshal params: %w", err)
		}
		rawParams = b
	}

	req := cdpRequest{ID: id, Method: method, Params: rawParams, SessionID: sessionID}
	reqBytes, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	if err := c.conn.Write(ctx, websocket.MessageText, reqBytes); err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}

	for {
		_, msg, err := c.conn.Read(ctx)
		if err != nil {
			return nil, fmt.Errorf("read: %w", err)
		}

		var resp cdpResponse
		if err := json.Unmarshal(msg, &resp); err != nil {
			continue // skip malformed messages
		}
		if resp.ID != id {
			continue // skip events and responses to other commands
		}
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
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
	raw, err := c.send(ctx, "Browser.getVersion", nil, "")
	if err != nil {
		return nil, fmt.Errorf("Browser.getVersion: %w", err)
	}
	var v BrowserVersion
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("unmarshal Browser.getVersion: %w", err)
	}
	return &v, nil
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

	targetsResult, err := c.send(ctx, "Target.getTargets", nil, "")
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
		_, _ = c.send(ctx, "Target.closeTarget", map[string]any{
			"targetId": t.TargetID,
		}, "")
	}
	if pageTargetID == "" {
		createResult, err := c.send(ctx, "Target.createTarget", map[string]any{
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

	attachResult, err := c.send(ctx, "Target.attachToTarget", map[string]any{
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
		_, _ = c.send(detachCtx, "Target.detachFromTarget", map[string]any{
			"sessionId": attach.SessionID,
		}, "")
	}()

	if _, err := c.send(ctx, "Page.navigate", map[string]any{"url": url}, attach.SessionID); err != nil {
		return fmt.Errorf("Page.navigate: %w", err)
	}
	return nil
}

// SetDeviceMetricsOverride sets the viewport dimensions on the first page
// target found in the browser. It attaches to the target with a flattened
// session, sends Emulation.setDeviceMetricsOverride, then detaches.
func (c *Client) SetDeviceMetricsOverride(ctx context.Context, width, height int) error {
	targetsResult, err := c.send(ctx, "Target.getTargets", nil, "")
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
		if t.Type == "page" {
			pageTargetID = t.TargetID
			break
		}
	}
	if pageTargetID == "" {
		return fmt.Errorf("no page target found")
	}

	attachResult, err := c.send(ctx, "Target.attachToTarget", map[string]any{
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

	_, err = c.send(ctx, "Emulation.setDeviceMetricsOverride", map[string]any{
		"width":             width,
		"height":            height,
		"deviceScaleFactor": 1,
		"mobile":            false,
	}, attach.SessionID)
	if err != nil {
		return fmt.Errorf("Emulation.setDeviceMetricsOverride: %w", err)
	}

	_, _ = c.send(ctx, "Target.detachFromTarget", map[string]any{
		"sessionId": attach.SessionID,
	}, "")

	return nil
}
