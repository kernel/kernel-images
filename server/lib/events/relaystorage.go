package events

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"time"
)

// Relay batch defaults. maxBytes is in S2 metered bytes (8 + body length per
// record) and stays under the relay's 1 MiB append limit.
const (
	DefaultRelayMaxRecords = 200
	DefaultRelayMaxBytes   = 768 * 1024
	DefaultRelayLinger     = 100 * time.Millisecond
	DefaultRelayMaxQueue   = 8192

	relayMeteredOverhead = 8
	// relayExportTimeout bounds one batch, retries included, like the OTLP
	// sink's BTEL_OTLP_EXPORT_TIMEOUT default. It is shorter than the retry
	// policy's maxElapsed, so it is what ends a batch's retries in practice.
	relayExportTimeout = 30 * time.Second
	// relayDrainGrace bounds how long Close waits for the cancelled sender to
	// record its drops once its ctx has expired.
	relayDrainGrace = 250 * time.Millisecond
)

// relayRetry mirrors the OTLP exporter's retry policy: exponential backoff
// from 5s up to 30s with jitter, waiting the greater of the backoff and
// Retry-After, and giving up after a minute or relayExportTimeout, whichever
// comes first.
type relayRetry struct {
	initial, max, maxElapsed time.Duration
}

var defaultRelayRetry = relayRetry{initial: 5 * time.Second, max: 30 * time.Second, maxElapsed: time.Minute}

// RelayConfig configures the metro-api storage relay sink.
type RelayConfig struct {
	// BaseURLFunc resolves the metro-api base URL per batch, so a fork posts to
	// the host it was handed rather than its seed's. Only its scheme and host
	// are used.
	BaseURLFunc func() string
	// Path is the full relay request path, any metro-api base path included.
	Path string
	// TokenFunc resolves the instance JWT per request.
	TokenFunc func() string

	MaxRecords int
	MaxBytes   int
	Linger     time.Duration
	MaxQueue   int

	Client  *http.Client
	Metrics *RelayMetrics
	retry   relayRetry
}

func (c *RelayConfig) withDefaults() {
	if c.MaxRecords <= 0 {
		c.MaxRecords = DefaultRelayMaxRecords
	}
	if c.MaxBytes <= 0 {
		c.MaxBytes = DefaultRelayMaxBytes
	}
	if c.Linger <= 0 {
		c.Linger = DefaultRelayLinger
	}
	if c.MaxQueue <= 0 {
		c.MaxQueue = DefaultRelayMaxQueue
	}
	if c.Client == nil {
		c.Client = &http.Client{
			// The relay is configured, not discovered, so a redirect is never
			// something to follow with the instance credential attached.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	if c.Metrics == nil {
		c.Metrics = &RelayMetrics{}
	}
	if c.retry == (relayRetry{}) {
		c.retry = defaultRelayRetry
	}
}

// relayRecord carries no timestamp. S2 stamps each record when it arrives, as
// it does for the direct sink, so the since/until window of a telemetry read
// means the same thing whichever sink stored the events.
type relayRecord struct {
	Body json.RawMessage `json:"body"`
}

func (r relayRecord) metered() int { return relayMeteredOverhead + len(r.Body) }

// relayStorage implements Storage by posting envelopes in batches to the
// metro-api relay, which appends them to the instance's S2 stream. Append never
// blocks: the queue is bounded and drops its oldest record when full. One
// sender goroutine posts one batch at a time, so records arrive in order.
type relayStorage struct {
	cfg RelayConfig
	log *slog.Logger

	mu      sync.Mutex
	queue   []relayRecord
	closing bool
	wake    chan struct{}

	sendCtx    context.Context
	cancelSend context.CancelFunc
	done       chan struct{}
	closeOnce  sync.Once
}

func newRelayStorage(ctx context.Context, cfg RelayConfig, log *slog.Logger) (*relayStorage, error) {
	if cfg.Path == "" || cfg.BaseURLFunc == nil || cfg.TokenFunc == nil {
		return nil, fmt.Errorf("relay storage: path, base URL, and token are required")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("relay storage: context already cancelled: %w", err)
	}
	cfg.withDefaults()
	// The sender runs independently of ctx so SIGTERM does not cut off the
	// final flush; Close bounds it instead.
	sendCtx, cancel := context.WithCancel(context.Background())
	s := &relayStorage{
		cfg:        cfg,
		log:        log,
		wake:       make(chan struct{}, 1),
		sendCtx:    sendCtx,
		cancelSend: cancel,
		done:       make(chan struct{}),
	}
	go s.run()
	return s, nil
}

// Append marshals env exactly as the direct S2 sink does and queues it.
func (s *relayStorage) Append(ctx context.Context, env Envelope) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("relay storage: marshal envelope seq=%d: %w", env.Seq, err)
	}
	rec := relayRecord{Body: data}

	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		s.cfg.Metrics.addDropped(relayDropShutdown, 1)
		return nil
	}
	if len(s.queue) >= s.cfg.MaxQueue {
		s.queue = s.queue[1:]
		s.cfg.Metrics.addDropped(relayDropQueueFull, 1)
	}
	s.queue = append(s.queue, rec)
	s.cfg.Metrics.queueDepth.Store(int64(len(s.queue)))
	s.mu.Unlock()
	s.signal()
	return nil
}

// Close flushes what it can within ctx, then drops the rest.
func (s *relayStorage) Close(ctx context.Context) error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closing = true
		s.mu.Unlock()
		s.signal()
	})
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
	}
	s.cancelSend()
	select {
	case <-s.done:
	case <-time.After(relayDrainGrace):
	}
	return ctx.Err()
}

func (s *relayStorage) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *relayStorage) run() {
	defer close(s.done)
	defer s.dropRemaining()
	for {
		batch, ok := s.nextBatch()
		if !ok {
			return
		}
		s.send(batch)
		if s.sendCtx.Err() != nil {
			return
		}
	}
}

// nextBatch waits until a batch is due: full by count or bytes, the linger
// since the sender first saw a queued record has passed, or Close was called.
// It returns false once closing with nothing left to send, or when the sender
// is cut off.
func (s *relayStorage) nextBatch() ([]relayRecord, bool) {
	var timer *time.Timer
	var linger <-chan time.Time
	lingered := false
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		s.mu.Lock()
		n, full := s.batchLenLocked()
		if n == 0 && s.closing {
			s.mu.Unlock()
			return nil, false
		}
		if n > 0 && (full || lingered || s.closing) {
			batch := slices.Clone(s.queue[:n])
			s.queue = s.queue[n:]
			s.cfg.Metrics.queueDepth.Store(int64(len(s.queue)))
			s.mu.Unlock()
			return batch, true
		}
		s.mu.Unlock()

		if n > 0 && timer == nil {
			timer = time.NewTimer(s.cfg.Linger)
			linger = timer.C
		}
		select {
		case <-s.wake:
		case <-linger:
			lingered = true
		case <-s.sendCtx.Done():
			return nil, false
		}
	}
}

// batchLenLocked returns how many queued records fit in one batch and whether
// that batch is full. A record over the byte budget on its own still ships
// alone.
func (s *relayStorage) batchLenLocked() (int, bool) {
	size := 0
	for i, rec := range s.queue {
		if i == s.cfg.MaxRecords {
			return i, true
		}
		if i > 0 && size+rec.metered() > s.cfg.MaxBytes {
			return i, true
		}
		size += rec.metered()
	}
	n := len(s.queue)
	return n, n >= s.cfg.MaxRecords || size >= s.cfg.MaxBytes
}

func (s *relayStorage) dropRemaining() {
	s.mu.Lock()
	n := len(s.queue)
	s.queue = nil
	s.closing = true
	s.cfg.Metrics.queueDepth.Store(0)
	s.mu.Unlock()
	if n > 0 {
		s.cfg.Metrics.addDropped(relayDropShutdown, n)
	}
}

// send posts one batch, retrying throttled and unavailable responses. Any
// other outcome drops the batch and counts it.
func (s *relayStorage) send(batch []relayRecord) {
	body, err := json.Marshal(struct {
		Records []relayRecord `json:"records"`
	}{batch})
	if err != nil {
		s.fail(batch, "marshal", relayDropRejected, err)
		return
	}
	ctx, cancel := context.WithTimeout(s.sendCtx, relayExportTimeout)
	defer cancel()

	start := time.Now()
	backoff := s.cfg.retry.initial
	for {
		status, retryAfter, err := s.post(ctx, body)
		if err == nil && status >= 200 && status < 300 {
			s.cfg.Metrics.exported.Add(uint64(len(batch)))
			return
		}
		label := strconv.Itoa(status)
		if err != nil {
			label = "error"
		}
		if s.sendCtx.Err() != nil {
			s.fail(batch, label, relayDropShutdown, err)
			return
		}
		if !relayRetryable(status, err) {
			s.fail(batch, label, relayDropRejected, err)
			return
		}
		delay := max(retryAfter, jitter(backoff))
		if time.Since(start)+delay > s.cfg.retry.maxElapsed {
			s.fail(batch, label, relayDropRetriesExhausted, err)
			return
		}
		s.cfg.Metrics.retries.Add(1)
		s.log.Debug("relay storage: retrying batch", "records", len(batch), "status", label, "delay", delay)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			reason := relayDropRetriesExhausted
			if s.sendCtx.Err() != nil {
				reason = relayDropShutdown
			}
			s.fail(batch, label, reason, ctx.Err())
			return
		}
		backoff = min(time.Duration(float64(backoff)*1.5), s.cfg.retry.max)
	}
}

func (s *relayStorage) fail(batch []relayRecord, status, reason string, err error) {
	s.cfg.Metrics.addFailed(status)
	s.cfg.Metrics.addDropped(reason, len(batch))
	s.log.Warn("relay storage: dropped batch", "records", len(batch), "status", status, "err", err)
}

// post sends one request. The bearer is attached only to the request built for
// the resolved relay URL, and redirects are not followed, so the instance
// credential never reaches another host.
func (s *relayStorage) post(ctx context.Context, body []byte) (status int, retryAfter time.Duration, err error) {
	target, err := relayURL(s.cfg.BaseURLFunc(), s.cfg.Path)
	if err != nil {
		return 0, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	// metro-api closes idle connections when a deploy replaces its process. A
	// batch written to a kept-alive connection at that moment gets no response,
	// and net/http will not resend a POST on its own, so the batch would be
	// dropped on every deploy that catches it. An empty Idempotency-Key marks
	// the request as safe to resend on a fresh connection and is not sent on
	// the wire. The relay does not deduplicate: if it appended the batch and
	// died before answering, the resend stores it twice, which is the same
	// duplicate a retried 503 can already produce.
	req.Header["Idempotency-Key"] = []string{}
	if tok := s.cfg.TokenFunc(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := s.cfg.Client.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if secs, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && secs > 0 {
		retryAfter = time.Duration(secs) * time.Second
	}
	return resp.StatusCode, retryAfter, nil
}

// relayRetryable matches the OTLP exporter: retry 429, 502, 503, and 504, and
// temporary transport errors; everything else is final.
func relayRetryable(status int, err error) bool {
	if err != nil {
		var urlErr *url.Error
		return errors.As(err, &urlErr) && urlErr.Temporary()
	}
	switch status {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

func jitter(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.5 + rand.Float64()))
}

// relayURL is base's scheme and host with path. Provisioning already folds any
// metro-api base path into path, as it does for the OTLP relay path.
func relayURL(base, path string) (string, error) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("relay storage: invalid metro-api base URL")
	}
	return (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: path}).String(), nil
}

// NewRelayStorageController builds a storage controller that writes through
// the metro-api relay. It shares the S2 controller's lifecycle and fork gate:
// streamFn must resolve before storage opens, so a fork never writes while it
// still holds its seed's identity. The relay names the stream from the
// instance JWT, so the resolved value is not sent.
func NewRelayStorageController(es *EventStream, cfg RelayConfig, streamFn func() string, log *slog.Logger) *S2StorageController {
	return &S2StorageController{
		es:       es,
		streamFn: streamFn,
		open: func(ctx context.Context, _ string) (Storage, error) {
			return newRelayStorage(ctx, cfg, log)
		},
		logAttr: []any{"via", "metro-api relay"},
		log:     log,
	}
}
