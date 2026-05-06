package events

import (
	"context"
	"fmt"
	"sync"
	"time"

	s2 "github.com/s2-streamstore/s2-sdk-go/s2"
)

// s2Producer bundles a Producer with its supporting session.
type s2Producer struct {
	cancel   context.CancelFunc
	ctx      context.Context
	producer *s2.Producer
	session  *s2.AppendSession
}

func (p *s2Producer) close() {
	_ = p.producer.Close()
	_ = p.session.Close()
	p.cancel()
}

// S2Storage is an EventsStorage backed by S2 append-only streams. One
// producer is lazily created per capture session and evicted on session end.
type S2Storage struct {
	basin     *s2.BasinClient
	lingerMs  int
	maxRecs   int
	mu        sync.Mutex
	producers map[string]*s2Producer
}

// NewS2Storage creates an S2Storage for the given basin. Connectivity errors
// are surfaced lazily on the first Append call, so this constructor succeeds
// even with append-only tokens that lack streams:list permission.
func NewS2Storage(basinName, token string, lingerMs, maxRecs int) (*S2Storage, error) {
	client := s2.New(token, nil)
	basin := client.Basin(basinName)

	return &S2Storage{
		basin:     basin,
		lingerMs:  lingerMs,
		maxRecs:   maxRecs,
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
		ctx:      ctx,
		producer: producer,
		session:  session,
	}
	s.producers[streamName] = p
	return p, nil
}

// Append submits data to the named stream and waits for the S2 ack before
// returning. The batcher coalesces records before flushing; Append blocks
// until the batch is confirmed durable so the caller can rely on the nil
// return value as a true durability signal.
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

	pendingAck, err := fut.Wait(ctx)
	if err != nil {
		return fmt.Errorf("s2: ack wait for %q: %w", streamName, err)
	}
	if _, err := pendingAck.Ack(ctx); err != nil {
		return fmt.Errorf("s2: ack for %q: %w", streamName, err)
	}
	return nil
}

// Remove drains and closes the producer for streamName, preventing unbounded
// producer accumulation on long-running servers that cycle through sessions.
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
