package events

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	oapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// readEnvelope is a test helper that calls Read and asserts a non-drop result.
func readEnvelope(t *testing.T, r *Reader, ctx context.Context) Envelope {
	t.Helper()
	res, err := r.Read(ctx)
	require.NoError(t, err)
	require.NotNil(t, res.Envelope, "expected envelope, got drop")
	return *res.Envelope
}

func TestEventSerialization(t *testing.T) {
	ev := Event{
		Ts:       1234567890000,
		Type:     "console.log",
		Category: Console,
		Source: oapi.BrowserEventSource{
			Kind:  oapi.Cdp,
			Event: func() *string { s := "Runtime.consoleAPICalled"; return &s }(),
			Metadata: func() *map[string]string {
				m := map[string]string{
					"target_id":       "target-1",
					"cdp_session_id":  "cdp-session-1",
					"frame_id":        "frame-1",
					"parent_frame_id": "parent-frame-1",
				}
				return &m
			}(),
		},
		Data: json.RawMessage(`{"message":"hello"}`),
	}

	b, err := json.Marshal(ev)
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(b, &decoded))

	assert.Equal(t, "console.log", decoded["type"])
	assert.Equal(t, "console", decoded["category"])

	src, ok := decoded["source"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "cdp", src["kind"])
	assert.Equal(t, "Runtime.consoleAPICalled", src["event"])
	meta, ok := src["metadata"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "target-1", meta["target_id"])
	assert.Equal(t, "cdp-session-1", meta["cdp_session_id"])
}

func TestEnvelopeSerialization(t *testing.T) {
	env := Envelope{
		Seq: 1,
		Event: Event{
			Ts:       1000,
			Type:     "console.log",
			Category: Console,
			Source:   oapi.BrowserEventSource{Kind: oapi.Cdp},
		},
	}

	b, err := json.Marshal(env)
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(b, &decoded))

	assert.Equal(t, float64(1), decoded["seq"])
	assert.NotContains(t, decoded, "telemetry_session_id")
	inner, ok := decoded["event"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "console.log", inner["type"])
}

func TestEventData(t *testing.T) {
	rawData := json.RawMessage(`{"key":"value","num":42}`)
	ev := Event{
		Ts:       1000,
		Type:     "page.navigation",
		Category: Page,
		Source:   oapi.BrowserEventSource{Kind: oapi.Cdp},
		Data:     rawData,
	}

	b, err := json.Marshal(ev)
	require.NoError(t, err)

	s := string(b)
	assert.Contains(t, s, `"data":{"key":"value","num":42}`)
	assert.NotContains(t, s, `"data":"{`)
}

func TestEventOmitEmpty(t *testing.T) {
	ev := Event{
		Ts:       1000,
		Type:     "console.log",
		Category: Console,
		Source:   oapi.BrowserEventSource{Kind: oapi.Cdp},
	}

	b, err := json.Marshal(ev)
	require.NoError(t, err)

	s := string(b)
	assert.NotContains(t, s, `"event"`)
}

func mkEnv(seq uint64, ev Event) Envelope {
	return Envelope{Seq: seq, Event: ev}
}

func cdpEvent(typ string, cat oapi.TelemetryEventCategory) Event {
	return Event{Type: typ, Category: cat, Source: oapi.BrowserEventSource{Kind: oapi.Cdp}}
}

func newTestRingBuffer(t *testing.T, capacity int) *ringBuffer {
	t.Helper()
	rb, err := newRingBuffer(capacity)
	require.NoError(t, err)
	return rb
}

// TestRingBuffer: publish 3 envelopes; reader reads all 3 in order
func TestRingBuffer(t *testing.T) {
	rb := newTestRingBuffer(t,10)
	reader := rb.newReader(0)

	envelopes := []Envelope{
		mkEnv(1, cdpEvent("console.log", Console)),
		mkEnv(2, cdpEvent("network.request", Network)),
		mkEnv(3, cdpEvent("page.navigation", Page)),
	}

	for _, env := range envelopes {
		rb.publish(env)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	for i, expected := range envelopes {
		got := readEnvelope(t, reader, ctx)
		assert.Equal(t, expected.Event.Type, got.Event.Type, "event %d", i)
		assert.Equal(t, expected.Event.Category, got.Event.Category, "event %d", i)
	}
}

// TestRingBufferOverflowNoBlock: writer never blocks even with no readers
func TestRingBufferOverflowNoBlock(t *testing.T) {
	rb := newTestRingBuffer(t,2)

	done := make(chan struct{})
	go func() {
		rb.publish(mkEnv(1, cdpEvent("console.log", Console)))
		rb.publish(mkEnv(2, cdpEvent("console.log", Console)))
		rb.publish(mkEnv(3, cdpEvent("console.log", Console)))
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Millisecond):
		t.Fatal("Publish blocked with no readers")
	}

	reader := rb.newReader(0)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	res, err := reader.Read(ctx)
	require.NoError(t, err)
	assert.Nil(t, res.Envelope, "expected drop, not envelope")
	assert.True(t, res.Dropped > 0)
}

func TestRingBufferOverflowExistingReader(t *testing.T) {
	rb := newTestRingBuffer(t,2)
	reader := rb.newReader(0)

	rb.publish(mkEnv(1, cdpEvent("console.log", Console)))
	rb.publish(mkEnv(2, cdpEvent("console.log", Console)))
	rb.publish(mkEnv(3, cdpEvent("console.log", Console)))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// First read should be a drop notification
	res, err := reader.Read(ctx)
	require.NoError(t, err)
	assert.Nil(t, res.Envelope)
	assert.Equal(t, uint64(1), res.Dropped)

	// After the drop the reader continues with the surviving envelopes
	second := readEnvelope(t, reader, ctx)
	assert.Equal(t, uint64(2), second.Seq)

	third := readEnvelope(t, reader, ctx)
	assert.Equal(t, uint64(3), third.Seq)
}

func TestNewReaderResume(t *testing.T) {
	rb := newTestRingBuffer(t,10)
	for i := uint64(1); i <= 5; i++ {
		rb.publish(mkEnv(i, cdpEvent("console.log", Console)))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	t.Run("resume_mid_stream", func(t *testing.T) {
		reader := rb.newReader(3)
		env := readEnvelope(t, reader, ctx)
		assert.Equal(t, uint64(4), env.Seq)
	})

	t.Run("resume_at_latest", func(t *testing.T) {
		reader := rb.newReader(5)
		// Nothing to read — should block until ctx cancels
		shortCtx, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
		defer cancel()
		_, err := reader.Read(shortCtx)
		assert.ErrorIs(t, err, context.DeadlineExceeded)
	})

	t.Run("resume_before_oldest_triggers_drop", func(t *testing.T) {
		small := newTestRingBuffer(t, 3)
		for i := uint64(1); i <= 5; i++ {
			small.publish(mkEnv(i, cdpEvent("console.log", Console)))
		}
		// oldest in ring is seq 3, requesting resume after seq 1
		reader := small.newReader(1)
		res, err := reader.Read(ctx)
		require.NoError(t, err)
		assert.Nil(t, res.Envelope)
		assert.Equal(t, uint64(1), res.Dropped)

		env := readEnvelope(t, reader, ctx)
		assert.Equal(t, uint64(3), env.Seq)
	})
}

func TestConcurrentPublishRead(t *testing.T) {
	const numEvents = 20
	rb := newTestRingBuffer(t,32)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	reader := rb.newReader(0)

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < numEvents; i++ {
			_, err := reader.Read(ctx)
			if !assert.NoError(t, err) {
				return
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 1; i <= numEvents; i++ {
			rb.publish(mkEnv(uint64(i), cdpEvent("console.log", Console)))
		}
	}()

	wg.Wait()
}

func TestConcurrentReaders(t *testing.T) {
	rb := newTestRingBuffer(t,20)

	numReaders := 3
	numEvents := 5

	readers := make([]*Reader, numReaders)
	for i := range readers {
		readers[i] = rb.newReader(0)
	}

	for i := 0; i < numEvents; i++ {
		rb.publish(mkEnv(uint64(i+1), cdpEvent("console.log", Console)))
	}

	var wg sync.WaitGroup
	results := make([][]Envelope, numReaders)

	for i, r := range readers {
		wg.Add(1)
		go func(idx int, reader *Reader) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			var envs []Envelope
			for j := 0; j < numEvents; j++ {
				env := readEnvelope(t, reader, ctx)
				envs = append(envs, env)
			}
			results[idx] = envs
		}(i, r)
	}

	wg.Wait()

	for i, envs := range results {
		assert.Len(t, envs, numEvents, "reader %d", i)
		for j, env := range envs {
			assert.Equal(t, uint64(j+1), env.Seq, "reader %d event %d", i, j)
		}
	}
}


func TestRingBufferResetWithActiveReader(t *testing.T) {
	rb := newTestRingBuffer(t,10)
	reader := rb.newReader(0)

	// Publish some events so the reader advances.
	for i := uint64(1); i <= 5; i++ {
		rb.publish(mkEnv(i, cdpEvent("console.log", Console)))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for i := 0; i < 5; i++ {
		readEnvelope(t, reader, ctx)
	}
	// reader.nextSeq is now 6.

	// Reset — reader should wake up and block until new publishes arrive.
	rb.reset()

	shortCtx, shortCancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer shortCancel()
	_, err := reader.Read(shortCtx)
	assert.ErrorIs(t, err, context.DeadlineExceeded, "reader should block after reset")

	// Publish new events; reader should resume from seq 1.
	rb.publish(mkEnv(1, cdpEvent("page.navigation", Page)))
	env := readEnvelope(t, reader, ctx)
	assert.Equal(t, uint64(1), env.Seq)
	assert.Equal(t, "page.navigation", env.Event.Type)
}

func TestNewRingBufferRejectsNonPositiveCapacity(t *testing.T) {
	for _, cap := range []int{0, -1} {
		rb, err := newRingBuffer(cap)
		assert.Nil(t, rb)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "capacity must be > 0")
	}
}

