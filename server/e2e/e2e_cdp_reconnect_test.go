package e2e

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	instanceoapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/require"
)

type cdpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type cdpEnvelope struct {
	ID        int             `json:"id,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *cdpError       `json:"error,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
}

type cdpCall struct {
	response chan cdpEnvelope
}

type cdpEventWaiter struct {
	method    string
	sessionID string
	response  chan cdpEnvelope
}

type cdpClient struct {
	conn     *websocket.Conn
	closeMu  sync.Mutex
	closed   bool
	closeCh  chan struct{}
	closeErr error

	mu      sync.Mutex
	nextID  int
	pending map[int]cdpCall
	waiters []cdpEventWaiter
}

type cdpExerciseResult struct {
	Browser         string
	Title           string
	Heading         string
	Sum             int
	ReadyState      string
	ScreenshotBytes int
}

func newCDPClient(ctx context.Context, wsURL string) (*cdpClient, error) {
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(100 * 1024 * 1024)

	client := &cdpClient{
		conn:    conn,
		closeCh: make(chan struct{}),
		nextID:  1,
		pending: make(map[int]cdpCall),
	}
	go client.readLoop()
	return client, nil
}

func (c *cdpClient) readLoop() {
	for {
		_, msg, err := c.conn.Read(context.Background())
		if err != nil {
			c.closeWithErr(err)
			return
		}

		var envelope cdpEnvelope
		if err := json.Unmarshal(msg, &envelope); err != nil {
			c.closeWithErr(fmt.Errorf("unmarshal CDP message: %w", err))
			return
		}

		if envelope.ID != 0 {
			c.mu.Lock()
			call, ok := c.pending[envelope.ID]
			if ok {
				delete(c.pending, envelope.ID)
			}
			c.mu.Unlock()
			if ok {
				call.response <- envelope
			}
			continue
		}

		c.mu.Lock()
		for i, waiter := range c.waiters {
			if waiter.method == envelope.Method && waiter.sessionID == envelope.SessionID {
				c.waiters = append(c.waiters[:i], c.waiters[i+1:]...)
				c.mu.Unlock()
				waiter.response <- envelope
				goto handled
			}
		}
		c.mu.Unlock()

	handled:
	}
}

func (c *cdpClient) closeWithErr(err error) {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	c.closeErr = err

	c.mu.Lock()
	for _, call := range c.pending {
		close(call.response)
	}
	c.pending = map[int]cdpCall{}
	for _, waiter := range c.waiters {
		close(waiter.response)
	}
	c.waiters = nil
	c.mu.Unlock()

	close(c.closeCh)
}

func (c *cdpClient) Close() {
	_ = c.conn.Close(websocket.StatusNormalClosure, "")
}

func (c *cdpClient) WaitClosed(ctx context.Context) error {
	select {
	case <-c.closeCh:
		return c.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *cdpClient) Call(ctx context.Context, method string, params any, sessionID string) (json.RawMessage, error) {
	c.closeMu.Lock()
	closed := c.closed
	c.closeMu.Unlock()
	if closed {
		return nil, fmt.Errorf("connection closed: %w", c.closeErr)
	}

	id := 0
	responseCh := make(chan cdpEnvelope, 1)

	c.mu.Lock()
	id = c.nextID
	c.nextID++
	c.pending[id] = cdpCall{response: responseCh}
	c.mu.Unlock()

	payload := map[string]any{
		"id":     id,
		"method": method,
		"params": params,
	}
	if sessionID != "" {
		payload["sessionId"] = sessionID
	}

	writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := wsjsonWrite(writeCtx, c.conn, payload); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, err
	}

	select {
	case envelope, ok := <-responseCh:
		if !ok {
			return nil, fmt.Errorf("connection closed while waiting for %s", method)
		}
		if envelope.Error != nil {
			return nil, fmt.Errorf("CDP %s failed: %d %s", method, envelope.Error.Code, envelope.Error.Message)
		}
		return envelope.Result, nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (c *cdpClient) WaitForEvent(ctx context.Context, method, sessionID string) error {
	responseCh := make(chan cdpEnvelope, 1)

	c.mu.Lock()
	c.waiters = append(c.waiters, cdpEventWaiter{
		method:    method,
		sessionID: sessionID,
		response:  responseCh,
	})
	c.mu.Unlock()

	select {
	case _, ok := <-responseCh:
		if !ok {
			return fmt.Errorf("connection closed before event %s", method)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func wsjsonWrite(ctx context.Context, conn *websocket.Conn, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, payload)
}

func decodeJSONStringField(raw json.RawMessage, field string) (string, error) {
	var values map[string]any
	if err := json.Unmarshal(raw, &values); err != nil {
		return "", err
	}

	value, ok := values[field].(string)
	if !ok {
		return "", fmt.Errorf("expected field %q in %s", field, string(raw))
	}
	return value, nil
}

func connectAndExerciseCDP(ctx context.Context, wsURL, label string) (*cdpClient, cdpExerciseResult, error) {
	client, err := newCDPClient(ctx, wsURL)
	if err != nil {
		return nil, cdpExerciseResult{}, err
	}

	versionRaw, err := client.Call(ctx, "Browser.getVersion", map[string]any{}, "")
	if err != nil {
		client.Close()
		return nil, cdpExerciseResult{}, err
	}
	browser, err := decodeJSONStringField(versionRaw, "product")
	if err != nil {
		client.Close()
		return nil, cdpExerciseResult{}, err
	}

	targetRaw, err := client.Call(ctx, "Target.createTarget", map[string]any{"url": "about:blank"}, "")
	if err != nil {
		client.Close()
		return nil, cdpExerciseResult{}, err
	}
	targetID, err := decodeJSONStringField(targetRaw, "targetId")
	if err != nil {
		client.Close()
		return nil, cdpExerciseResult{}, err
	}

	attachRaw, err := client.Call(ctx, "Target.attachToTarget", map[string]any{
		"targetId": targetID,
		"flatten":  true,
	}, "")
	if err != nil {
		client.Close()
		return nil, cdpExerciseResult{}, err
	}
	sessionID, err := decodeJSONStringField(attachRaw, "sessionId")
	if err != nil {
		client.Close()
		return nil, cdpExerciseResult{}, err
	}

	if _, err := client.Call(ctx, "Page.enable", map[string]any{}, sessionID); err != nil {
		client.Close()
		return nil, cdpExerciseResult{}, err
	}
	if _, err := client.Call(ctx, "Runtime.enable", map[string]any{}, sessionID); err != nil {
		client.Close()
		return nil, cdpExerciseResult{}, err
	}

	loadCtx, loadCancel := context.WithTimeout(ctx, 15*time.Second)
	defer loadCancel()
	loadDone := make(chan error, 1)
	go func() {
		loadDone <- client.WaitForEvent(loadCtx, "Page.loadEventFired", sessionID)
	}()

	html := fmt.Sprintf("<!doctype html><title>%s</title><h1>%s</h1><script>window.sum=[1,2,3,4,5,6].reduce((a,b)=>a+b,0)</script>", label, label)
	if _, err := client.Call(ctx, "Page.navigate", map[string]any{
		"url": "data:text/html," + url.PathEscape(html),
	}, sessionID); err != nil {
		client.Close()
		return nil, cdpExerciseResult{}, err
	}

	if err := <-loadDone; err != nil {
		client.Close()
		return nil, cdpExerciseResult{}, err
	}

	evalRaw, err := client.Call(ctx, "Runtime.evaluate", map[string]any{
		"expression":    `JSON.stringify({title:document.title,heading:document.querySelector("h1").textContent,sum:window.sum,ready:document.readyState})`,
		"returnByValue": true,
	}, sessionID)
	if err != nil {
		client.Close()
		return nil, cdpExerciseResult{}, err
	}

	var evalEnvelope struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
	}
	if err := json.Unmarshal(evalRaw, &evalEnvelope); err != nil {
		client.Close()
		return nil, cdpExerciseResult{}, err
	}

	var summary struct {
		Title   string `json:"title"`
		Heading string `json:"heading"`
		Sum     int    `json:"sum"`
		Ready   string `json:"ready"`
	}
	if err := json.Unmarshal([]byte(evalEnvelope.Result.Value), &summary); err != nil {
		client.Close()
		return nil, cdpExerciseResult{}, err
	}

	screenshotRaw, err := client.Call(ctx, "Page.captureScreenshot", map[string]any{"format": "png"}, sessionID)
	if err != nil {
		client.Close()
		return nil, cdpExerciseResult{}, err
	}
	var screenshotEnvelope struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(screenshotRaw, &screenshotEnvelope); err != nil {
		client.Close()
		return nil, cdpExerciseResult{}, err
	}
	screenshotBytes, err := base64.StdEncoding.DecodeString(screenshotEnvelope.Data)
	if err != nil {
		client.Close()
		return nil, cdpExerciseResult{}, err
	}

	if _, err := client.Call(ctx, "Target.closeTarget", map[string]any{"targetId": targetID}, ""); err != nil {
		client.Close()
		return nil, cdpExerciseResult{}, err
	}

	return client, cdpExerciseResult{
		Browser:         browser,
		Title:           summary.Title,
		Heading:         summary.Heading,
		Sum:             summary.Sum,
		ReadyState:      summary.Ready,
		ScreenshotBytes: len(screenshotBytes),
	}, nil
}

func restartChromiumViaAPI(ctx context.Context, client *instanceoapi.ClientWithResponses) error {
	args := []string{"-c", "/etc/supervisor/supervisord.conf", "restart", "chromium"}
	req := instanceoapi.ProcessExecJSONRequestBody{
		Command: "supervisorctl",
		Args:    &args,
	}

	rsp, err := client.ProcessExecWithResponse(ctx, req)
	if err != nil {
		return err
	}
	if rsp.JSON200 == nil {
		return fmt.Errorf("restart chromium returned status %s", rsp.Status())
	}
	return nil
}

func waitForContainerFile(ctx context.Context, client *instanceoapi.ClientWithResponses, path string, timeout time.Duration) error {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		args := []string{"-lc", fmt.Sprintf("test -f %q", path)}
		req := instanceoapi.ProcessExecJSONRequestBody{
			Command: "sh",
			Args:    &args,
		}
		reqCtx, reqCancel := context.WithTimeout(waitCtx, 2*time.Second)
		rsp, err := client.ProcessExecWithResponse(reqCtx, req)
		reqCancel()
		if err == nil && rsp.JSON200 != nil && rsp.JSON200.ExitCode != nil && *rsp.JSON200.ExitCode == 0 {
			return nil
		}

		select {
		case <-waitCtx.Done():
			return waitCtx.Err()
		case <-ticker.C:
		}
	}
}

func touchContainerFile(ctx context.Context, client *instanceoapi.ClientWithResponses, path string) error {
	args := []string{"-lc", fmt.Sprintf("mkdir -p %q && touch %q", filepath.Dir(path), path)}
	req := instanceoapi.ProcessExecJSONRequestBody{
		Command: "sh",
		Args:    &args,
	}
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rsp, err := client.ProcessExecWithResponse(reqCtx, req)
	if err != nil {
		return err
	}
	if rsp.JSON200 == nil {
		return fmt.Errorf("touch %s returned status %s", path, rsp.Status())
	}
	if rsp.JSON200.ExitCode == nil || *rsp.JSON200.ExitCode != 0 {
		return fmt.Errorf("touch %s failed with exit code %v", path, rsp.JSON200.ExitCode)
	}
	return nil
}

func fetchBrowserWebSocketURL(ctx context.Context, c *TestContainer) (string, error) {
	// The CDP endpoint is served over TLS behind the hypeman :9222 ingress
	// (wss), so /json/version must be fetched over https there; docker is
	// plaintext (ws/http). Derive the HTTP scheme from the CDP URL's transport.
	scheme := "http"
	if strings.HasPrefix(c.CDPURL(), "wss://") {
		scheme = "https"
	}
	versionURL := fmt.Sprintf("%s://%s/json/version", scheme, c.CDPAddr())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, versionURL, nil)
	if err != nil {
		return "", err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("json/version returned %s", resp.Status)
	}

	var payload struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", err
	}
	if payload.WebSocketDebuggerURL == "" {
		return "", fmt.Errorf("json/version missing webSocketDebuggerUrl")
	}
	return payload.WebSocketDebuggerURL, nil
}

func waitForChangedBrowserWebSocketURL(ctx context.Context, c *TestContainer, previous string, timeout time.Duration) (string, error) {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		current, err := fetchBrowserWebSocketURL(waitCtx, c)
		if err == nil && current != "" && current != previous {
			return current, nil
		}

		select {
		case <-waitCtx.Done():
			if err != nil {
				return "", fmt.Errorf("waiting for changed browser websocket url: %w", err)
			}
			return "", waitCtx.Err()
		case <-ticker.C:
		}
	}
}

func TestCDPProxyReconnectPendingConnectionDuringRestart(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker not available: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	const hookPath = "/tmp/devtoolsproxy-race"
	c := NewTestContainer(t, headlessImage)
	require.NoError(t, c.Start(ctx, ContainerConfig{
		Env: map[string]string{
			"DEVTOOLS_PROXY_TEST_POST_CURRENT_BLOCK_FILE": hookPath,
		},
	}), "failed to start container")
	defer c.Stop(ctx)

	require.NoError(t, c.WaitReady(ctx), "api not ready")
	require.NoError(t, c.WaitDevTools(ctx), "devtools not ready")
	initialBrowserWS, err := fetchBrowserWebSocketURL(ctx, c)
	require.NoError(t, err, "failed to fetch initial browser websocket URL")

	apiClient, err := c.APIClientNoKeepAlive()
	require.NoError(t, err)

	initialConn, initialResult, err := connectAndExerciseCDP(ctx, c.CDPURL(), "before-restart")
	require.NoError(t, err, "initial CDP session failed")
	defer initialConn.Close()

	require.Equal(t, "before-restart", initialResult.Title)
	require.Equal(t, "before-restart", initialResult.Heading)
	require.Equal(t, 21, initialResult.Sum)
	require.Equal(t, "complete", initialResult.ReadyState)
	require.Greater(t, initialResult.ScreenshotBytes, 1000)

	type reconnectResult struct {
		client *cdpClient
		data   cdpExerciseResult
		err    error
	}
	reconnectCh := make(chan reconnectResult, 1)
	reconnectURL := c.CDPURL() + "?devtoolsProxyTestHook=1"
	go func() {
		client, data, err := connectAndExerciseCDP(ctx, reconnectURL, "after-restart")
		reconnectCh <- reconnectResult{client: client, data: data, err: err}
	}()

	require.NoError(t, waitForContainerFile(ctx, apiClient, hookPath+".ready", 15*time.Second), "pending reconnect never reached post-current hook")

	require.NoError(t, restartChromiumViaAPI(ctx, apiClient), "restart chromium failed")

	closeCtx, closeCancel := context.WithTimeout(ctx, 30*time.Second)
	defer closeCancel()
	closeErr := initialConn.WaitClosed(closeCtx)
	require.Error(t, closeErr, "expected initial CDP connection to close on chromium restart")
	require.False(t, errors.Is(closeErr, context.DeadlineExceeded), "timed out waiting for initial CDP connection to close")

	_, err = initialConn.Call(ctx, "Browser.getVersion", map[string]any{}, "")
	require.Error(t, err, "expected stale CDP connection to reject commands after restart")

	updatedBrowserWS, err := waitForChangedBrowserWebSocketURL(ctx, c, initialBrowserWS, 20*time.Second)
	require.NoError(t, err, "proxy never exposed a new browser websocket URL after restart")
	require.NotEqual(t, initialBrowserWS, updatedBrowserWS)
	require.NoError(t, touchContainerFile(ctx, apiClient, hookPath+".release"), "failed to release pending reconnect")

	select {
	case reconnect := <-reconnectCh:
		require.NoError(t, reconnect.err, "reconnect CDP session failed")
		if reconnect.client != nil {
			defer reconnect.client.Close()
		}
		require.Equal(t, "after-restart", reconnect.data.Title)
		require.Equal(t, "after-restart", reconnect.data.Heading)
		require.Equal(t, 21, reconnect.data.Sum)
		require.Equal(t, "complete", reconnect.data.ReadyState)
		require.Greater(t, reconnect.data.ScreenshotBytes, 1000)
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for reconnect CDP session")
	}
}
