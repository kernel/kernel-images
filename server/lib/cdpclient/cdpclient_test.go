package cdpclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeCDP is a minimal CDP server that responds to the three commands used by
// SetDeviceMetricsOverride: Target.getTargets, Target.attachToTarget,
// Emulation.setDeviceMetricsOverride, and Target.detachFromTarget.
type fakeCDP struct {
	getTargetsCalled     bool
	attachCalled         bool
	setMetricsCalled     bool
	setMetricsWidth      int
	setMetricsHeight     int
	detachCalled         bool
	pageTargetID         string
	sessionID            string
	failGetTargets       bool
	failSetMetrics       bool
	returnNoPageTargets  bool
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
		var cdpErr *cdpError

		switch req.Method {
		case "Target.getTargets":
			f.getTargetsCalled = true
			if f.failGetTargets {
				cdpErr = &cdpError{Code: -1, Message: "mock error"}
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
				cdpErr = &cdpError{Code: -2, Message: "metrics error"}
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

func TestDial(t *testing.T) {
	t.Run("invalid URL", func(t *testing.T) {
		ctx := context.Background()
		_, err := Dial(ctx, "ws://127.0.0.1:0/invalid")
		require.Error(t, err)
	})
}
