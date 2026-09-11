package api

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/require"
)

func vaultRequest() *oapi.VaultFillRequest {
	value := "secret-value"
	return &oapi.VaultFillRequest{Bindings: []oapi.VaultFillBinding{{Selector: "#password", Type: oapi.Password, Value: &value}}}
}

func TestVaultFillValidation(t *testing.T) {
	require.True(t, validVaultFillRequest(vaultRequest()))
	for _, mutate := range []func(*oapi.VaultFillRequest){
		func(r *oapi.VaultFillRequest) { r.Bindings = nil },
		func(r *oapi.VaultFillRequest) { r.Bindings = make([]oapi.VaultFillBinding, 101) },
		func(r *oapi.VaultFillRequest) { r.Bindings[0].Value = nil },
		func(r *oapi.VaultFillRequest) { r.Bindings[0].Selector = "" },
		func(r *oapi.VaultFillRequest) { r.Bindings[0].Type = "secret-value" },
		func(r *oapi.VaultFillRequest) { value := strings.Repeat("x", 65537); r.Bindings[0].Value = &value },
		func(r *oapi.VaultFillRequest) { timeout := 30001; r.TimeoutMs = &timeout },
		func(r *oapi.VaultFillRequest) { timeout := 0; r.TimeoutMs = &timeout },
	} {
		request := vaultRequest()
		mutate(request)
		require.False(t, validVaultFillRequest(request))
	}
	require.False(t, validVaultFillRequest(nil))
}

func TestVaultFillSanitizesDaemonResults(t *testing.T) {
	for _, data := range []string{
		`{"status":"secret-value","fields":[]}`,
		`{"status":"filled","fields":[{"index":0,"status":"secret-value"}]}`,
		`{"status":"filled","fields":[{"index":1,"status":"filled"}]}`,
		`{"status":"filled","fields":null}`,
		`secret-value`,
	} {
		result := parseVaultFillResult([]byte(data), 1)
		require.Equal(t, unknownVaultFillResult(1), result)
	}
	result := parseVaultFillResult([]byte(`{"status":"filled","fields":[{"index":0,"status":"filled","value":"secret-value"}],"error":"secret-value"}`), 1)
	encoded, err := json.Marshal(result)
	require.NoError(t, err)
	require.JSONEq(t, `{"status":"filled","fields":[{"index":0,"status":"filled"}]}`, string(encoded))
}

func TestVaultFillHTTPValidationAndTelemetry(t *testing.T) {
	withTelemetryMiddlewareEnabled(t)
	rp := &recordingPublisher{}
	service := &ApiService{}
	// Valid requests fail busy before daemon startup or any write.
	service.playwrightMu.Lock()
	defer service.playwrightMu.Unlock()
	strict := oapi.NewStrictHandlerWithOptions(service, []oapi.StrictMiddlewareFunc{TelemetryStrictMiddleware()}, oapi.StrictHTTPServerOptions{
		RequestErrorHandlerFunc: StrictRequestErrorHandler, ResponseErrorHandlerFunc: StrictResponseErrorHandler,
	})
	handler := TelemetryHTTPMiddleware(rp.publish)(VaultFillRequestMiddleware(oapi.Handler(strict)))
	for _, body := range []string{
		`{"bindings":[{"selector":"#password","value":"secret-value","type":"password"}],"timeout_ms":"secret-value"}`,
		`{"bindings":[{"selector":"#password","value":"secret-value","type":"secret-value"}]}`,
		`{"bindings":[{"selector":"#password","type":"password"}]}`,
		`{"bindings":[{"selector":"#password","value":null,"type":"password"}]}`,
		`{"secret-value":`,
		`null`,
		strings.Repeat("secret-value", maxVaultFillRequestBytes),
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/vault/fill", strings.NewReader(body)))
		require.Equal(t, http.StatusBadRequest, recorder.Code)
		require.JSONEq(t, `{"message":"invalid_request"}`, recorder.Body.String())
	}
	body, err := json.Marshal(vaultRequest())
	require.NoError(t, err)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/vault/fill", strings.NewReader(string(body))))
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	require.JSONEq(t, `{"message":"executor_unavailable"}`, recorder.Body.String())
	events := rp.snapshot()
	require.NotEmpty(t, events)
	for _, event := range events {
		require.NotContains(t, string(event.Data), "secret-value")
		require.NotContains(t, string(event.Data), "#password")
		require.NotContains(t, string(event.Data), `"code"`)
	}
	for _, handler := range []func(http.ResponseWriter, *http.Request, error){StrictRequestErrorHandler, StrictResponseErrorHandler} {
		recorder := httptest.NewRecorder()
		handler(recorder, httptest.NewRequest(http.MethodPost, "/vault/fill", nil), errors.New("secret-value"))
		require.NotContains(t, recorder.Body.String(), "secret-value")
	}
}

func TestVaultFillHTTPProtocol(t *testing.T) {
	listener, err := net.Listen("unix", playwrightDaemonSocket)
	require.NoError(t, err)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			var request vaultDaemonRequest
			if json.NewDecoder(conn).Decode(&request) != nil {
				_ = conn.Close()
				continue
			}
			var result any = map[string]int{"version": 1}
			if request.Method == "vault_fill" {
				result = map[string]any{"status": "filled", "fields": []map[string]any{{"index": 0, "status": "filled", "value": "secret-value"}}, "error": "secret-value"}
			}
			_ = json.NewEncoder(conn).Encode(map[string]any{"id": request.ID, "success": true, "result": result})
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() { _ = listener.Close(); <-done })
	withTelemetryMiddlewareEnabled(t)
	rp := &recordingPublisher{}
	strict := oapi.NewStrictHandlerWithOptions(&ApiService{}, []oapi.StrictMiddlewareFunc{TelemetryStrictMiddleware()}, oapi.StrictHTTPServerOptions{
		RequestErrorHandlerFunc: StrictRequestErrorHandler, ResponseErrorHandlerFunc: StrictResponseErrorHandler,
	})
	handler := TelemetryHTTPMiddleware(rp.publish)(VaultFillRequestMiddleware(oapi.Handler(strict)))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/vault/fill/capabilities", nil))
	require.Equal(t, http.StatusOK, recorder.Code)
	require.JSONEq(t, `{"version":1}`, recorder.Body.String())
	body, err := json.Marshal(vaultRequest())
	require.NoError(t, err)
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/vault/fill", strings.NewReader(string(body))))
	require.Equal(t, http.StatusOK, recorder.Code)
	require.JSONEq(t, `{"status":"filled","fields":[{"index":0,"status":"filled"}]}`, recorder.Body.String())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/vault/fill", strings.NewReader(string(body))).WithContext(ctx))
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	require.JSONEq(t, `{"message":"executor_unavailable"}`, recorder.Body.String())
	for _, event := range rp.snapshot() {
		require.NotContains(t, string(event.Data), "secret-value")
		require.NotContains(t, string(event.Data), "#password")
		require.NotContains(t, string(event.Data), `"code"`)
	}
}

func TestVaultDaemonTransport(t *testing.T) {
	_, err := callVaultDaemon(context.Background(), filepath.Join(t.TempDir(), "missing.sock"), vaultDaemonRequest{Method: "vault_fill", Request: vaultRequest()}, time.Second)
	require.ErrorIs(t, err, errVaultExecutorUnavailable)
	for _, mode := range []string{"success", "raw_error", "mismatch", "oversized", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			socket := filepath.Join(t.TempDir(), "daemon.sock")
			listener, err := net.Listen("unix", socket)
			require.NoError(t, err)
			defer listener.Close()
			done := make(chan struct{})
			go func() {
				defer close(done)
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				var request vaultDaemonRequest
				if json.NewDecoder(conn).Decode(&request) != nil {
					return
				}
				switch mode {
				case "raw_error":
					_, _ = conn.Write([]byte(`{"error":"secret-value","stack":"secret-value"}` + "\n"))
				case "mismatch":
					_ = json.NewEncoder(conn).Encode(map[string]any{"id": "secret-value", "success": true})
				case "oversized":
					_, _ = conn.Write([]byte(strings.Repeat("secret-value", 10000) + "\n"))
				case "timeout":
					var b [1]byte
					_, _ = conn.Read(b[:])
				default:
					_ = json.NewEncoder(conn).Encode(map[string]any{"id": request.ID, "success": true, "result": map[string]int{"version": 1}})
				}
			}()
			result, err := callVaultDaemon(context.Background(), socket, vaultDaemonRequest{Method: "vault_fill", Request: vaultRequest()}, 100*time.Millisecond)
			if mode == "success" {
				require.NoError(t, err)
				require.JSONEq(t, `{"version":1}`, string(result))
			} else {
				require.Error(t, err)
				require.NotContains(t, err.Error(), "secret-value")
			}
			<-done
		})
	}
}
