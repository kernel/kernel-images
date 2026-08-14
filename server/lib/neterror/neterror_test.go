package neterror

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestTracker() *Tracker {
	return NewTracker(slog.New(slog.DiscardHandler))
}

func loadingFailed(errorText, resourceType string, canceled bool) []byte {
	return fmt.Appendf(nil,
		`{"method":"Network.loadingFailed","params":{"requestId":"1","timestamp":1.5,"type":%q,"errorText":%q,"canceled":%t}}`,
		resourceType, errorText, canceled)
}

func TestObserveCountsHTTP2ProtocolError(t *testing.T) {
	tr := newTestTracker()
	tr.Observe(loadingFailed("net::ERR_HTTP2_PROTOCOL_ERROR", "Document", false))
	tr.Observe(loadingFailed("net::ERR_HTTP2_PROTOCOL_ERROR", "Document", false))
	tr.Observe(loadingFailed("net::ERR_HTTP2_PROTOCOL_ERROR", "XHR", false))

	counts, overflow := tr.Snapshot()
	assert.Zero(t, overflow)
	assert.Equal(t, map[Key]int64{
		{Error: "net::ERR_HTTP2_PROTOCOL_ERROR", ResourceType: "Document"}: 2,
		{Error: "net::ERR_HTTP2_PROTOCOL_ERROR", ResourceType: "XHR"}:      1,
	}, counts)
}

func TestObserveIgnoresNonFailures(t *testing.T) {
	tr := newTestTracker()
	for _, msg := range []string{
		`{"method":"Network.responseReceived","params":{"requestId":"1"}}`,
		`{"id":7,"result":{}}`,
		// The needle appears in a payload that is not the event itself.
		`{"method":"Network.dataReceived","params":{"body":"\"Network.loadingFailed\""}}`,
		// Malformed JSON that still contains the needle.
		`{"method":"Network.loadingFailed","params":{`,
	} {
		tr.Observe([]byte(msg))
	}

	counts, overflow := tr.Snapshot()
	assert.Empty(t, counts)
	assert.Zero(t, overflow)
}

func TestObserveSkipsCancellations(t *testing.T) {
	tr := newTestTracker()
	// Client-initiated aborts show up either as canceled or as ERR_ABORTED.
	tr.Observe(loadingFailed("net::ERR_ABORTED", "Fetch", false))
	tr.Observe(loadingFailed("net::ERR_HTTP2_PROTOCOL_ERROR", "Fetch", true))

	counts, _ := tr.Snapshot()
	assert.Empty(t, counts)
}

func TestObserveRequiresNetErrorPrefix(t *testing.T) {
	tr := newTestTracker()
	for _, errorText := range []string{"", "Failed", "ERR_HTTP2_PROTOCOL_ERROR", "net::OK", "blocked"} {
		tr.Observe(loadingFailed(errorText, "Fetch", false))
	}

	counts, overflow := tr.Snapshot()
	assert.Empty(t, counts)
	assert.Zero(t, overflow)
}

// route.abort() with Playwright's default reason is reported as ERR_FAILED
// with canceled false, so it is indistinguishable from a genuine generic
// failure and is deliberately counted. It must stay in its own series rather
// than contaminate the specific errors these counts exist to track.
func TestObserveCountsErrFailedSeparately(t *testing.T) {
	tr := newTestTracker()
	for range 50 {
		tr.Observe(loadingFailed("net::ERR_FAILED", "Image", false))
	}
	tr.Observe(loadingFailed("net::ERR_HTTP2_PROTOCOL_ERROR", "Document", false))

	counts, _ := tr.Snapshot()
	assert.EqualValues(t, 50, counts[Key{Error: "net::ERR_FAILED", ResourceType: "Image"}])
	assert.EqualValues(t, 1, counts[Key{Error: "net::ERR_HTTP2_PROTOCOL_ERROR", ResourceType: "Document"}])
}

func TestRecordCapsDistinctKeys(t *testing.T) {
	tr := newTestTracker()
	for i := range maxKeys + 10 {
		tr.Observe(loadingFailed(fmt.Sprintf("net::ERR_SYNTHETIC_%d", i), "Other", false))
	}
	// A key already tracked still counts after the cap is hit.
	tr.Observe(loadingFailed("net::ERR_SYNTHETIC_0", "Other", false))

	counts, overflow := tr.Snapshot()
	assert.Len(t, counts, maxKeys)
	assert.EqualValues(t, 10, overflow)
	assert.EqualValues(t, 2, counts[Key{Error: "net::ERR_SYNTHETIC_0", ResourceType: "Other"}])
}

func TestRecordThrottlesLogging(t *testing.T) {
	var logged int
	tr := newTestTracker()
	tr.logger = slog.New(countingHandler{n: &logged})

	now := time.Now()
	tr.now = func() time.Time { return now }

	// The first failure logs; the rest of the burst is absorbed by the metric.
	for range 5 {
		tr.Observe(loadingFailed("net::ERR_HTTP2_PROTOCOL_ERROR", "Document", false))
	}
	require.Equal(t, 1, logged)

	now = now.Add(logInterval)
	tr.Observe(loadingFailed("net::ERR_HTTP2_PROTOCOL_ERROR", "Document", false))
	assert.Equal(t, 2, logged)
}

func TestObserveIsConcurrencySafe(t *testing.T) {
	tr := newTestTracker()
	msg := loadingFailed("net::ERR_HTTP2_PROTOCOL_ERROR", "Document", false)

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				tr.Observe(msg)
			}
		})
	}
	wg.Wait()

	counts, _ := tr.Snapshot()
	assert.EqualValues(t, 800, counts[Key{Error: "net::ERR_HTTP2_PROTOCOL_ERROR", ResourceType: "Document"}])
}

func TestSortedKeysIsStable(t *testing.T) {
	counts := map[Key]int64{
		{Error: "net::ERR_TIMED_OUT", ResourceType: "XHR"}:            1,
		{Error: "net::ERR_HTTP2_PROTOCOL_ERROR", ResourceType: "XHR"}: 1,
		{Error: "net::ERR_HTTP2_PROTOCOL_ERROR", ResourceType: "Doc"}: 1,
	}
	assert.Equal(t, []Key{
		{Error: "net::ERR_HTTP2_PROTOCOL_ERROR", ResourceType: "Doc"},
		{Error: "net::ERR_HTTP2_PROTOCOL_ERROR", ResourceType: "XHR"},
		{Error: "net::ERR_TIMED_OUT", ResourceType: "XHR"},
	}, SortedKeys(counts))
}

type countingHandler struct {
	slog.Handler
	n *int
}

func (h countingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h countingHandler) Handle(context.Context, slog.Record) error {
	*h.n++
	return nil
}

func (h countingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h countingHandler) WithGroup(string) slog.Handler      { return h }

func BenchmarkObserveNonFailure(b *testing.B) {
	tr := newTestTracker()
	// A typical relayed frame: no needle, so it pays only the substring scan.
	msg := []byte(`{"method":"Network.responseReceived","params":{"requestId":"42","loaderId":"7","timestamp":1234.5,"type":"Script","response":{"url":"https://example.com/app.js","status":200,"mimeType":"text/javascript"}}}`)
	for b.Loop() {
		tr.Observe(msg)
	}
}
