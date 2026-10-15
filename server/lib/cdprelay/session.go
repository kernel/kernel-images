// Package cdprelay retains a logical CDP connection while its host transport is parked.
package cdprelay

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"
)

var (
	errConflict    = errors.New("relay state conflict")
	errBusy        = errors.New("relay has outstanding commands or events")
	errGone        = errors.New("relay session ended")
	errUnavailable = errors.New("CDP upstream unavailable")
)

type frame struct {
	Seq  uint64          `json:"seq"`
	Data json.RawMessage `json:"data"`
}

type request struct {
	Epoch uint64          `json:"epoch"`
	Seq   uint64          `json:"seq"`
	Ack   uint64          `json:"ack"`
	Data  json.RawMessage `json:"data,omitempty"`
}

type state struct {
	Epoch uint64 `json:"epoch"`
	Seq   uint64 `json:"seq"`
}

type session struct {
	mu                           sync.Mutex
	writeMu                      sync.Mutex
	conn                         *websocket.Conn
	ctx                          context.Context
	cancel                       context.CancelFunc
	epoch, sent, received, acked uint64
	lastHash                     [32]byte
	parked, closed               bool
	nested                       bool
	pending                      map[string]struct{}
	queue                        []frame
	bytes, limit                 int
	lastSeen                     time.Time
	notify                       chan struct{}
}

func commandKey(data []byte) string {
	var m struct {
		ID      *int64 `json:"id"`
		Session string `json:"sessionId"`
	}
	if json.Unmarshal(data, &m) != nil || m.ID == nil {
		return ""
	}
	return m.Session + ":" + strconv.FormatInt(*m.ID, 10)
}

func (s *session) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeLocked()
}

func (s *session) closeLocked() {
	if s.closed {
		return
	}
	s.closed = true
	s.queue = nil
	s.bytes = 0
	s.cancel()
	s.conn.CloseNow()
}

func (s *session) read() {
	for {
		kind, data, err := s.conn.Read(s.ctx)
		s.mu.Lock()
		if s.closed || err != nil || kind != websocket.MessageText || s.bytes+len(data) > s.limit || len(s.queue) >= 4096 {
			s.closeLocked()
			s.mu.Unlock()
			return
		}
		delete(s.pending, commandKey(data))
		s.received++
		s.queue = append(s.queue, frame{Seq: s.received, Data: data})
		s.bytes += len(data)
		s.mu.Unlock()
		select {
		case s.notify <- struct{}{}:
		default:
		}
	}
}

func (s *session) send(r request) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	hash := sha256.Sum256(r.Data)
	key := commandKey(r.Data)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errGone
	}
	if s.parked || r.Epoch != s.epoch {
		s.mu.Unlock()
		return errConflict
	}
	if r.Seq == s.sent && r.Seq > 0 {
		s.mu.Unlock()
		if hash != s.lastHash {
			return errConflict
		}
		return nil
	}
	if r.Seq != s.sent+1 || key == "" {
		s.mu.Unlock()
		return errConflict
	}
	if _, exists := s.pending[key]; exists {
		s.mu.Unlock()
		return errConflict
	}
	if len(s.pending) >= 1024 {
		s.mu.Unlock()
		return errBusy
	}
	var command struct {
		Method string `json:"method"`
	}
	if json.Unmarshal(r.Data, &command) != nil || command.Method == "" {
		s.mu.Unlock()
		return errConflict
	}
	// The outer acknowledgment does not complete a nested CDP command.
	if command.Method == "Target.sendMessageToTarget" {
		s.nested = true
	}
	s.pending[key] = struct{}{}
	s.mu.Unlock()
	// Cancellation of a host HTTP request must not tear down Chrome or repeat a command.
	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	defer cancel()
	err := s.conn.Write(ctx, websocket.MessageText, r.Data)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil || s.closed {
		s.closeLocked()
		return errGone
	}
	s.sent, s.lastHash = r.Seq, hash
	return nil
}

func (s *session) ackLocked(ack uint64) error {
	if ack < s.acked || ack > s.received {
		return errConflict
	}
	for len(s.queue) > 0 && s.queue[0].Seq <= ack {
		s.bytes -= len(s.queue[0].Data)
		s.queue[0] = frame{}
		s.queue = s.queue[1:]
	}
	s.acked = ack
	return nil
}

func (s *session) events(ctx context.Context, epoch, ack uint64, wait time.Duration) ([]frame, error) {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return nil, errGone
		}
		if epoch != s.epoch || s.parked {
			s.mu.Unlock()
			return nil, errConflict
		}
		if err := s.ackLocked(ack); err != nil {
			s.mu.Unlock()
			return nil, err
		}
		batch := append(make([]frame, 0, len(s.queue)), s.queue...)
		s.mu.Unlock()
		if len(batch) > 0 {
			return batch, nil
		}
		select {
		case <-s.notify:
		case <-timer.C:
			return batch, nil
		case <-s.ctx.Done():
			return nil, errGone
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (s *session) transition(r request, park bool) (state, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	current := state{Epoch: s.epoch, Seq: s.sent}
	if s.closed {
		return current, errGone
	}
	// Retrying a lost transition response is safe; an older epoch cannot mutate a resumed session.
	if s.parked == park && s.epoch == r.Epoch+1 && s.sent == r.Seq {
		return current, nil
	}
	if s.epoch != r.Epoch || s.sent != r.Seq || s.parked == park {
		return current, errConflict
	}
	if err := s.ackLocked(r.Ack); err != nil {
		return current, err
	}
	if park && (s.nested || len(s.pending) != 0 || len(s.queue) != 0) {
		return current, errBusy
	}
	s.parked = park
	s.epoch++
	return state{Epoch: s.epoch, Seq: s.sent}, nil
}

func (s *session) touch() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.lastSeen = time.Now()
	return true
}

func (s *session) snapshot() (state, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return state{}, errGone
	}
	return state{Epoch: s.epoch, Seq: s.sent}, nil
}

func parseUint(value string) (uint64, error) {
	n, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid sequence")
	}
	return n, nil
}
