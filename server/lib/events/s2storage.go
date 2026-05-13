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

// S2Storage appends all events to a single fixed stream set at construction time.
type S2Storage struct {
	producer       s2Producer
	shutdownCtx    context.Context
	shutdownCancel context.CancelFunc
	closeOnce      sync.Once
	ackErrors      atomic.Uint64
	log            *slog.Logger
}

// ctx controls AppendSession creation; pass context.Background() so the pipeline
// outlives signal cancellation and can be explicitly flushed via Close.
func NewS2Storage(ctx context.Context, basin, accessToken, streamName string, cfg S2Config, log *slog.Logger) (*S2Storage, error) {
	if basin == "" || accessToken == "" || streamName == "" {
		return nil, fmt.Errorf("s2storage: basin, accessToken, and streamName are required")
	}

	client := s2.New(accessToken, nil)
	stream := client.Basin(basin).Stream(s2.StreamName(streamName))

	session, err := stream.AppendSession(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("s2storage: open append session: %w", err)
	}

	if cfg.BatcherLinger == 0 {
		cfg.BatcherLinger = 100 * time.Millisecond
	}
	if cfg.BatcherMaxRecords == 0 {
		cfg.BatcherMaxRecords = 50
	}
	batcher := s2.NewBatcher(ctx, &s2.BatchingOptions{
		Linger:     cfg.BatcherLinger,
		MaxRecords: cfg.BatcherMaxRecords,
	})
	producer := s2.NewProducer(ctx, batcher, session)

	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())
	return &S2Storage{
		producer:       s2Producer{p: producer},
		shutdownCtx:    shutdownCtx,
		shutdownCancel: shutdownCancel,
		log:            log,
	}, nil
}

func (s *S2Storage) Append(ctx context.Context, env Envelope) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("s2storage: marshal envelope seq=%d: %w", env.Seq, err)
	}

	future, err := s.producer.p.Submit(s2.AppendRecord{Body: data})
	if err != nil {
		return fmt.Errorf("s2storage: submit seq=%d: %w", env.Seq, err)
	}

	s.producer.wg.Add(1)
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

// AckErrors returns the total number of async ack failures since construction.
func (s *S2Storage) AckErrors() uint64 {
	return s.ackErrors.Load()
}

// Close cancels in-flight ack goroutines, waits for them to drain, then closes
// the producer (which flushes the S2 batcher to the network).
func (s *S2Storage) Close(ctx context.Context) error {
	s.closeOnce.Do(s.shutdownCancel)
	return s.producer.close(ctx)
}
