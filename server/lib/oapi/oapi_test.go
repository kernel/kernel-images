package oapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

type sseTestWriter struct {
	header  http.Header
	status  int
	body    bytes.Buffer
	flushes int
}

func (w *sseTestWriter) Header() http.Header { return w.header }

func (w *sseTestWriter) WriteHeader(status int) { w.status = status }

func (w *sseTestWriter) Write(p []byte) (int, error) { return w.body.Write(p) }

func (w *sseTestWriter) Flush() { w.flushes++ }

func TestGeneratedSSEResponsesFlushAndDisableBuffering(t *testing.T) {
	tests := []struct {
		name  string
		visit func(http.ResponseWriter) error
	}{
		{
			name: "filesystem events",
			visit: func(w http.ResponseWriter) error {
				return (StreamFsEvents200TexteventStreamResponse{Body: strings.NewReader("data: fs\n\n")}).VisitStreamFsEventsResponse(w)
			},
		},
		{
			name: "logs",
			visit: func(w http.ResponseWriter) error {
				return (LogsStream200TexteventStreamResponse{Body: strings.NewReader("data: logs\n\n")}).VisitLogsStreamResponse(w)
			},
		},
		{
			name: "process stdout",
			visit: func(w http.ResponseWriter) error {
				return (ProcessStdoutStream200TexteventStreamResponse{Body: strings.NewReader("data: stdout\n\n")}).VisitProcessStdoutStreamResponse(w)
			},
		},
		{
			name: "telemetry",
			visit: func(w http.ResponseWriter) error {
				return (StreamTelemetryEvents200TexteventStreamResponse{Body: strings.NewReader("data: telemetry\n\n")}).VisitStreamTelemetryEventsResponse(w)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := &sseTestWriter{header: make(http.Header)}
			if err := tt.visit(w); err != nil {
				t.Fatalf("visit response: %v", err)
			}
			if w.status != http.StatusOK {
				t.Fatalf("status = %d, want %d", w.status, http.StatusOK)
			}
			if got := w.header.Get("Content-Type"); got != "text/event-stream" {
				t.Fatalf("Content-Type = %q", got)
			}
			if got := w.header.Get("Cache-Control"); got != "no-cache" {
				t.Fatalf("Cache-Control = %q", got)
			}
			if got := w.header.Get("X-Accel-Buffering"); got != "no" {
				t.Fatalf("X-Accel-Buffering = %q", got)
			}
			if w.flushes != 1 {
				t.Fatalf("flushes = %d, want 1", w.flushes)
			}
		})
	}
}

func TestGeneratedOpenAPISpecMatchesSourceDescription(t *testing.T) {
	swagger, err := GetSwagger()
	if err != nil {
		t.Fatalf("get swagger: %v", err)
	}
	data, err := json.Marshal(swagger)
	if err != nil {
		t.Fatalf("marshal swagger: %v", err)
	}
	if !bytes.Contains(data, []byte("including the 1,000-item cap on stray output buffered between executions")) {
		t.Fatal("embedded OpenAPI spec is missing the stray-output item limit description")
	}
}
