package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	bidiDepsOnce sync.Once
	bidiDepsErr  error
)

func getBidiPath() string {
	return "./bidi"
}

func ensureBidiDeps(t *testing.T) {
	t.Helper()
	bidiDepsOnce.Do(func() {
		nodeModulesPath := getBidiPath() + "/node_modules"
		if _, err := os.Stat(nodeModulesPath); os.IsNotExist(err) {
			cmd := exec.Command("npm", "install")
			cmd.Dir = getBidiPath()
			// Vibium can download a local browser at install time, which is unnecessary
			// for remote-BiDi tests running against our containerized browser.
			cmd.Env = append(os.Environ(), "VIBIUM_SKIP_BROWSER_DOWNLOAD=1")
			output, err := cmd.CombinedOutput()
			if err != nil {
				bidiDepsErr = fmt.Errorf("failed to install bidi dependencies: %w\noutput: %s", err, string(output))
			}
		}
	})

	require.NoError(t, bidiDepsErr, "bidi dependency setup failed")
}

// bidiConn wraps a WebSocket connection for BiDi JSON-RPC communication.
// It runs a background read loop that dispatches command responses to pending
// callers and fires one-shot event listeners.
type bidiConn struct {
	conn   *websocket.Conn
	ctx    context.Context
	nextID int

	mu      sync.Mutex
	pending map[int]chan bidiResult

	eventMu   sync.Mutex
	listeners []*eventListener
}

type bidiResult struct {
	Result json.RawMessage
	Error  json.RawMessage
}

type eventListener struct {
	method string
	ch     chan json.RawMessage
}

func newBidiConn(ctx context.Context, conn *websocket.Conn) *bidiConn {
	bc := &bidiConn{
		conn:    conn,
		ctx:     ctx,
		pending: make(map[int]chan bidiResult),
	}
	go bc.readLoop()
	return bc
}

func (bc *bidiConn) readLoop() {
	for {
		_, data, err := bc.conn.Read(bc.ctx)
		if err != nil {
			return
		}

		var raw map[string]json.RawMessage
		if err := json.Unmarshal(data, &raw); err != nil {
			continue
		}

		// Command response (has "id" field)
		if idRaw, ok := raw["id"]; ok {
			var id int
			if json.Unmarshal(idRaw, &id) == nil {
				bc.mu.Lock()
				ch, found := bc.pending[id]
				if found {
					delete(bc.pending, id)
				}
				bc.mu.Unlock()
				if found {
					ch <- bidiResult{Result: raw["result"], Error: raw["error"]}
				}
			}
			continue
		}

		// Event (has "method" but no "id")
		if methodRaw, ok := raw["method"]; ok {
			var method string
			if json.Unmarshal(methodRaw, &method) == nil {
				bc.eventMu.Lock()
				for i, l := range bc.listeners {
					if l.method == method {
						l.ch <- raw["params"]
						bc.listeners = append(bc.listeners[:i], bc.listeners[i+1:]...)
						break
					}
				}
				bc.eventMu.Unlock()
			}
		}
	}
}

// send sends a BiDi command and blocks until the response arrives.
func (bc *bidiConn) send(method string, params interface{}) (json.RawMessage, error) {
	bc.mu.Lock()
	bc.nextID++
	id := bc.nextID
	ch := make(chan bidiResult, 1)
	bc.pending[id] = ch
	bc.mu.Unlock()

	data, err := json.Marshal(map[string]interface{}{
		"id": id, "method": method, "params": params,
	})
	if err != nil {
		return nil, err
	}

	if err := bc.conn.Write(bc.ctx, websocket.MessageText, data); err != nil {
		return nil, err
	}

	select {
	case <-bc.ctx.Done():
		return nil, bc.ctx.Err()
	case res := <-ch:
		if len(res.Error) > 0 && string(res.Error) != "null" {
			return nil, fmt.Errorf("bidi error: %s", string(res.Error))
		}
		return res.Result, nil
	}
}

// addListener registers a one-shot event listener and returns a channel
// that receives the event params when the matching event fires.
func (bc *bidiConn) addListener(method string) <-chan json.RawMessage {
	ch := make(chan json.RawMessage, 1)
	bc.eventMu.Lock()
	bc.listeners = append(bc.listeners, &eventListener{method: method, ch: ch})
	bc.eventMu.Unlock()
	return ch
}

// collectEvent waits for an event on the given channel with a timeout.
func collectEvent(t *testing.T, ch <-chan json.RawMessage, name string, timeout time.Duration) json.RawMessage {
	t.Helper()
	select {
	case params := <-ch:
		return params
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for event: %s", name)
		return nil
	}
}

// TestBidiWebSocket exercises the raw WebSocket BiDi protocol through the
// ChromeDriver proxy: session lifecycle, browsing context operations, event
// subscription, script evaluation, and navigation events.
func TestBidiWebSocket(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker not available: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	c := NewTestContainer(t, headlessImage)
	require.NoError(t, c.Start(ctx, ContainerConfig{}), "failed to start container")
	defer c.Stop(ctx)

	require.NoError(t, c.WaitReady(ctx), "api not ready")
	require.NoError(t, c.WaitChromeDriver(ctx), "chromedriver not ready")

	// Connect to BiDi WebSocket endpoint
	bidiURL := fmt.Sprintf("ws://127.0.0.1:%d/session", c.ChromeDriverPort)
	t.Logf("connecting to BiDi endpoint: %s", bidiURL)

	conn, _, err := websocket.Dial(ctx, bidiURL, nil)
	require.NoError(t, err, "failed to connect to BiDi WebSocket at %s", bidiURL)
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)

	bc := newBidiConn(ctx, conn)

	// 1. session.new
	result, err := bc.send("session.new", map[string]interface{}{
		"capabilities": map[string]interface{}{
			"alwaysMatch": map[string]interface{}{
				"webSocketUrl":            true,
				"unhandledPromptBehavior": map[string]string{"default": "ignore"},
			},
		},
	})
	require.NoError(t, err, "session.new failed")

	var session map[string]interface{}
	require.NoError(t, json.Unmarshal(result, &session))
	require.NotEmpty(t, session["sessionId"], "session should have an ID")
	t.Logf("session created: %v", session["sessionId"])

	// 2. session.status
	result, err = bc.send("session.status", map[string]interface{}{})
	require.NoError(t, err, "session.status failed")
	t.Logf("session.status: %s", string(result))

	// 3. browsingContext.getTree → extract context ID
	result, err = bc.send("browsingContext.getTree", map[string]interface{}{})
	require.NoError(t, err, "browsingContext.getTree failed")

	var tree map[string]interface{}
	require.NoError(t, json.Unmarshal(result, &tree))
	contexts, ok := tree["contexts"].([]interface{})
	require.True(t, ok && len(contexts) > 0, "should have at least one browsing context")
	contextID, ok := contexts[0].(map[string]interface{})["context"].(string)
	require.True(t, ok && contextID != "", "context ID should be a non-empty string")
	t.Logf("context ID: %s", contextID)

	// 4. session.subscribe
	_, err = bc.send("session.subscribe", map[string]interface{}{
		"events": []string{
			"log.entryAdded",
			"browsingContext.domContentLoaded",
			"browsingContext.load",
		},
	})
	require.NoError(t, err, "session.subscribe failed")

	// 5. Verify log.entryAdded fires on console.log
	logCh := bc.addListener("log.entryAdded")

	_, err = bc.send("script.evaluate", map[string]interface{}{
		"expression":   "console.log('hello from bidi e2e test')",
		"target":       map[string]string{"context": contextID},
		"awaitPromise": false,
	})
	require.NoError(t, err, "script.evaluate (console.log) failed")

	logParams := collectEvent(t, logCh, "log.entryAdded", 5*time.Second)
	t.Logf("log.entryAdded received: %s", string(logParams))

	// 6. Navigate and verify domContentLoaded + load events
	dclCh := bc.addListener("browsingContext.domContentLoaded")
	loadCh := bc.addListener("browsingContext.load")

	_, err = bc.send("browsingContext.navigate", map[string]interface{}{
		"context": contextID,
		"url":     "https://example.com",
		"wait":    "complete",
	})
	require.NoError(t, err, "browsingContext.navigate failed")

	collectEvent(t, dclCh, "browsingContext.domContentLoaded", 15*time.Second)
	t.Log("received browsingContext.domContentLoaded")

	collectEvent(t, loadCh, "browsingContext.load", 15*time.Second)
	t.Log("received browsingContext.load")

	// 7. Verify page title via script.evaluate
	result, err = bc.send("script.evaluate", map[string]interface{}{
		"expression":   "document.title",
		"target":       map[string]string{"context": contextID},
		"awaitPromise": false,
	})
	require.NoError(t, err, "script.evaluate (document.title) failed")
	require.Contains(t, string(result), "Example Domain",
		"page title should contain 'Example Domain', got: %s", string(result))
	t.Logf("document.title: %s", string(result))

	// 8. session.unsubscribe
	_, err = bc.send("session.unsubscribe", map[string]interface{}{
		"events": []string{
			"log.entryAdded",
			"browsingContext.domContentLoaded",
			"browsingContext.load",
		},
	})
	require.NoError(t, err, "session.unsubscribe failed")

	// 9. session.end
	_, err = bc.send("session.end", map[string]interface{}{})
	require.NoError(t, err, "session.end failed")

	conn.Close(websocket.StatusNormalClosure, "test complete")
	t.Log("BiDi WebSocket test passed")
}

// TestBidiHTTPSession tests the Selenium-style flow: HTTP POST /session to
// create a WebDriver session, verify the webSocketUrl is rewritten to point
// through the proxy, then connect via BiDi and run commands.
func TestBidiHTTPSession(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker not available: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	c := NewTestContainer(t, headlessImage)
	require.NoError(t, c.Start(ctx, ContainerConfig{}), "failed to start container")
	defer c.Stop(ctx)

	require.NoError(t, c.WaitReady(ctx), "api not ready")
	require.NoError(t, c.WaitChromeDriver(ctx), "chromedriver not ready")

	chromeDriverURL := c.ChromeDriverURL()

	// POST /session to create a WebDriver session
	sessionBody, err := json.Marshal(map[string]interface{}{
		"capabilities": map[string]interface{}{
			"alwaysMatch": map[string]interface{}{
				"browserName":             "chrome",
				"webSocketUrl":            true,
				"unhandledPromptBehavior": map[string]string{"default": "ignore"},
			},
		},
	})
	require.NoError(t, err)

	t.Logf("POST %s/session", chromeDriverURL)
	resp, err := http.Post(chromeDriverURL+"/session", "application/json", bytes.NewReader(sessionBody))
	require.NoError(t, err, "POST /session request failed")
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "expected 200 for POST /session")

	respBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	t.Logf("POST /session response: %s", string(respBody))

	// Parse response and extract session details
	var sessionResp map[string]interface{}
	require.NoError(t, json.Unmarshal(respBody, &sessionResp))

	value, ok := sessionResp["value"].(map[string]interface{})
	require.True(t, ok, "response should have 'value' object")
	sessionID, ok := value["sessionId"].(string)
	require.True(t, ok && sessionID != "", "session ID should be a non-empty string")

	caps, ok := value["capabilities"].(map[string]interface{})
	require.True(t, ok, "response should have 'capabilities'")
	wsURL, ok := caps["webSocketUrl"].(string)
	require.True(t, ok && wsURL != "", "webSocketUrl should be present in capabilities")

	t.Logf("session ID: %s, webSocketUrl: %s", sessionID, wsURL)

	// Verify the proxy rewrote webSocketUrl to point through itself
	expectedHost := fmt.Sprintf("127.0.0.1:%d", c.ChromeDriverPort)
	require.Contains(t, wsURL, expectedHost,
		"webSocketUrl should point through the proxy (expected host %s), got: %s", expectedHost, wsURL)

	// Connect to the BiDi WebSocket on the returned URL
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	require.NoError(t, err, "failed to connect to BiDi via webSocketUrl: %s", wsURL)
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)

	bc := newBidiConn(ctx, conn)

	// browsingContext.getTree should work on the existing session
	result, err := bc.send("browsingContext.getTree", map[string]interface{}{})
	require.NoError(t, err, "browsingContext.getTree failed on HTTP session")

	var tree map[string]interface{}
	require.NoError(t, json.Unmarshal(result, &tree))
	contexts, ok := tree["contexts"].([]interface{})
	require.True(t, ok && len(contexts) > 0, "should have at least one browsing context")
	contextID, ok := contexts[0].(map[string]interface{})["context"].(string)
	require.True(t, ok && contextID != "", "context ID should be a non-empty string")
	t.Logf("context ID: %s", contextID)

	// script.evaluate should work
	result, err = bc.send("script.evaluate", map[string]interface{}{
		"expression":   "'hello from HTTP session BiDi'",
		"target":       map[string]string{"context": contextID},
		"awaitPromise": false,
	})
	require.NoError(t, err, "script.evaluate failed on HTTP session")
	require.Contains(t, string(result), "hello from HTTP session BiDi")
	t.Logf("script.evaluate result: %s", string(result))

	conn.Close(websocket.StatusNormalClosure, "test complete")

	// Clean up: DELETE /session/{id}
	delReq, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		fmt.Sprintf("%s/session/%s", chromeDriverURL, sessionID), nil)
	require.NoError(t, err)

	delResp, err := http.DefaultClient.Do(delReq)
	require.NoError(t, err, "DELETE /session request failed")
	delResp.Body.Close()
	assert.Equal(t, http.StatusOK, delResp.StatusCode, "expected 200 for DELETE /session")

	t.Log("BiDi HTTP session test passed")
}

// TestBidiPuppeteer exercises Puppeteer's webDriverBiDi protocol through
// the ChromeDriver proxy by running the test-puppeteer-bidi.js script.
func TestBidiPuppeteer(t *testing.T) {
	t.Parallel()
	ensureBidiDeps(t)

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker not available: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	c := NewTestContainer(t, headlessImage)
	require.NoError(t, c.Start(ctx, ContainerConfig{}), "failed to start container")
	defer c.Stop(ctx)

	require.NoError(t, c.WaitReady(ctx), "api not ready")
	require.NoError(t, c.WaitChromeDriver(ctx), "chromedriver not ready")

	endpoint := fmt.Sprintf("ws://127.0.0.1:%d/session", c.ChromeDriverPort)
	t.Logf("running test-puppeteer-bidi.js against %s", endpoint)

	cmd := exec.CommandContext(ctx, "node", "test-puppeteer-bidi.js", "--endpoint", endpoint)
	cmd.Dir = getBidiPath()
	out, err := cmd.CombinedOutput()
	t.Logf("test-puppeteer-bidi.js output:\n%s", string(out))
	require.NoError(t, err, "test-puppeteer-bidi.js failed: %v", err)
}

// TestBidiVibium exercises Vibium's direct remote-browser connection over
// WebDriver BiDi by connecting to the proxy's ws://.../session endpoint and
// letting Vibium create the BiDi session itself.
func TestBidiVibium(t *testing.T) {
	t.Parallel()
	ensureBidiDeps(t)

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker not available: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	c := NewTestContainer(t, headlessImage)
	require.NoError(t, c.Start(ctx, ContainerConfig{}), "failed to start container")
	defer c.Stop(ctx)

	require.NoError(t, c.WaitReady(ctx), "api not ready")
	require.NoError(t, c.WaitChromeDriver(ctx), "chromedriver not ready")

	endpoint := fmt.Sprintf("ws://127.0.0.1:%d/session", c.ChromeDriverPort)
	t.Logf("running test-vibium-bidi.js against %s", endpoint)

	cmd := exec.CommandContext(ctx, "node", "test-vibium-bidi.js", "--endpoint", endpoint)
	cmd.Dir = getBidiPath()
	out, err := cmd.CombinedOutput()
	t.Logf("test-vibium-bidi.js output:\n%s", string(out))
	require.NoError(t, err, "test-vibium-bidi.js failed: %v", err)
}

// TestBidiSelenium exercises Selenium WebDriver's BiDi support through
// the ChromeDriver proxy by running the test-selenium-bidi.js script.
func TestBidiSelenium(t *testing.T) {
	t.Parallel()
	ensureBidiDeps(t)

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker not available: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	c := NewTestContainer(t, headlessImage)
	require.NoError(t, c.Start(ctx, ContainerConfig{}), "failed to start container")
	defer c.Stop(ctx)

	require.NoError(t, c.WaitReady(ctx), "api not ready")
	require.NoError(t, c.WaitChromeDriver(ctx), "chromedriver not ready")

	endpoint := fmt.Sprintf("http://127.0.0.1:%d", c.ChromeDriverPort)
	t.Logf("running test-selenium-bidi.js against %s", endpoint)

	cmd := exec.CommandContext(ctx, "node", "test-selenium-bidi.js", "--endpoint", endpoint)
	cmd.Dir = getBidiPath()
	out, err := cmd.CombinedOutput()
	t.Logf("test-selenium-bidi.js output:\n%s", string(out))
	require.NoError(t, err, "test-selenium-bidi.js failed: %v", err)
}
