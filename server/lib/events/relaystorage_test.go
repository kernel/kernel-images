package events

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const relayTestPath = "/btel-relay/v1/records"

type relayRequest struct {
	auth    string
	records []relayRecord
}

// fakeRelay records each request and answers with respond(n), n counting
// from 1. A nil respond acknowledges everything.
type fakeRelay struct {
	srv     *httptest.Server
	mu      sync.Mutex
	reqs    []relayRequest
	respond func(n int, w http.ResponseWriter) bool
}

func newFakeRelay(t *testing.T) *fakeRelay {
	f := &fakeRelay{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != relayTestPath || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Records []relayRecord `json:"records"`
		}
		require.NoError(t, json.Unmarshal(raw, &body))
		f.mu.Lock()
		f.reqs = append(f.reqs, relayRequest{auth: r.Header.Get("Authorization"), records: body.Records})
		n, respond := len(f.reqs), f.respond
		f.mu.Unlock()
		if respond != nil && respond(n, w) {
			return
		}
		_, _ = io.WriteString(w, `{"start_seq_num":0,"end_seq_num":1}`)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRelay) requests() []relayRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]relayRequest(nil), f.reqs...)
}

func (f *fakeRelay) records() []relayRecord {
	var out []relayRecord
	for _, r := range f.requests() {
		out = append(out, r.records...)
	}
	return out
}

func testRelayConfig(base string) RelayConfig {
	return RelayConfig{
		BaseURLFunc: func() string { return base },
		Path:        relayTestPath,
		TokenFunc:   func() string { return "instance-jwt" },
		Linger:      10 * time.Millisecond,
		Metrics:     &RelayMetrics{},
		retry:       relayRetry{initial: 5 * time.Millisecond, max: 20 * time.Millisecond, maxElapsed: 5 * time.Second},
	}
}

func newTestRelayStorage(t *testing.T, cfg RelayConfig) *relayStorage {
	t.Helper()
	s, err := newRelayStorage(context.Background(), cfg, slog.Default())
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s
}

func closeRelay(t *testing.T, s *relayStorage) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, s.Close(ctx))
}

func relayEnvelope(seq uint64, data string) Envelope {
	return Envelope{Seq: seq, Event: Event{
		Ts:       1_700_000_000_123_456 + int64(seq),
		Type:     "console.log",
		Category: Console,
		Data:     json.RawMessage(data),
	}}
}

func TestRelayStorage_SendsEnvelopesInOrderUnchanged(t *testing.T) {
	relay := newFakeRelay(t)
	s := newTestRelayStorage(t, testRelayConfig(relay.srv.URL))

	envs := []Envelope{
		relayEnvelope(1, `{"text":"<b>&amp;</b>"}`),
		relayEnvelope(2, `{"text":"  line"}`),
		relayEnvelope(3, `{}`),
	}
	for _, env := range envs {
		require.NoError(t, s.Append(context.Background(), env))
	}
	closeRelay(t, s)

	got := relay.records()
	require.Len(t, got, len(envs))
	for i, env := range envs {
		want, err := json.Marshal(env)
		require.NoError(t, err)
		assert.Equal(t, string(want), string(got[i].Body), "body must match the direct S2 sink's record byte for byte")
		assert.Equal(t, env.Event.Ts/1000, got[i].TS, "ts is the event time in milliseconds")
	}
	assert.Equal(t, "Bearer instance-jwt", relay.requests()[0].auth)
	assert.Equal(t, uint64(3), s.cfg.Metrics.Exported())
}

func TestRelayStorage_FlushesOnRecordAndByteLimits(t *testing.T) {
	relay := newFakeRelay(t)
	cfg := testRelayConfig(relay.srv.URL)
	cfg.Linger = time.Hour
	cfg.MaxRecords = 3
	s := newTestRelayStorage(t, cfg)

	for i := range 3 {
		require.NoError(t, s.Append(context.Background(), relayEnvelope(uint64(i), `{}`)))
	}
	require.Eventually(t, func() bool { return len(relay.requests()) == 1 }, 5*time.Second, 5*time.Millisecond,
		"a full batch flushes without waiting for the linger")

	relay2 := newFakeRelay(t)
	payload := `{"pad":"` + string(make([]byte, 0)) + `xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"}`
	env := relayEnvelope(1, payload)
	body, _ := json.Marshal(env)
	cfg = testRelayConfig(relay2.srv.URL)
	cfg.Linger = time.Hour
	cfg.MaxBytes = 2*(relayMeteredOverhead+len(body)) + 1
	s2 := newTestRelayStorage(t, cfg)
	for range 5 {
		require.NoError(t, s2.Append(context.Background(), env))
	}
	require.Eventually(t, func() bool { return len(relay2.requests()) == 2 }, 5*time.Second, 5*time.Millisecond)
	for _, req := range relay2.requests() {
		size := 0
		for _, rec := range req.records {
			size += rec.metered()
		}
		assert.LessOrEqual(t, size, cfg.MaxBytes)
	}
	closeRelay(t, s2)
	assert.Len(t, relay2.records(), 5)
}

func TestRelayStorage_RetriesRetryableStatuses(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			relay := newFakeRelay(t)
			relay.respond = func(n int, w http.ResponseWriter) bool {
				if n > 2 {
					return false
				}
				w.WriteHeader(status)
				return true
			}
			s := newTestRelayStorage(t, testRelayConfig(relay.srv.URL))
			require.NoError(t, s.Append(context.Background(), relayEnvelope(1, `{}`)))
			closeRelay(t, s)

			assert.Len(t, relay.requests(), 3)
			assert.Equal(t, uint64(2), s.cfg.Metrics.Retries())
			assert.Equal(t, uint64(1), s.cfg.Metrics.Exported())
			assert.Empty(t, s.cfg.Metrics.Dropped())
		})
	}
}

func TestRelayStorage_HonorsRetryAfter(t *testing.T) {
	relay := newFakeRelay(t)
	relay.respond = func(n int, w http.ResponseWriter) bool {
		if n > 1 {
			return false
		}
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusServiceUnavailable)
		return true
	}
	s := newTestRelayStorage(t, testRelayConfig(relay.srv.URL))
	start := time.Now()
	require.NoError(t, s.Append(context.Background(), relayEnvelope(1, `{}`)))
	closeRelay(t, s)
	assert.GreaterOrEqual(t, time.Since(start), time.Second)
	assert.Len(t, relay.requests(), 2)
}

func TestRelayStorage_DropsNonRetryableStatuses(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusRequestEntityTooLarge, http.StatusInternalServerError} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			relay := newFakeRelay(t)
			relay.respond = func(_ int, w http.ResponseWriter) bool {
				w.WriteHeader(status)
				return true
			}
			s := newTestRelayStorage(t, testRelayConfig(relay.srv.URL))
			require.NoError(t, s.Append(context.Background(), relayEnvelope(1, `{}`)))
			require.NoError(t, s.Append(context.Background(), relayEnvelope(2, `{}`)))
			closeRelay(t, s)

			assert.Len(t, relay.requests(), 1, "no retry")
			assert.Equal(t, map[string]uint64{relayDropRejected: 2}, s.cfg.Metrics.Dropped())
			assert.Equal(t, map[string]uint64{strconv.Itoa(status): 1}, s.cfg.Metrics.Failed())
			assert.Zero(t, s.cfg.Metrics.Retries())
		})
	}
}

func TestRelayStorage_DropsOnConnectionFailure(t *testing.T) {
	relay := newFakeRelay(t)
	relay.srv.Close()
	s := newTestRelayStorage(t, testRelayConfig(relay.srv.URL))
	require.NoError(t, s.Append(context.Background(), relayEnvelope(1, `{}`)))
	closeRelay(t, s)
	assert.Equal(t, map[string]uint64{"error": 1}, s.cfg.Metrics.Failed())
	assert.Equal(t, map[string]uint64{relayDropRejected: 1}, s.cfg.Metrics.Dropped())
}

func TestRelayStorage_DoesNotFollowRedirects(t *testing.T) {
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { elsewhere.Add(1) }))
	defer other.Close()
	relay := newFakeRelay(t)
	relay.respond = func(_ int, w http.ResponseWriter) bool {
		w.Header().Set("Location", other.URL+relayTestPath)
		w.WriteHeader(http.StatusTemporaryRedirect)
		return true
	}
	s := newTestRelayStorage(t, testRelayConfig(relay.srv.URL))
	require.NoError(t, s.Append(context.Background(), relayEnvelope(1, `{}`)))
	closeRelay(t, s)
	assert.Zero(t, elsewhere.Load(), "the credential must not follow a redirect")
	assert.Equal(t, map[string]uint64{"307": 1}, s.cfg.Metrics.Failed())
}

// A fork learns its token and metro-api after the sink is built, so both are
// read per batch.
func TestRelayStorage_ResolvesTokenAndBaseURLPerBatch(t *testing.T) {
	seed, fork := newFakeRelay(t), newFakeRelay(t)
	var base, token atomic.Pointer[string]
	setIdentity := func(b, tok string) { base.Store(&b); token.Store(&tok) }
	setIdentity(seed.srv.URL+"/ignored-base-path", "seed-jwt")

	cfg := testRelayConfig("")
	cfg.BaseURLFunc = func() string { return *base.Load() }
	cfg.TokenFunc = func() string { return *token.Load() }
	s := newTestRelayStorage(t, cfg)

	require.NoError(t, s.Append(context.Background(), relayEnvelope(1, `{}`)))
	require.Eventually(t, func() bool { return len(seed.requests()) == 1 }, 5*time.Second, 5*time.Millisecond)
	setIdentity(fork.srv.URL, "fork-jwt")
	require.NoError(t, s.Append(context.Background(), relayEnvelope(2, `{}`)))
	closeRelay(t, s)

	assert.Equal(t, "Bearer seed-jwt", seed.requests()[0].auth)
	require.Len(t, fork.requests(), 1)
	assert.Equal(t, "Bearer fork-jwt", fork.requests()[0].auth)
}

func TestRelayStorage_DropsOldestWhenQueueIsFull(t *testing.T) {
	release := make(chan struct{})
	relay := newFakeRelay(t)
	relay.respond = func(n int, _ http.ResponseWriter) bool {
		if n == 1 {
			<-release
		}
		return false
	}
	cfg := testRelayConfig(relay.srv.URL)
	cfg.MaxRecords = 1
	cfg.MaxQueue = 3
	s := newTestRelayStorage(t, cfg)

	require.NoError(t, s.Append(context.Background(), relayEnvelope(0, `{}`)))
	require.Eventually(t, func() bool { return len(relay.requests()) == 1 }, 5*time.Second, 5*time.Millisecond)
	for i := 1; i <= 10; i++ {
		require.NoError(t, s.Append(context.Background(), relayEnvelope(uint64(i), `{}`)))
	}
	assert.Equal(t, int64(3), s.cfg.Metrics.QueueDepth())
	assert.Equal(t, map[string]uint64{relayDropQueueFull: 7}, s.cfg.Metrics.Dropped())
	close(release)
	closeRelay(t, s)

	var seqs []uint64
	for _, rec := range relay.records() {
		var env Envelope
		require.NoError(t, json.Unmarshal(rec.Body, &env))
		seqs = append(seqs, env.Seq)
	}
	assert.Equal(t, []uint64{0, 8, 9, 10}, seqs, "the newest records survive")
}

func TestRelayStorage_CloseDropsWhatItCannotFlush(t *testing.T) {
	relay := newFakeRelay(t)
	relay.respond = func(_ int, w http.ResponseWriter) bool {
		w.WriteHeader(http.StatusServiceUnavailable)
		return true
	}
	cfg := testRelayConfig(relay.srv.URL)
	cfg.retry = relayRetry{initial: time.Hour, max: time.Hour, maxElapsed: 2 * time.Hour}
	s, err := newRelayStorage(context.Background(), cfg, slog.Default())
	require.NoError(t, err)
	require.NoError(t, s.Append(context.Background(), relayEnvelope(1, `{}`)))
	require.Eventually(t, func() bool { return len(relay.requests()) == 1 }, 5*time.Second, 5*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, s.Close(ctx), context.DeadlineExceeded)
	assert.Equal(t, uint64(1), cfg.Metrics.Dropped()[relayDropRetriesExhausted])
	require.NoError(t, s.Append(context.Background(), relayEnvelope(2, `{}`)))
	assert.Equal(t, uint64(1), cfg.Metrics.Dropped()[relayDropShutdown], "appends after Close are dropped")
}

func TestRelayURL(t *testing.T) {
	for base, want := range map[string]string{
		"https://169.254.0.1:8443":     "https://169.254.0.1:8443/metro/btel-relay/v1/records",
		"http://127.0.0.1:4000/metro/": "http://127.0.0.1:4000/metro/btel-relay/v1/records",
		"https://h.example/x?y=1#z":    "https://h.example/metro/btel-relay/v1/records",
	} {
		got, err := relayURL(base, "/metro/btel-relay/v1/records")
		require.NoError(t, err)
		assert.Equal(t, want, got, base)
	}
	for _, base := range []string{"", "://", "ftp://h/", "https://"} {
		_, err := relayURL(base, relayTestPath)
		assert.Error(t, err, base)
	}
}
