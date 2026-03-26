package cdpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"

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
	conn  *websocket.Conn
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
		"width":            width,
		"height":           height,
		"deviceScaleFactor": 1,
		"mobile":           false,
	}, attach.SessionID)
	if err != nil {
		return fmt.Errorf("Emulation.setDeviceMetricsOverride: %w", err)
	}

	_, _ = c.send(ctx, "Target.detachFromTarget", map[string]any{
		"sessionId": attach.SessionID,
	}, "")

	return nil
}
