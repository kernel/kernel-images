package events

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/s2-streamstore/s2-sdk-go/s2"
)

// S2Config holds batcher tuning parameters for the S2 backend.
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
	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}
	return sp.p.Close()
}

// S2Storage is a Storage backed by S2. All events are appended to a single
// fixed stream whose name is provided at construction time.
type S2Storage struct {
	producer   s2Producer
	shutdownCh chan struct{} // closed when Close is called, bounds ack goroutine contexts
	log        *slog.Logger
}

// NewS2Storage creates an S2Storage that appends to the given stream within basin.
// ctx is used for AppendSession creation and should be the process lifetime context.
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

	batcher := s2.NewBatcher(ctx, &s2.BatchingOptions{
		Linger:     cfg.BatcherLinger,
		MaxRecords: cfg.BatcherMaxRecords,
	})
	producer := s2.NewProducer(ctx, batcher, session)

	return &S2Storage{
		producer:   s2Producer{p: producer},
		shutdownCh: make(chan struct{}),
		log:        log,
	}, nil
}

// Append marshals env to JSON and submits it to the S2 producer.
func (s *S2Storage) Append(_ context.Context, env Envelope) error {
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
		ackCtx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			select {
			case <-s.shutdownCh:
				cancel()
			case <-ackCtx.Done():
			}
		}()

		ticket, err := future.Wait(ackCtx)
		if err != nil {
			s.log.Error("s2storage: wait for submit failed", "seq", env.Seq, "err", err)
			return
		}
		if ticket == nil {
			return
		}
		if _, err := ticket.Ack(ackCtx); err != nil {
			s.log.Error("s2storage: ack failed", "seq", env.Seq, "err", err)
		}
	}()

	return nil
}

// Close cancels in-flight ack goroutines, waits for them to drain, then closes
// the producer (which flushes the S2 batcher to the network).
func (s *S2Storage) Close() error {
	close(s.shutdownCh)
	return s.producer.close(context.Background())
}
