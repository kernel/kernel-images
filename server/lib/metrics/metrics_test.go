package metrics

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriterSample(t *testing.T) {
	w := &Writer{}
	w.Metric("test_metric", "A test metric.", "gauge")
	w.Sample("test_metric", nil, 42)
	w.Sample("test_metric", []Label{{"mode", "user"}, {"mount", "/"}}, 1.5)
	w.Sample("test_metric", []Label{{"name", "quote\"back\\slash\nnewline"}}, math.Inf(1))

	assert.Equal(t, `# HELP test_metric A test metric.
# TYPE test_metric gauge
test_metric 42
test_metric{mode="user",mount="/"} 1.5
test_metric{name="quote\"back\\slash\nnewline"} +Inf
`, string(w.Bytes()))
}

type stubCollector struct {
	name string
	fn   func(w *Writer) error
}

func (s *stubCollector) Name() string { return s.name }
func (s *stubCollector) Collect(_ context.Context, w *Writer) error {
	return s.fn(w)
}

func TestHandlerDiscardsFailedCollectorOutput(t *testing.T) {
	failing := &stubCollector{name: "bad", fn: func(w *Writer) error {
		w.Metric("bad_partial", "A partially written family.", "histogram")
		w.Sample("bad_partial_sum", nil, 1)
		return errors.New("boom")
	}}
	ok := &stubCollector{name: "good", fn: func(w *Writer) error {
		w.Metric("good_total", "Good things.", "counter")
		w.Sample("good_total", nil, 3)
		return nil
	}}

	h := Handler(slog.New(slog.DiscardHandler), failing, ok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))

	require.Equal(t, 200, rec.Code)
	assert.Equal(t, "text/plain; version=0.0.4; charset=utf-8", rec.Header().Get("Content-Type"))
	assert.NotContains(t, rec.Body.String(), "bad_partial")
	assert.Contains(t, rec.Body.String(), "good_total 3\n")
}
