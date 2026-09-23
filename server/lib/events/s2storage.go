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

// S2StorageWriter reads from an EventStream and forwards each event after
// afterSeq to S2. Construct with NewS2StorageWriter, call Start to begin, Stop
// to drain and shut down.
type S2StorageWriter struct {
	es          *EventStream
	basin       string
	accessToken string
	streamName  string
	afterSeq    uint64
	cfg         S2Config
	log         *slog.Logger

	mu      sync.Mutex
	started bool
	storage *s2Storage
	writer  *StorageWriter
	done    chan struct{}
}

func NewS2StorageWriter(es *EventStream, basin, accessToken, streamName string, afterSeq uint64, cfg S2Config, log *slog.Logger) *S2StorageWriter {
	return &S2StorageWriter{
		es:          es,
		basin:       basin,
		accessToken: accessToken,
		streamName:  streamName,
		afterSeq:    afterSeq,
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
	w.writer = NewStorageWriterAfter(w.es, storage, w.log, w.afterSeq)
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

// S2StorageController opens at most one writer because StorageWriter is
// single-use and an append session is bound to one stream.
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
	start       *s2ControllerStart
	stopDone    chan struct{}
	everStarted bool
}

type s2ControllerStart struct {
	done chan struct{}
	err  error
}

// NewS2StorageController resolves streamFn at Start because a fork learns its
// stream after construction.
func NewS2StorageController(es *EventStream, basin, token string, streamFn func() string, cfg S2Config, log *slog.Logger) *S2StorageController {
	return &S2StorageController{es: es, basin: basin, token: token, streamFn: streamFn, cfg: cfg, log: log}
}

// Start opens the writer, which stores only events published after afterSeq:
// the ring still holds whatever was captured before storage was wanted, and
// that must not be persisted. afterSeq applies to the call that opens the
// writer; once one has, later calls are no-ops.
func (c *S2StorageController) Start(parent context.Context, afterSeq uint64) (err error) {
	c.mu.Lock()
	if c.everStarted {
		c.mu.Unlock()
		return nil
	}
	if c.start != nil {
		start := c.start
		c.mu.Unlock()
		if err := waitForS2ControllerOperation(parent, start.done); err != nil {
			return err
		}
		return start.err
	}
	if c.basin == "" || c.token == "" {
		c.mu.Unlock()
		return nil
	}
	start := &s2ControllerStart{done: make(chan struct{})}
	c.start = start
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		start.err = err
		c.start = nil
		close(start.done)
		c.mu.Unlock()
	}()

	stream := c.streamFn()
	if stream == "" {
		return nil
	}
	runCtx, cancel := context.WithCancel(parent)
	w := NewS2StorageWriter(c.es, c.basin, c.token, stream, afterSeq, c.cfg, c.log)
	if err = w.Start(runCtx); err != nil {
		cancel()
		return err
	}
	c.log.Info("S2 storage enabled", "basin", c.basin, "stream", stream)
	c.mu.Lock()
	c.writer, c.cancel, c.everStarted = w, cancel, true
	c.mu.Unlock()
	return nil
}

func (c *S2StorageController) Stop(ctx context.Context) error {
	for {
		c.mu.Lock()
		if c.start != nil {
			start := c.start
			c.mu.Unlock()
			if err := waitForS2ControllerOperation(ctx, start.done); err != nil {
				return err
			}
			continue
		}
		if c.stopDone != nil {
			stopDone := c.stopDone
			c.mu.Unlock()
			if err := waitForS2ControllerOperation(ctx, stopDone); err != nil {
				return err
			}
			continue
		}
		if c.writer == nil {
			c.mu.Unlock()
			return nil
		}
		writer, cancel := c.writer, c.cancel
		stopDone := make(chan struct{})
		c.stopDone = stopDone
		c.mu.Unlock()

		cancel()
		err := writer.Stop(ctx)

		c.mu.Lock()
		if err == nil {
			c.writer, c.cancel = nil, nil
		}
		c.stopDone = nil
		close(stopDone)
		c.mu.Unlock()
		return err
	}
}

func waitForS2ControllerOperation(ctx context.Context, done <-chan struct{}) error {
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *S2StorageController) Running() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writer != nil
}

// EverStarted remains true after Stop so callers can tell whether anything
// could have been persisted.
func (c *S2StorageController) EverStarted() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.everStarted
}
