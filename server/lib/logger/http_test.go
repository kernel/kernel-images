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

		req := httptest.NewRequest(http.MethodGet, "/process/abc?x=1", nil)
		req.Header.Set(chiMiddleware.RequestIDHeader, "hypeman/abc-1")
		r.ServeHTTP(httptest.NewRecorder(), req)

		lines := decodeLines(t, &buf)
		require.Len(t, lines, 1)
		line := lines[0]
		assert.Equal(t, tc.level, line["level"])
		assert.Equal(t, "request", line["msg"])
		assert.Equal(t, "http://example.com/process/abc?x=1", line["url"])
		assert.Equal(t, "hypeman/abc-1", line["request_id"])
		assert.EqualValues(t, tc.status, line["status"])
		if tc.status >= http.StatusOK {
			assert.EqualValues(t, 2, line["bytes"])
		}
		attrs := line[AttrsKey].(map[string]any)
		assert.Equal(t, "GET", attrs["http.request.method"])
		assert.EqualValues(t, tc.status, attrs["http.response.status_code"])
		assert.Equal(t, "/process/{id}", attrs["http.route"])
		assert.Contains(t, attrs, "duration_ms")
		assert.NotContains(t, attrs, "url")
	}
}

func TestRequestLoggerDefaultsToOK(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	h := RequestLogger(log)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	line := decodeLines(t, &buf)[0]
	assert.Equal(t, "INFO", line["level"])
	assert.EqualValues(t, http.StatusOK, line["status"])
	assert.NotContains(t, line, "request_id")
	assert.NotContains(t, line[AttrsKey], "http.route")
}

func TestRecoverer(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	h := RequestLogger(log)(Recoverer(log)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("x" + strings.Repeat("é", 300))
	})))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	lines := decodeLines(t, &buf)
	require.Len(t, lines, 2)
	panicLine, requestLine := lines[0], lines[1]
	assert.Equal(t, "ERROR", panicLine["level"])
	assert.Equal(t, "panic", panicLine["msg"])
	assert.Equal(t, "x"+strings.Repeat("é", 300), panicLine["panic"])
	assert.Contains(t, panicLine["stack"], "TestRecoverer")
	attrs := panicLine[AttrsKey].(map[string]any)
	assert.Equal(t, "panic", attrs["exception.type"])
	// Cut at 512 bytes, without the half of a rune that falls on the boundary.
	assert.Equal(t, "x"+strings.Repeat("é", 255), attrs["exception.message"])
	assert.Equal(t, "ERROR", requestLine["level"])
	assert.EqualValues(t, http.StatusInternalServerError, requestLine["status"])
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
