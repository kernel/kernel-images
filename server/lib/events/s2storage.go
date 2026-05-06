package events

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	s2 "github.com/s2-streamstore/s2-sdk-go/s2"
)

// s2Producer bundles a Producer with its supporting session and tracks
// in-flight ack goroutines so Close can drain cleanly.
type s2Producer struct {
	cancel   context.CancelFunc
	producer *s2.Producer
	session  *s2.AppendSession
	wg       sync.WaitGroup
}

func (p *s2Producer) close() {
	_ = p.producer.Close()
	p.wg.Wait()
	_ = p.session.Close()
	p.cancel()
}

// S2Storage is an EventsStorage backed by S2 append-only streams. One
// producer is lazily created per capture session and evicted on session end.
type S2Storage struct {
	basin    *s2.BasinClient
	lingerMs int
	maxRecs  int
	mu       sync.Mutex
	producers map[string]*s2Producer
}

// NewS2Storage creates an S2Storage and performs a startup connectivity probe
// against the given basin. Returns an error if the basin/token is invalid or
// unreachable.
func NewS2Storage(ctx context.Context, basinName, token string, lingerMs, maxRecs int) (*S2Storage, error) {
	client := s2.New(token, nil)
	basin := client.Basin(basinName)

	limit := 1
	if _, err := basin.Streams.List(ctx, &s2.ListStreamsArgs{Limit: &limit}); err != nil {
		return nil, fmt.Errorf("s2 connectivity probe failed for basin %q: %w", basinName, err)
	}

	return &S2Storage{
		basin:    basin,
		lingerMs: lingerMs,
		maxRecs:  maxRecs,
		producers: make(map[string]*s2Producer),
	}, nil
}

// getOrCreate returns the existing producer for streamName or lazily creates
// one. Must be called with s.mu held.
func (s *S2Storage) getOrCreate(streamName string) (*s2Producer, error) {
	if p, ok := s.producers[streamName]; ok {
		return p, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	stream := s.basin.Stream(s2.StreamName(streamName))

	session, err := stream.AppendSession(ctx, nil)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("s2: open append session for %q: %w", streamName, err)
	}

	batcher := s2.NewBatcher(ctx, &s2.BatchingOptions{
		Linger:     time.Duration(s.lingerMs) * time.Millisecond,
		MaxRecords: s.maxRecs,
	})

	producer := s2.NewProducer(ctx, batcher, session)
	p := &s2Producer{
		cancel:   cancel,
		producer: producer,
		session:  session,
	}
	s.producers[streamName] = p
	return p, nil
}

// Append submits data to the named stream. The S2 batcher coalesces records
// before flushing to the network; acks are handled asynchronously.
func (s *S2Storage) Append(ctx context.Context, streamName string, data []byte) error {
	s.mu.Lock()
	p, err := s.getOrCreate(streamName)
	s.mu.Unlock()
	if err != nil {
		return err
	}

	fut, err := p.producer.Submit(s2.AppendRecord{Body: data})
	if err != nil {
		return fmt.Errorf("s2: submit to %q: %w", streamName, err)
	}

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		pendingAck, err := fut.Wait(context.Background())
		if err != nil {
			slog.Warn("s2: ack wait failed", "stream", streamName, "err", err)
			return
		}
		if _, err := pendingAck.Ack(context.Background()); err != nil {
			slog.Warn("s2: ack failed", "stream", streamName, "err", err)
		}
	}()

	return nil
}

// Remove drains and closes the producer for streamName, preventing unbounded
// producer accumulation on long-running servers that cycle through sessions.
// Called from the DELETE /events/capture_session handler after session teardown.
func (s *S2Storage) Remove(streamName string) {
	s.mu.Lock()
	p, ok := s.producers[streamName]
	if ok {
		delete(s.producers, streamName)
	}
	s.mu.Unlock()
	if ok {
		p.close()
	}
}

// Close drains all in-flight producers and releases resources.
func (s *S2Storage) Close() error {
	s.mu.Lock()
	producers := s.producers
	s.producers = make(map[string]*s2Producer)
	s.mu.Unlock()
	for _, p := range producers {
		p.close()
	}
	return nil
}
