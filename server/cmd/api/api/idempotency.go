package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	maxIdempotencyKeyLength = 255

	// idempotencyRetention bounds how long a finished call's result is kept
	// for retries. Clients retry within seconds.
	idempotencyRetention = 10 * time.Minute
)

var (
	errIdempotencyKeyReused = errors.New("Idempotency-Key was already used for a different request")
	errIdempotentCallsBusy  = errors.New("too many calls with a new Idempotency-Key are waiting or running; retry later")
)

// idempotentCalls runs each Idempotency-Key at most once. A call with a key
// already seen gets the first call's response instead of running again, so a
// client can retry after losing a response without repeating side effects.
//
// Only responses the caller marks as kept are remembered. Every other outcome
// means the call did not run, so the key is released and a retry runs fresh.
type idempotentCalls[R any] struct {
	// maxFinished caps the remembered results, oldest dropped first. Calls
	// still running are never dropped.
	maxFinished int
	// maxPending caps calls waiting or running. They outlive their callers,
	// so nothing else bounds them.
	maxPending int
	retention  time.Duration
	now        func() time.Time

	mu       sync.Mutex
	calls    map[string]*idempotentCall[R]
	finished []string // keys of kept results, oldest first
	pending  int
}

type idempotentCall[R any] struct {
	fingerprint [sha256.Size]byte
	done        chan struct{}
	finishedAt  time.Time
	resp        R
	err         error
}

func newIdempotentCalls[R any](maxFinished, maxPending int) *idempotentCalls[R] {
	return &idempotentCalls[R]{
		maxFinished: maxFinished,
		maxPending:  maxPending,
		retention:   idempotencyRetention,
		now:         time.Now,
		calls:       make(map[string]*idempotentCall[R]),
	}
}

func validateIdempotencyKey(key string) error {
	if key == "" || len(key) > maxIdempotencyKeyLength {
		return fmt.Errorf("Idempotency-Key must be 1 to %d characters", maxIdempotencyKeyLength)
	}
	for i := 0; i < len(key); i++ {
		if key[i] < 0x20 || key[i] > 0x7e {
			return errors.New("Idempotency-Key must be printable ASCII")
		}
	}
	return nil
}

// idempotencyFingerprint identifies a request's parameters, so a key reused
// for a different request can be refused.
func idempotencyFingerprint(request any) [sha256.Size]byte {
	b, err := json.Marshal(request)
	if err != nil {
		panic(fmt.Sprintf("idempotency fingerprint: %v", err))
	}
	return sha256.Sum256(b)
}

// Do runs call under key, or returns the response of the call already made
// with it, waiting for that call if it is still running. call runs on a
// context detached from ctx's cancellation, so the result is there for a
// retry even if this caller disconnects. keep reports whether a response
// shows the call ran; any other response releases the key. Do returns
// errIdempotencyKeyReused when key was used with a different fingerprint, and
// errIdempotentCallsBusy without running call when too many are pending.
func (c *idempotentCalls[R]) Do(ctx context.Context, key string, fingerprint [sha256.Size]byte, call func(context.Context) (R, error), keep func(R) bool) (R, error) {
	c.mu.Lock()
	c.expireLocked()
	if existing, ok := c.calls[key]; ok {
		c.mu.Unlock()
		var zero R
		if existing.fingerprint != fingerprint {
			return zero, errIdempotencyKeyReused
		}
		select {
		case <-existing.done:
			return existing.resp, existing.err
		case <-ctx.Done():
			return zero, context.Cause(ctx)
		}
	}
	if c.pending >= c.maxPending {
		c.mu.Unlock()
		var zero R
		return zero, errIdempotentCallsBusy
	}
	entry := &idempotentCall[R]{fingerprint: fingerprint, done: make(chan struct{})}
	c.calls[key] = entry
	c.pending++
	c.mu.Unlock()

	go func() {
		entry.resp, entry.err = runRecovered(context.WithoutCancel(ctx), call)
		c.mu.Lock()
		defer c.mu.Unlock()
		c.pending--
		if entry.err == nil && keep(entry.resp) {
			entry.finishedAt = c.now()
			c.finished = append(c.finished, key)
			for len(c.finished) > c.maxFinished {
				delete(c.calls, c.finished[0])
				c.finished = c.finished[1:]
			}
		} else {
			delete(c.calls, key)
		}
		close(entry.done)
	}()

	select {
	case <-entry.done:
		return entry.resp, entry.err
	case <-ctx.Done():
		var zero R
		return zero, context.Cause(ctx)
	}
}

// runRecovered turns a panic in call into an error. call runs off the request
// goroutine, outside the router's panic recovery.
func runRecovered[R any](ctx context.Context, call func(context.Context) (R, error)) (resp R, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
		}
	}()
	return call(ctx)
}

// expireLocked drops kept results older than the retention period. The caller
// must hold mu.
func (c *idempotentCalls[R]) expireLocked() {
	cutoff := c.now().Add(-c.retention)
	for len(c.finished) > 0 && c.calls[c.finished[0]].finishedAt.Before(cutoff) {
		delete(c.calls, c.finished[0])
		c.finished = c.finished[1:]
	}
}
