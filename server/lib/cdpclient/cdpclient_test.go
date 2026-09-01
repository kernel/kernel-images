package cdpclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeCDP is a minimal CDP server that responds to the commands used by
// SetDeviceMetricsOverride and GetBrowserVersion.
type fakeCDP struct {
	getTargetsCalled    bool
	attachCalled        bool
	setMetricsCalled    bool
	setMetricsWidth     int
	setMetricsHeight    int
	detachCalled        bool
	pageTargetID        string
	sessionID           string
	failGetTargets      bool
	failSetMetrics      bool
	returnNoPageTargets bool
	getVersionCalled    bool
	failGetVersion      bool
	productResponse     string
	loadUnpackedCalled  bool
	loadUnpackedPath    string
	loadUnpackedID      string
	failLoadUnpacked    bool
	getExtensionsCalled bool
	extensions          []ExtensionInfo
	failGetExtensions   bool
	navigateCalled      bool
	navigateCalls       int
	navigateURL         string
	pageStates          []string
	pageStateIndex      int
}

func (f *fakeCDP) handler(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	ctx := r.Context()

	for {
		_, msg, err := conn.Read(ctx)
		if err != nil {
			return
		}

		var req cdpRequest
		if err := json.Unmarshal(msg, &req); err != nil {
			continue
		}

		var result any
		var cdpErr *Error

		switch req.Method {
		case "Target.getTargets":
			f.getTargetsCalled = true
			if f.failGetTargets {
				cdpErr = &Error{Code: -1, Message: "mock error"}
			} else {
				targets := []map[string]string{}
				if !f.returnNoPageTargets {
					targets = append(targets, map[string]string{
						"targetId": f.pageTargetID,
						"type":     "page",
					})
				}
				result = map[string]any{"targetInfos": targets}
			}
		case "Target.attachToTarget":
			f.attachCalled = true
			result = map[string]string{"sessionId": f.sessionID}
		case "Emulation.setDeviceMetricsOverride":
			f.setMetricsCalled = true
			if f.failSetMetrics {
				cdpErr = &Error{Code: -2, Message: "metrics error"}
			} else {
				var params map[string]any
				_ = json.Unmarshal(req.Params, &params)
				f.setMetricsWidth = int(params["width"].(float64))
				f.setMetricsHeight = int(params["height"].(float64))
				result = map[string]any{}
			}
		case "Target.detachFromTarget":
			f.detachCalled = true
			result = map[string]any{}
		case "Browser.getVersion":
			f.getVersionCalled = true
			if f.failGetVersion {
				cdpErr = &Error{Code: -3, Message: "version error"}
			} else {
				product := f.productResponse
				if product == "" {
					product = "HeadlessChrome/test"
				}
				result = map[string]any{
					"protocolVersion": "1.3",
					"product":         product,
					"revision":        "@deadbeef",
					"userAgent":       "Mozilla/5.0 fake",
					"jsVersion":       "1.2.3",
				}
			}
		case "Extensions.loadUnpacked":
			f.loadUnpackedCalled = true
			var params map[string]string
			_ = json.Unmarshal(req.Params, &params)
			f.loadUnpackedPath = params["path"]
			if f.failLoadUnpacked {
				cdpErr = &Error{Code: -4, Message: "invalid extension"}
			} else {
				result = map[string]string{"id": f.loadUnpackedID}
			}
		case "Extensions.getExtensions":
			f.getExtensionsCalled = true
			if f.failGetExtensions {
				cdpErr = &Error{Code: -5, Message: "extensions unavailable"}
			} else {
				result = map[string]any{"extensions": f.extensions}
			}
		case "Page.navigate":
			f.navigateCalled = true
			f.navigateCalls++
			var params map[string]any
			_ = json.Unmarshal(req.Params, &params)
			f.navigateURL, _ = params["url"].(string)
			result = map[string]any{"frameId": "frame-1"}
		case "Runtime.evaluate":
			state := `{"url":"about:blank","readyState":"loading"}`
			if len(f.pageStates) > 0 {
				state = f.pageStates[f.pageStateIndex]
				if f.pageStateIndex < len(f.pageStates)-1 {
					f.pageStateIndex++
				}
			}
			result = map[string]any{"result": map[string]any{"type": "string", "value": state}}
		}

		resp := map[string]any{"id": req.ID}
		if cdpErr != nil {
			resp["error"] = cdpErr
		} else {
			resp["result"] = result
		}

		b, _ := json.Marshal(resp)
		_ = conn.Write(ctx, websocket.MessageText, b)
	}
}

func startFakeCDP(t *testing.T, f *fakeCDP) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

func TestSetDeviceMetricsOverride(t *testing.T) {
	t.Run("happy path", func(t *testing.T) {
		f := &fakeCDP{
			pageTargetID: "target-123",
			sessionID:    "session-abc",
		}
		url := startFakeCDP(t, f)

		ctx := context.Background()
		client, err := Dial(ctx, url)
		require.NoError(t, err)
		defer client.Close()

		err = client.SetDeviceMetricsOverride(ctx, 1920, 1080)
		require.NoError(t, err)

		assert.True(t, f.getTargetsCalled)
		assert.True(t, f.attachCalled)
		assert.True(t, f.setMetricsCalled)
		assert.True(t, f.detachCalled)
		assert.Equal(t, 1920, f.setMetricsWidth)
		assert.Equal(t, 1080, f.setMetricsHeight)
	})

	t.Run("no page target", func(t *testing.T) {
		f := &fakeCDP{
			returnNoPageTargets: true,
		}
		url := startFakeCDP(t, f)

		ctx := context.Background()
		client, err := Dial(ctx, url)
		require.NoError(t, err)
		defer client.Close()

		err = client.SetDeviceMetricsOverride(ctx, 1920, 1080)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no page target found")
	})

	t.Run("getTargets failure", func(t *testing.T) {
		f := &fakeCDP{
			failGetTargets: true,
		}
		url := startFakeCDP(t, f)

		ctx := context.Background()
		client, err := Dial(ctx, url)
		require.NoError(t, err)
		defer client.Close()

		err = client.SetDeviceMetricsOverride(ctx, 1920, 1080)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "Target.getTargets")
	})

	t.Run("setDeviceMetrics failure", func(t *testing.T) {
		f := &fakeCDP{
			pageTargetID:   "target-123",
			sessionID:      "session-abc",
			failSetMetrics: true,
		}
		url := startFakeCDP(t, f)

		ctx := context.Background()
		client, err := Dial(ctx, url)
		require.NoError(t, err)
		defer client.Close()

		err = client.SetDeviceMetricsOverride(ctx, 800, 600)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "Emulation.setDeviceMetricsOverride")
	})

	t.Run("context cancellation", func(t *testing.T) {
		f := &fakeCDP{
			pageTargetID: "target-123",
			sessionID:    "session-abc",
		}
		url := startFakeCDP(t, f)

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		_, err := Dial(ctx, url)
		require.Error(t, err)
	})
}

func TestDispatchStartURLAndWait(t *testing.T) {
	t.Run("waits for loaded destination", func(t *testing.T) {
		f := &fakeCDP{
			pageTargetID: "target-123",
			sessionID:    "session-abc",
			pageStates: []string{
				`{"url":"chrome-error://chromewebdata/","readyState":"complete"}`,
				`{"url":"https://start.duckduckgo.com/","readyState":"loading"}`,
				`{"url":"https://duckduckgo.com/","readyState":"complete"}`,
			},
		}
		url := startFakeCDP(t, f)

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		err := DispatchStartURLAndWait(ctx, url, "chrome://newtab/", "https://start.duckduckgo.com/")
		require.NoError(t, err)
		assert.True(t, f.navigateCalled)
		assert.Equal(t, "chrome://newtab/", f.navigateURL)
	})

	t.Run("retries a failed initial navigation", func(t *testing.T) {
		f := &fakeCDP{
			pageTargetID: "target-123",
			sessionID:    "session-abc",
			pageStates: []string{
				`{"url":"chrome-error://chromewebdata/","readyState":"complete"}`,
				`{"url":"chrome-error://chromewebdata/","readyState":"complete"}`,
				`{"url":"chrome-error://chromewebdata/","readyState":"complete"}`,
				`{"url":"chrome-error://chromewebdata/","readyState":"complete"}`,
				`{"url":"chrome-error://chromewebdata/","readyState":"complete"}`,
				`{"url":"chrome-error://chromewebdata/","readyState":"complete"}`,
				`{"url":"https://start.duckduckgo.com/","readyState":"complete"}`,
			},
		}
		url := startFakeCDP(t, f)

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		err := DispatchStartURLAndWait(ctx, url, "chrome://newtab/", "https://start.duckduckgo.com/")
		require.NoError(t, err)
		assert.GreaterOrEqual(t, f.navigateCalls, 2)
		assert.Equal(t, "chrome://newtab/", f.navigateURL)
	})

	t.Run("times out on Chrome error page", func(t *testing.T) {
		f := &fakeCDP{
			pageTargetID: "target-123",
			sessionID:    "session-abc",
			pageStates:   []string{`{"url":"chrome-error://chromewebdata/","readyState":"complete"}`},
		}
		url := startFakeCDP(t, f)

		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		err := DispatchStartURLAndWait(ctx, url, "chrome://newtab/", "https://start.duckduckgo.com/")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "chrome-error://chromewebdata/")
	})
}

func TestBrowserWebSocketURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"webSocketDebuggerUrl":"ws://127.0.0.1/devtools/browser/test"}`))
	}))
	defer srv.Close()

	got, err := BrowserWebSocketURL(context.Background(), srv.URL)
	require.NoError(t, err)
	assert.Equal(t, "ws://127.0.0.1/devtools/browser/test", got)
}

func TestDial(t *testing.T) {
	t.Run("invalid URL", func(t *testing.T) {
		ctx := context.Background()
		_, err := Dial(ctx, "ws://127.0.0.1:0/invalid")
		require.Error(t, err)
	})
}

func TestGetBrowserVersion(t *testing.T) {
	t.Run("happy path", func(t *testing.T) {
		f := &fakeCDP{productResponse: "HeadlessChrome/145.0.0.0"}
		url := startFakeCDP(t, f)

		ctx := context.Background()
		client, err := Dial(ctx, url)
		require.NoError(t, err)
		defer client.Close()

		v, err := client.GetBrowserVersion(ctx)
		require.NoError(t, err)
		require.NotNil(t, v)
		assert.True(t, f.getVersionCalled)
		assert.Equal(t, "HeadlessChrome/145.0.0.0", v.Product)
		assert.Equal(t, "1.3", v.ProtocolVersion)
	})

	t.Run("CDP error from chromium", func(t *testing.T) {
		f := &fakeCDP{failGetVersion: true}
		url := startFakeCDP(t, f)

		ctx := context.Background()
		client, err := Dial(ctx, url)
		require.NoError(t, err)
		defer client.Close()

		_, err = client.GetBrowserVersion(ctx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "Browser.getVersion")
	})
}

func TestGetExtensions(t *testing.T) {
	t.Run("happy path", func(t *testing.T) {
		want := []ExtensionInfo{{
			ID:      "abcdefghijklmnopabcdefghijklmnop",
			Name:    "Test Extension",
			Version: "1.0",
			Path:    "/home/kernel/extensions/test",
			Enabled: true,
		}}
		f := &fakeCDP{extensions: want}
		url := startFakeCDP(t, f)

		client, err := Dial(context.Background(), url)
		require.NoError(t, err)
		defer client.Close()

		got, err := client.GetExtensions(context.Background())
		require.NoError(t, err)
		assert.Equal(t, want, got)
		assert.True(t, f.getExtensionsCalled)
	})

	t.Run("CDP error", func(t *testing.T) {
		f := &fakeCDP{failGetExtensions: true}
		url := startFakeCDP(t, f)

		client, err := Dial(context.Background(), url)
		require.NoError(t, err)
		defer client.Close()

		_, err = client.GetExtensions(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "Extensions.getExtensions")
	})
}

func TestLoadUnpackedExtension(t *testing.T) {
	t.Run("happy path", func(t *testing.T) {
		f := &fakeCDP{loadUnpackedID: "abcdefghijklmnopabcdefghijklmnop"}
		url := startFakeCDP(t, f)

		client, err := Dial(context.Background(), url)
		require.NoError(t, err)
		defer client.Close()

		id, err := client.LoadUnpackedExtension(context.Background(), "/home/kernel/extensions/test")
		require.NoError(t, err)
		assert.Equal(t, f.loadUnpackedID, id)
		assert.True(t, f.loadUnpackedCalled)
		assert.Equal(t, "/home/kernel/extensions/test", f.loadUnpackedPath)
	})

	t.Run("CDP error from chromium", func(t *testing.T) {
		f := &fakeCDP{failLoadUnpacked: true}
		url := startFakeCDP(t, f)

		client, err := Dial(context.Background(), url)
		require.NoError(t, err)
		defer client.Close()

		_, err = client.LoadUnpackedExtension(context.Background(), "/bad-extension")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "Extensions.loadUnpacked")
	})

	t.Run("missing extension ID", func(t *testing.T) {
		f := &fakeCDP{}
		url := startFakeCDP(t, f)

		client, err := Dial(context.Background(), url)
		require.NoError(t, err)
		defer client.Close()

		_, err = client.LoadUnpackedExtension(context.Background(), "/home/kernel/extensions/test")
		require.EqualError(t, err, "Extensions.loadUnpacked returned no extension ID")
	})
}

func TestClientCorrelatesConcurrentCommandsAndPreservesEvents(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		requests := make([]cdpRequest, 0, 2)
		for len(requests) < cap(requests) {
			_, payload, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			var request cdpRequest
			if json.Unmarshal(payload, &request) == nil {
				requests = append(requests, request)
			}
		}
		for _, request := range requests {
			event, _ := json.Marshal(map[string]any{
				"method": "Test.event", "sessionId": request.SessionID,
				"params": map[string]any{"command": request.Method},
			})
			_ = conn.Write(r.Context(), websocket.MessageText, event)
		}
		for i := len(requests) - 1; i >= 0; i-- {
			request := requests[i]
			response, _ := json.Marshal(map[string]any{
				"id": request.ID, "result": map[string]any{"command": request.Method},
			})
			_ = conn.Write(r.Context(), websocket.MessageText, response)
		}
	}))
	defer server.Close()

	client, err := DialWithEvents(context.Background(), "ws"+strings.TrimPrefix(server.URL, "http"))
	require.NoError(t, err)
	defer client.Close()

	commands := []string{"Browser.getVersion", "Target.getTargets"}
	type result struct {
		command string
		err     error
	}
	results := make(chan result, len(commands))
	var wg sync.WaitGroup
	for _, command := range commands {
		wg.Add(1)
		go func() {
			defer wg.Done()
			raw, sendErr := client.Send(context.Background(), command, nil, "session-1")
			if sendErr != nil {
				results <- result{err: sendErr}
				return
			}
			var response struct {
				Command string `json:"command"`
			}
			if unmarshalErr := json.Unmarshal(raw, &response); unmarshalErr != nil {
				results <- result{err: unmarshalErr}
				return
			}
			results <- result{command: response.Command}
		}()
	}
	wg.Wait()
	close(results)

	seenResults := make(map[string]bool)
	for result := range results {
		require.NoError(t, result.err)
		seenResults[result.command] = true
	}
	for _, command := range commands {
		require.True(t, seenResults[command])
	}

	seenEvents := make(map[string]bool)
	for range commands {
		event := <-client.Events()
		require.Equal(t, "Test.event", event.Method)
		var params struct {
			Command string `json:"command"`
		}
		require.NoError(t, json.Unmarshal(event.Params, &params))
		seenEvents[params.Command] = true
	}
	for _, command := range commands {
		require.True(t, seenEvents[command])
	}
}

func TestClientPreservesResponseBeforeDisconnect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		_, payload, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		var request cdpRequest
		if json.Unmarshal(payload, &request) != nil {
			return
		}
		response, _ := json.Marshal(map[string]any{
			"id": request.ID, "result": map[string]any{"completed": true},
		})
		_ = conn.Write(r.Context(), websocket.MessageText, response)
	}))
	defer server.Close()

	client, err := Dial(context.Background(), "ws"+strings.TrimPrefix(server.URL, "http"))
	require.NoError(t, err)
	defer client.Close()

	raw, err := client.Send(context.Background(), "Test.complete", nil, "")
	require.NoError(t, err)
	require.JSONEq(t, `{"completed":true}`, string(raw))
}

func TestEventClientCloseUnblocksFullEventStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		event, _ := json.Marshal(map[string]any{"method": "Test.event"})
		for {
			if conn.Write(r.Context(), websocket.MessageText, event) != nil {
				return
			}
		}
	}))
	defer server.Close()

	client, err := DialWithEvents(context.Background(), "ws"+strings.TrimPrefix(server.URL, "http"))
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return len(client.Events()) == cap(client.events)
	}, time.Second, 10*time.Millisecond)
	require.NoError(t, client.Close())

	drained := make(chan struct{})
	go func() {
		for range client.Events() {
		}
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(time.Second):
		t.Fatal("event stream did not close")
	}
}

func TestClientMarksInFlightCommandUnknownOnDisconnect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		_, _, _ = conn.Read(r.Context())
	}))
	defer server.Close()

	client, err := Dial(context.Background(), "ws"+strings.TrimPrefix(server.URL, "http"))
	require.NoError(t, err)
	defer client.Close()

	_, err = client.Send(context.Background(), "Browser.getVersion", nil, "")
	require.ErrorIs(t, err, ErrOutcomeUnknown)
}

func TestCommandOnlyClientDiscardsEvents(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		_, payload, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		var request cdpRequest
		if json.Unmarshal(payload, &request) != nil {
			return
		}
		event, _ := json.Marshal(map[string]any{"method": "Test.event"})
		for range 300 {
			if conn.Write(r.Context(), websocket.MessageText, event) != nil {
				return
			}
		}
		response, _ := json.Marshal(map[string]any{"id": request.ID, "result": map[string]any{}})
		_ = conn.Write(r.Context(), websocket.MessageText, response)
	}))
	defer server.Close()

	client, err := Dial(context.Background(), "ws"+strings.TrimPrefix(server.URL, "http"))
	require.NoError(t, err)
	defer client.Close()
	require.Nil(t, client.Events())

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = client.Send(ctx, "Browser.getVersion", nil, "")
	require.NoError(t, err)
}
