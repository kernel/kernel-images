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

	// idempotencyAbandonGrace is how long a call keeps running after its last
	// caller disconnects, waiting for a retry to collect it. The SDKs wait at
	// most 8 seconds between retries.
	idempotencyAbandonGrace = 10 * time.Second
)

var (
	errIdempotencyKeyReused    = errors.New("Idempotency-Key was already used for a different request")
	errIdempotentCallsBusy     = errors.New("too many calls with a new Idempotency-Key are waiting or running; retry later")
	errIdempotentCallAbandoned = errors.New("no caller is waiting for this call")
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
	// abandonGrace is how long a call keeps running with no caller waiting
	// for it before it is cancelled.
	abandonGrace time.Duration
	now          func() time.Time

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

	cancel  context.CancelCauseFunc
	waiters int         // guarded by mu
	abandon *time.Timer // guarded by mu; set while no caller waits
}

func newIdempotentCalls[R any](maxFinished, maxPending int) *idempotentCalls[R] {
	return &idempotentCalls[R]{
		maxFinished:  maxFinished,
		maxPending:   maxPending,
		retention:    idempotencyRetention,
		abandonGrace: idempotencyAbandonGrace,
		now:          time.Now,
		calls:        make(map[string]*idempotentCall[R]),
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
// with it, waiting for that call if it is still running. A caller that
// disconnects does not cancel call: it keeps running while any caller waits
// on the key, and for abandonGrace after the last one leaves, so a retry can
// collect the result. Then its context is cancelled, as a caller's
// disconnect would cancel an unkeyed call. keep reports whether a response
// shows the call ran; any other response releases the key. Do returns
// errIdempotencyKeyReused when key was used with a different fingerprint, and
// errIdempotentCallsBusy without running call when too many are pending.
func (c *idempotentCalls[R]) Do(ctx context.Context, key string, fingerprint [sha256.Size]byte, call func(context.Context) (R, error), keep func(R) bool) (R, error) {
	var zero R
	c.mu.Lock()
	c.expireLocked()
	entry, ok := c.calls[key]
	switch {
	case ok && entry.fingerprint != fingerprint:
		c.mu.Unlock()
		return zero, errIdempotencyKeyReused
	case ok:
		c.attachLocked(entry)
	case c.pending >= c.maxPending:
		c.mu.Unlock()
		return zero, errIdempotentCallsBusy
	default:
		var callCtx context.Context
		entry = &idempotentCall[R]{fingerprint: fingerprint, done: make(chan struct{}), waiters: 1}
		callCtx, entry.cancel = context.WithCancelCause(context.WithoutCancel(ctx))
		c.calls[key] = entry
		c.pending++
		go c.run(callCtx, key, entry, call, keep)
	}
	c.mu.Unlock()
	defer c.detach(entry)

	select {
	case <-entry.done:
		return entry.resp, entry.err
	case <-ctx.Done():
		return zero, context.Cause(ctx)
	}
}

// attachLocked records a caller waiting on entry. The caller must hold mu.
func (c *idempotentCalls[R]) attachLocked(entry *idempotentCall[R]) {
	entry.waiters++
	if entry.abandon != nil {
		entry.abandon.Stop()
		entry.abandon = nil
	}
}

// detach records that a caller stopped waiting on entry. When it was the last
// one and the call is still running, the call is cancelled unless another
// caller attaches within abandonGrace.
func (c *idempotentCalls[R]) detach(entry *idempotentCall[R]) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry.waiters--
	select {
	case <-entry.done:
		return
	default:
	}
	if entry.waiters > 0 {
		return
	}
	var timer *time.Timer
	timer = time.AfterFunc(c.abandonGrace, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if entry.abandon == timer {
			entry.cancel(errIdempotentCallAbandoned)
		}
	})
	entry.abandon = timer
}

// run makes the call for entry, then keeps or releases its result.
func (c *idempotentCalls[R]) run(ctx context.Context, key string, entry *idempotentCall[R], call func(context.Context) (R, error), keep func(R) bool) {
	entry.resp, entry.err = runRecovered(ctx, call)
	entry.cancel(nil)
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry.abandon != nil {
		entry.abandon.Stop()
		entry.abandon = nil
	}
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
