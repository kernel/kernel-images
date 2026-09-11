package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/kernel/kernel-images/server/lib/cdpmonitor"
	"github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/require"
)

type telemetryUpstream string

func (u telemetryUpstream) Current() string                    { return string(u) }
func (u telemetryUpstream) Subscribe() (<-chan string, func()) { return nil, func() {} }

func TestTelemetryCleanupDoesNotBlockAPI(t *testing.T) {
	for _, method := range []string{"PUT", "PATCH"} {
		for _, ending := range []string{"resume", "timeout", "shutdown"} {
			t.Run(method+"/"+ending, func(t *testing.T) { testTelemetryCleanupDoesNotBlockAPI(t, method, ending) })
		}
	}
}

func testTelemetryCleanupDoesNotBlockAPI(t *testing.T, method, ending string) {
	svc, err := newSvc(t, newMockRecordManager())
	require.NoError(t, err)
	registered, blocked, release, disabledDomains := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var registerOnce, blockOnce, releaseOnce, disableOnce sync.Once
	var runtimeEnables, connections atomic.Int32
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	var socketMu, writes sync.Mutex
	var socket *websocket.Conn
	send := func(ctx context.Context, conn *websocket.Conn, value any) error {
		writes.Lock()
		defer writes.Unlock()
		return wsjson.Write(ctx, conn, value)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		connections.Add(1)
		socketMu.Lock()
		socket = conn
		socketMu.Unlock()
		for {
			var command struct {
				ID     int    `json:"id"`
				Method string `json:"method"`
			}
			if wsjson.Read(r.Context(), conn, &command) != nil {
				return
			}
			result := map[string]any{}
			switch command.Method {
			case "Target.getTargets":
				result["targetInfos"] = []any{map[string]any{"targetId": "page", "type": "page"}}
			case "Target.attachToTarget":
				result["sessionId"] = "session"
			case "Runtime.enable":
				runtimeEnables.Add(1)
			case "Runtime.disable":
				disableOnce.Do(func() { close(disabledDomains) })
			case "Page.addScriptToEvaluateOnNewDocument":
				result["identifier"] = "script"
			case "Page.removeScriptToEvaluateOnNewDocument":
				delay := false
				blockOnce.Do(func() { delay = true; close(blocked) })
				if delay {
					go func(id int) {
						select {
						case <-release:
						case <-time.After(11 * time.Second):
						case <-r.Context().Done():
							return
						}
						_ = send(r.Context(), conn, map[string]any{"id": id, "result": map[string]any{}})
					}(command.ID)
					continue
				}
			}
			if send(r.Context(), conn, map[string]any{"id": command.ID, "result": result}) != nil {
				return
			}
			if command.Method == "Page.addScriptToEvaluateOnNewDocument" {
				registerOnce.Do(func() { close(registered) })
			}
		}
	}))
	defer server.Close()
	mon := cdpmonitor.New(telemetryUpstream("ws"+strings.TrimPrefix(server.URL, "http")), svc.telemetrySession.Publish, 0, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	svc.cdpMonitor = mon
	require.NoError(t, mon.SetTelemetry(false))
	require.NoError(t, svc.StartNetworkMonitor())
	defer svc.Shutdown(context.Background())
	defer unblock()
	require.Eventually(t, func() bool { return mon.NetworkSnapshot().Up }, time.Second, time.Millisecond)
	on := true
	enabled := oapi.PutTelemetryRequestObject{Body: &oapi.BrowserTelemetryConfig{Browser: &oapi.BrowserTelemetryCategoriesConfig{Console: &oapi.BrowserTelemetryCategoryConfig{Enabled: &on}}}}
	_, err = svc.PutTelemetry(context.Background(), enabled)
	require.NoError(t, err)
	select {
	case <-registered:
	case <-time.After(time.Second):
		t.Fatal("optional registration not reached")
	}
	disabled := &oapi.BrowserTelemetryConfig{Browser: allCategoriesDisabled()}
	disable := func(ctx context.Context) any {
		if method == "PATCH" {
			response, _ := svc.PatchTelemetry(ctx, oapi.PatchTelemetryRequestObject{Body: disabled})
			return response
		}
		response, _ := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{Body: disabled})
		return response
	}
	assertDisabled := func(response any) {
		t.Helper()
		if method == "PATCH" {
			require.IsType(t, oapi.PatchTelemetry200JSONResponse{}, response)
		} else {
			require.IsType(t, oapi.PutTelemetry200JSONResponse{}, response)
		}
	}
	requestCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan any, 1)
	started := time.Now()
	go func() { done <- disable(requestCtx) }()
	select {
	case <-blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("cleanup command not reached")
	}
	select {
	case response := <-done:
		require.Less(t, time.Since(started), time.Second)
		t.Logf("%s disable response while cleanup is blocked: %s", method, time.Since(started))
		assertDisabled(response)
	case <-requestCtx.Done():
		t.Fatal("telemetry response exceeded a one-second request budget")
	}
	cancel() // Cleanup must outlive this request.
	require.False(t, svc.telemetrySession.Active())
	// GET and a newer enable both finish while the previous cleanup is blocked.
	requests := make(chan struct{})
	var getResponse oapi.GetTelemetryResponseObject
	go func() {
		getResponse, _ = svc.GetTelemetry(context.Background(), oapi.GetTelemetryRequestObject{})
		_, _ = svc.PutTelemetry(context.Background(), enabled)
		close(requests)
	}()
	select {
	case <-requests:
	case <-time.After(time.Second):
		t.Fatal("cleanup held the API-wide lock")
	}
	require.IsType(t, oapi.GetTelemetry404JSONResponse{}, getResponse)
	require.True(t, svc.telemetrySession.Active())
	seq := svc.eventStream.Seq()
	socketMu.Lock()
	conn := socket
	socketMu.Unlock()
	before := mon.NetworkSnapshot()
	require.NoError(t, send(context.Background(), conn, map[string]any{"method": "Runtime.consoleAPICalled", "sessionId": "session", "params": map[string]any{"type": "log", "args": []any{map[string]any{"type": "string", "value": "old-capture"}}}}))
	require.NoError(t, send(context.Background(), conn, map[string]any{"method": "Network.loadingFailed", "sessionId": "session", "params": map[string]any{"requestId": "during-cleanup", "errorText": "net::ERR_CONNECTION_RESET"}}))
	require.Eventually(t, func() bool { return mon.NetworkSnapshot().Resets == before.Resets+1 }, time.Second, time.Millisecond)
	require.Equal(t, seq, svc.eventStream.Seq(), "old capture published into the newer telemetry session")
	finalDisable := make(chan any, 1)
	go func() { finalDisable <- disable(context.Background()) }()
	select {
	case response := <-finalDisable:
		assertDisabled(response)
	case <-time.After(time.Second):
		t.Fatal("newer disable blocked on cleanup")
	}
	if ending == "resume" {
		unblock()
		select {
		case <-disabledDomains:
		case <-time.After(time.Second):
			t.Fatal("cleanup did not resume")
		}
		require.Never(t, func() bool { return runtimeEnables.Load() != 1 }, 250*time.Millisecond, time.Millisecond, "obsolete enable revision ran after the final disable")
		require.False(t, svc.telemetrySession.Active())
	}
	if ending == "timeout" {
		require.Eventually(t, func() bool { return connections.Load() >= 2 && mon.NetworkSnapshot().Up }, 6*time.Second, 10*time.Millisecond)
		require.EqualValues(t, 1, runtimeEnables.Load(), "cleanup recovery enabled stale telemetry")
		require.False(t, svc.telemetrySession.Active())
	}
	require.Equal(t, before.Resets+1, mon.NetworkSnapshot().Resets)
	require.Equal(t, before.Completed+1, mon.NetworkSnapshot().Completed)
	shutdown := make(chan struct{})
	go func() { _ = svc.Shutdown(context.Background()); close(shutdown) }()
	select {
	case <-shutdown:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown deadlocked behind cleanup")
	}
	require.False(t, mon.IsRunning())
}
