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

func (sp *s2Producer) close() error {
	sp.wg.Wait()
	return sp.p.Close()
}

// S2Storage is a Storage backed by S2. All events are appended to a single
// fixed stream whose name is provided at construction time.
type S2Storage struct {
	stream   *s2.StreamClient
	producer *s2Producer
}

// NewS2Storage creates an S2Storage that appends to the given stream within basin.
// ctx is used for AppendSession creation and should be the process lifetime context.
func NewS2Storage(ctx context.Context, basin, accessToken, streamName string, cfg S2Config) (*S2Storage, error) {
	if basin == "" || accessToken == "" || streamName == "" {
		return nil, fmt.Errorf("s2storage: basin, accessToken, and streamName are required")
	}

	linger := cfg.BatcherLinger
	if linger <= 0 {
		linger = 100 * time.Millisecond
	}
	maxRecs := cfg.BatcherMaxRecords
	if maxRecs <= 0 {
		maxRecs = 50
	}

	client := s2.New(accessToken, nil)
	stream := client.Basin(basin).Stream(s2.StreamName(streamName))

	session, err := stream.AppendSession(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("s2storage: open append session: %w", err)
	}

	batcher := s2.NewBatcher(ctx, &s2.BatchingOptions{
		Linger:     linger,
		MaxRecords: maxRecs,
	})
	producer := s2.NewProducer(ctx, batcher, session)

	return &S2Storage{
		stream:   stream,
		producer: &s2Producer{p: producer},
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
		// Use a fresh background context so the ack isn't tied to any request
		// context, but the wg ensures Close() waits before the process exits.
		ticket, err := future.Wait(context.Background())
		if err != nil {
			slog.Error("s2storage: wait for submit failed", "seq", env.Seq, "err", err)
			return
		}
		if ticket == nil {
			return
		}
		if _, err := ticket.Ack(context.Background()); err != nil {
			slog.Error("s2storage: ack failed", "seq", env.Seq, "err", err)
		}
	}()

	return nil
}

// Close drains in-flight ack goroutines then closes the producer, which flushes
// the S2 batcher to the network.
func (s *S2Storage) Close() error {
	return s.producer.close()
}
