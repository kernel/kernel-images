package wsproxy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeConn plays back a fixed script of reads and records what was written.
type fakeConn struct {
	mu       sync.Mutex
	reads    [][]byte
	readErr  error
	written  [][]byte
	writeErr error
	// idle, when set, parks Read once the script is exhausted instead of
	// returning EOF, so one side does not end the pump before the other side
	// has worked through its script.
	idle chan struct{}
}

func (c *fakeConn) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.reads) == 0 {
		idle, err := c.idle, c.readErr
		c.mu.Unlock()
		defer c.mu.Lock()
		if idle != nil {
			select {
			case <-idle:
			case <-ctx.Done():
			}
		}
		if err != nil {
			return 0, nil, err
		}
		return 0, nil, io.EOF
	}
	msg := c.reads[0]
	c.reads = c.reads[1:]
	return websocket.MessageText, msg, nil
}

func (c *fakeConn) Write(ctx context.Context, typ websocket.MessageType, p []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.writeErr != nil {
		return c.writeErr
	}
	c.written = append(c.written, append([]byte(nil), p...))
	return nil
}

func (c *fakeConn) Close(websocket.StatusCode, string) error { return nil }

func (c *fakeConn) writes() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([][]byte, len(c.written))
	copy(out, c.written)
	return out
}

func silent() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// An observer must never see a message the other side did not accept, or a
// reader would conclude the browser was told something it never was.
func TestPumpDoesNotObserveMessagesWhoseWriteFailed(t *testing.T) {
	idle := make(chan struct{})
	defer close(idle)
	client := &fakeConn{reads: [][]byte{[]byte("a"), []byte("b")}, idle: idle}
	upstream := &fakeConn{writeErr: errors.New("upstream gone"), idle: idle}

	var observed [][]byte
	var mu sync.Mutex
	observe := func(direction string, mt websocket.MessageType, msg []byte, ts int64) {
		mu.Lock()
		defer mu.Unlock()
		observed = append(observed, msg)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	Pump(ctx, client, upstream, func(PumpExitCause) {}, silent(), nil, observe)

	mu.Lock()
	defer mu.Unlock()
	if len(observed) != 0 {
		t.Fatalf("observed %d messages whose write failed, want 0", len(observed))
	}
}

// Observation runs after the forward, so the bytes an observer sees are the
// bytes the other side got, in the order it got them.
func TestPumpObservesForwardedMessagesInOrder(t *testing.T) {
	idle := make(chan struct{})
	defer close(idle)
	frames := [][]byte{[]byte("one"), []byte("two"), []byte("three")}
	client := &fakeConn{reads: frames}
	upstream := &fakeConn{idle: idle}

	var observed []string
	var mu sync.Mutex
	observe := func(direction string, mt websocket.MessageType, msg []byte, ts int64) {
		if direction != "->" {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		observed = append(observed, string(msg))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	Pump(ctx, client, upstream, func(PumpExitCause) {}, silent(), nil, observe)

	mu.Lock()
	defer mu.Unlock()
	if got := len(observed); got != len(frames) {
		t.Fatalf("observed %d messages, want %d", got, len(frames))
	}
	for i, want := range []string{"one", "two", "three"} {
		if observed[i] != want {
			t.Fatalf("observed[%d] = %q, want %q", i, observed[i], want)
		}
	}
	if got := len(upstream.writes()); got != len(frames) {
		t.Fatalf("forwarded %d messages, want %d", got, len(frames))
	}
}
