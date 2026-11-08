package logger

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	chiMiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func decodeLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &m), line)
		lines = append(lines, m)
	}
	return lines
}

func TestRequestLogger(t *testing.T) {
	for _, tc := range []struct {
		status int
		level  string
	}{
		{http.StatusOK, "INFO"},
		{http.StatusSwitchingProtocols, "INFO"},
		{http.StatusNotFound, "WARN"},
		{http.StatusBadGateway, "ERROR"},
	} {
		var buf bytes.Buffer
		log := slog.New(slog.NewJSONHandler(&buf, nil))
		r := chi.NewRouter()
		r.Use(chiMiddleware.RequestID, RequestLogger(log))
		r.Get("/process/{id}", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte("hi"))
		})

		req := httptest.NewRequest(http.MethodGet, "http://secret-host.example/process/abc?url=https://user:pass@site.example/x", nil)
		req.Header.Set(chiMiddleware.RequestIDHeader, "hypeman/abc-1")
		r.ServeHTTP(httptest.NewRecorder(), req)

		for _, leak := range []string{"secret-host", "user:pass", "site.example", "/process/abc"} {
			assert.NotContains(t, buf.String(), leak)
		}
		lines := decodeLines(t, &buf)
		require.Len(t, lines, 1)
		line := lines[0]
		assert.Equal(t, tc.level, line["level"])
		assert.Equal(t, "request", line["msg"])
		assert.Equal(t, "hypeman/abc-1", line["request_id"])
		if tc.status >= http.StatusOK {
			assert.EqualValues(t, 2, line["bytes"])
		}
		attrs := line[AttrsKey].(map[string]any)
		assert.Equal(t, "GET", attrs["http.request.method"])
		assert.EqualValues(t, tc.status, attrs["http.response.status_code"])
		assert.Equal(t, "/process/{id}", attrs["http.route"])
		assert.Contains(t, attrs, "duration_ms")
		assert.NotContains(t, attrs, "error.type")
	}
}

func TestRequestLoggerDefaultsToOK(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	h := RequestLogger(log)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	line := decodeLines(t, &buf)[0]
	assert.Equal(t, "INFO", line["level"])
	assert.NotContains(t, line, "request_id")
	attrs := line[AttrsKey].(map[string]any)
	assert.EqualValues(t, http.StatusOK, attrs["http.response.status_code"])
	assert.NotContains(t, attrs, "http.route")
}

func TestRequestLoggerAborted(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	h := RequestLogger(log)(Recoverer(log)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	})))
	assert.PanicsWithValue(t, http.ErrAbortHandler, func() {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	})

	lines := decodeLines(t, &buf)
	require.Len(t, lines, 1)
	assert.Equal(t, "ERROR", lines[0]["level"])
	attrs := lines[0][AttrsKey].(map[string]any)
	assert.Equal(t, "aborted", attrs["error.type"])
	assert.NotContains(t, attrs, "http.response.status_code")
}

func TestRecoverer(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	h := RequestLogger(log)(Recoverer(log)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("token=secret " + strings.Repeat("é", 300))
	})))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	lines := decodeLines(t, &buf)
	require.Len(t, lines, 2)
	panicLine, requestLine := lines[0], lines[1]
	assert.Equal(t, "ERROR", panicLine["level"])
	assert.Equal(t, "panic", panicLine["msg"])
	assert.Equal(t, "token=secret "+strings.Repeat("é", 300), panicLine["panic"])
	assert.Contains(t, panicLine["stack"], "TestRecoverer")
	assert.Equal(t, map[string]any{"exception.type": "string"}, panicLine[AttrsKey])
	assert.Equal(t, "ERROR", requestLine["level"])
	assert.EqualValues(t, http.StatusInternalServerError, requestLine[AttrsKey].(map[string]any)["http.response.status_code"])
}

func TestRecovererLeavesUpgradesAlone(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))
	h := Recoverer(log)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusSwitchingProtocols)
		panic("after upgrade")
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Connection", "Upgrade")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusSwitchingProtocols, rec.Code)
}

func TestRecovererRepanicsAbort(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	h := Recoverer(log)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	assert.PanicsWithValue(t, http.ErrAbortHandler, func() {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	})
	assert.Empty(t, buf.String())
}
