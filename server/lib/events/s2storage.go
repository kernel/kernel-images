package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/s2-streamstore/s2-sdk-go/s2"
)

type S2Config struct {
	// BatcherLinger is how long the batcher waits before flushing (default: 100ms).
	BatcherLinger time.Duration
	// BatcherMaxRecords is the max records per batch (default: 50).
	BatcherMaxRecords int
}

type s2Producer struct {
	p  *s2.Producer
	wg sync.WaitGroup
}

func (sp *s2Producer) close(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		sp.wg.Wait()
		close(done)
	}()
	var drainErr error
	select {
	case <-done:
	case <-ctx.Done():
		drainErr = ctx.Err()
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- sp.p.Close() }()
	select {
	case err := <-closeDone:
		return errors.Join(drainErr, err)
	case <-ctx.Done():
		return errors.Join(drainErr, ctx.Err())
	}
}

// s2Storage appends all events to a single fixed stream set at construction time.
type s2Storage struct {
	producer       s2Producer
	sessionCancel  context.CancelFunc
	shutdownCtx    context.Context
	shutdownCancel context.CancelFunc
	closeOnce      sync.Once
	ackErrors      atomic.Uint64
	log            *slog.Logger
}

// newS2Storage opens an AppendSession that runs under an independent context so
// SIGTERM does not kill it before the batcher flushes. Close cancels that
// context after the producer has drained.
func newS2Storage(ctx context.Context, basin, accessToken, streamName string, cfg S2Config, log *slog.Logger) (*s2Storage, error) {
	if basin == "" || accessToken == "" || streamName == "" {
		return nil, fmt.Errorf("s2storage: basin, accessToken, and streamName are required")
	}

	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("s2storage: context already cancelled: %w", err)
	}

	client := s2.New(accessToken, nil)
	stream := client.Basin(basin).Stream(s2.StreamName(streamName))

	// sessionCtx is independent of the signal context so SIGTERM does not kill
	// the session before the batcher has been flushed. Close cancels it after
	// the producer drains.
	sessionCtx, sessionCancel := context.WithCancel(context.Background())
	session, err := stream.AppendSession(sessionCtx, nil)
	if err != nil {
		sessionCancel()
		return nil, fmt.Errorf("s2storage: open append session: %w", err)
	}

	if cfg.BatcherLinger == 0 {
		cfg.BatcherLinger = 100 * time.Millisecond
	}
	if cfg.BatcherMaxRecords == 0 {
		cfg.BatcherMaxRecords = 50
	}
	batcher := s2.NewBatcher(context.Background(), &s2.BatchingOptions{
		Linger:     cfg.BatcherLinger,
		MaxRecords: cfg.BatcherMaxRecords,
	})
	producer := s2.NewProducer(context.Background(), batcher, session)

	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())
	return &s2Storage{
		producer:       s2Producer{p: producer},
		sessionCancel:  sessionCancel,
		shutdownCtx:    shutdownCtx,
		shutdownCancel: shutdownCancel,
		log:            log,
	}, nil
}

func (s *s2Storage) Append(ctx context.Context, env Envelope) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("s2storage: marshal envelope seq=%d: %w", env.Seq, err)
	}

	s.producer.wg.Add(1)
	future, err := s.producer.p.Submit(s2.AppendRecord{Body: data})
	if err != nil {
		s.producer.wg.Done()
		return fmt.Errorf("s2storage: submit seq=%d: %w", env.Seq, err)
	}

	go func() {
		defer s.producer.wg.Done()

		ticket, err := future.Wait(s.shutdownCtx)
		if err != nil {
			total := s.ackErrors.Add(1)
			s.log.Error("s2storage: wait for submit failed", "seq", env.Seq, "err", err, "total_ack_errors", total)
			return
		}
		if ticket == nil {
			return
		}
		if _, err := ticket.Ack(s.shutdownCtx); err != nil {
			total := s.ackErrors.Add(1)
			s.log.Error("s2storage: ack failed", "seq", env.Seq, "err", err, "total_ack_errors", total)
		}
	}()

	return nil
}

// Close cancels in-flight ack goroutines, waits for them to drain, flushes the
// S2 batcher to the network, then tears down the session.
func (s *s2Storage) Close(ctx context.Context) error {
	s.closeOnce.Do(s.shutdownCancel)
	err := s.producer.close(ctx)
	s.sessionCancel()
	return err
}

// S2StorageWriter reads from an EventStream and forwards each event to S2.
// Construct with NewS2StorageWriter, call Start to begin, Stop to drain and shut down.
type S2StorageWriter struct {
	es          *EventStream
	basin       string
	accessToken string
	streamName  string
	cfg         S2Config
	log         *slog.Logger

	mu      sync.Mutex
	started bool
	storage *s2Storage
	writer  *StorageWriter
	done    chan struct{}
}

func NewS2StorageWriter(es *EventStream, basin, accessToken, streamName string, cfg S2Config, log *slog.Logger) *S2StorageWriter {
	return &S2StorageWriter{
		es:          es,
		basin:       basin,
		accessToken: accessToken,
		streamName:  streamName,
		cfg:         cfg,
		log:         log,
	}
}

// Start opens the S2 append session and begins reading from the event stream.
// ctx governs the Run loop — cancel it (e.g. on SIGTERM) to stop reading.
// The session itself outlives ctx and is torn down by Stop after flushing.
func (w *S2StorageWriter) Start(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.started {
		return fmt.Errorf("s2storagewriter: Start called more than once")
	}
	storage, err := newS2Storage(ctx, w.basin, w.accessToken, w.streamName, w.cfg, w.log)
	if err != nil {
		return err
	}
	w.storage = storage
	w.writer = NewStorageWriter(w.es, storage, w.log)
	w.done = make(chan struct{})
	w.started = true
	go func() {
		defer close(w.done)
		if err := w.writer.Run(ctx); err != nil && ctx.Err() == nil {
			w.log.Error("s2 storage writer failed", "err", err)
		}
	}()
	return nil
}

// Stop waits for the Run goroutine to exit, drains any remaining ring events,
// then closes the S2 producer. ctx bounds the total shutdown time.
func (w *S2StorageWriter) Stop(ctx context.Context) error {
	w.mu.Lock()
	if !w.started {
		w.mu.Unlock()
		return nil
	}
	w.mu.Unlock()

	select {
	case <-w.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := w.writer.Drain(ctx); err != nil {
		w.log.Warn("s2 storage writer: drain incomplete", "err", err)
	}
	return w.storage.Close(ctx)
}

// S2StorageController owns the lifecycle of the S2 sink so the writer can be
// opened on demand rather than at boot. The append session binds one stream for
// its lifetime and the writer under it is single-use, so the controller opens at
// most one writer: once a writer has opened, Start is a no-op for the rest of
// the process. Safe for concurrent use.
type S2StorageController struct {
	es       *EventStream
	basin    string
	token    string
	streamFn func() string
	cfg      S2Config
	log      *slog.Logger

	mu          sync.Mutex
	writer      *S2StorageWriter
	cancel      context.CancelFunc
	everStarted bool
}

// NewS2StorageController resolves the stream name through streamFn at Start
// rather than here: an instance holding for a fork identity carries the stream
// of the instance it was forked from until the handoff lands.
func NewS2StorageController(es *EventStream, basin, token string, streamFn func() string, cfg S2Config, log *slog.Logger) *S2StorageController {
	return &S2StorageController{es: es, basin: basin, token: token, streamFn: streamFn, cfg: cfg, log: log}
}

// Start opens the sink, or is a no-op when a writer has already opened or the
// basin, token, or stream is unset. A failed open leaves the controller unstarted
// so a later identity can still open one. parent governs the read loop; the
// controller derives a cancelable child so Stop can halt the loop even when
// parent is still live.
func (c *S2StorageController) Start(parent context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.everStarted {
		return nil
	}
	if c.basin == "" || c.token == "" {
		return nil
	}
	stream := c.streamFn()
	if stream == "" {
		return nil
	}
	c.log.Info("S2 storage enabled", "basin", c.basin, "stream", stream)
	runCtx, cancel := context.WithCancel(parent)
	w := NewS2StorageWriter(c.es, c.basin, c.token, stream, c.cfg, c.log)
	if err := w.Start(runCtx); err != nil {
		cancel()
		return err
	}
	c.writer, c.cancel, c.everStarted = w, cancel, true
	return nil
}

// Stop drains and shuts down a running writer, or is a no-op if none is running.
// ctx bounds shutdown time.
func (c *S2StorageController) Stop(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.writer == nil {
		return nil
	}
	c.cancel()
	err := c.writer.Stop(ctx)
	c.writer, c.cancel = nil, nil
	return err
}

// Running reports whether the sink is currently forwarding events.
func (c *S2StorageController) Running() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writer != nil
}

// EverStarted reports whether a writer ever opened, including one already
// stopped. A caller that must guarantee nothing was persisted needs this rather
// than Running, which goes false again at shutdown.
func (c *S2StorageController) EverStarted() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.everStarted
}
