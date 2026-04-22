package events

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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
		Category: CategoryConsole,
		Source: Source{
			Kind:  KindCDP,
			Event: "Runtime.consoleAPICalled",
			Metadata: map[string]string{
				"target_id":       "target-1",
				"cdp_session_id":  "cdp-session-1",
				"frame_id":        "frame-1",
				"parent_frame_id": "parent-frame-1",
			},
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
		CaptureSessionID: "test-session-id",
		Seq:              1,
		Event: Event{
			Ts:       1000,
			Type:     "console.log",
			Category: CategoryConsole,
			Source:   Source{Kind: KindCDP},
		},
	}

	b, err := json.Marshal(env)
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(b, &decoded))

	assert.Equal(t, "test-session-id", decoded["capture_session_id"])
	assert.Equal(t, float64(1), decoded["seq"])
	inner, ok := decoded["event"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "console.log", inner["type"])
}

func TestEventData(t *testing.T) {
	rawData := json.RawMessage(`{"key":"value","num":42}`)
	ev := Event{
		Ts:       1000,
		Type:     "page.navigation",
		Category: CategoryPage,
		Source:   Source{Kind: KindCDP},
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
		Category: CategoryConsole,
		Source:   Source{Kind: KindCDP},
	}

	b, err := json.Marshal(ev)
	require.NoError(t, err)

	s := string(b)
	assert.NotContains(t, s, `"event"`)
}

func mkEnv(seq uint64, ev Event) Envelope {
	return Envelope{Seq: seq, Event: ev}
}

func cdpEvent(typ string, cat EventCategory) Event {
	return Event{Type: typ, Category: cat, Source: Source{Kind: KindCDP}}
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
		mkEnv(1, cdpEvent("console.log", CategoryConsole)),
		mkEnv(2, cdpEvent("network.request", CategoryNetwork)),
		mkEnv(3, cdpEvent("page.navigation", CategoryPage)),
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
		rb.publish(mkEnv(1, cdpEvent("console.log", CategoryConsole)))
		rb.publish(mkEnv(2, cdpEvent("console.log", CategoryConsole)))
		rb.publish(mkEnv(3, cdpEvent("console.log", CategoryConsole)))
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

	rb.publish(mkEnv(1, cdpEvent("console.log", CategoryConsole)))
	rb.publish(mkEnv(2, cdpEvent("console.log", CategoryConsole)))
	rb.publish(mkEnv(3, cdpEvent("console.log", CategoryConsole)))

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
		rb.publish(mkEnv(i, cdpEvent("console.log", CategoryConsole)))
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
			small.publish(mkEnv(i, cdpEvent("console.log", CategoryConsole)))
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
			rb.publish(mkEnv(uint64(i), cdpEvent("console.log", CategoryConsole)))
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
		rb.publish(mkEnv(uint64(i+1), cdpEvent("console.log", CategoryConsole)))
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

// TestFileWriter: per-category JSONL appender tests.
func TestFileWriter(t *testing.T) {
	t.Run("category_routing", func(t *testing.T) {
		dir := t.TempDir()
		fw, err := newFileWriter(dir)
		require.NoError(t, err)
		defer fw.Close()

		envsToFile := []struct {
			env      Envelope
			file     string
			category string
		}{
			{Envelope{Seq: 1, Event: Event{Type: "console.log", Category: CategoryConsole, Source: Source{Kind: KindCDP}, Ts: 1}}, "console.log", "console"},
			{Envelope{Seq: 2, Event: Event{Type: "network.request", Category: CategoryNetwork, Source: Source{Kind: KindCDP}, Ts: 1}}, "network.log", "network"},
			{Envelope{Seq: 3, Event: Event{Type: "liveview.click", Category: CategoryLiveview, Source: Source{Kind: KindKernelAPI}, Ts: 1}}, "liveview.log", "liveview"},
			{Envelope{Seq: 4, Event: Event{Type: "captcha.solve", Category: CategoryCaptcha, Source: Source{Kind: KindExtension}, Ts: 1}}, "captcha.log", "captcha"},
			{Envelope{Seq: 5, Event: Event{Type: "page.navigation", Category: CategoryPage, Source: Source{Kind: KindCDP}, Ts: 1}}, "page.log", "page"},
			{Envelope{Seq: 6, Event: Event{Type: "input.click", Category: CategoryInteraction, Source: Source{Kind: KindCDP}, Ts: 1}}, "interaction.log", "interaction"},
			{Envelope{Seq: 7, Event: Event{Type: "monitor.connected", Category: CategorySystem, Source: Source{Kind: KindKernelAPI}, Ts: 1}}, "system.log", "system"},
		}

		for _, e := range envsToFile {
			data, err := json.Marshal(e.env)
			require.NoError(t, err)
			require.NoError(t, fw.Write(e.file, data))
		}

		for _, e := range envsToFile {
			data, err := os.ReadFile(filepath.Join(dir, e.file))
			require.NoError(t, err, "missing file %s for type %s", e.file, e.env.Event.Type)

			line := bytes.TrimRight(data, "\n")
			require.True(t, json.Valid(line), "invalid JSON in %s", e.file)

			var decoded map[string]any
			require.NoError(t, json.Unmarshal(line, &decoded))
			inner, ok := decoded["event"].(map[string]any)
			require.True(t, ok)
			assert.Equal(t, e.category, inner["category"], "wrong category in %s", e.file)
			srcMap, ok := inner["source"].(map[string]any)
			require.True(t, ok, "source should be an object in %s", e.file)
			assert.Equal(t, string(e.env.Event.Source.Kind), srcMap["kind"], "wrong source kind in %s", e.file)
		}
	})

	t.Run("empty_filename_rejected", func(t *testing.T) {
		dir := t.TempDir()
		fw, err := newFileWriter(dir)
		require.NoError(t, err)
		defer fw.Close()

		err = fw.Write("", []byte(`{"seq":1}`))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "empty filename")
	})

	t.Run("concurrent_writes", func(t *testing.T) {
		dir := t.TempDir()
		fw, err := newFileWriter(dir)
		require.NoError(t, err)
		defer fw.Close()

		const goroutines = 10
		const eventsPerGoroutine = 100

		var wg sync.WaitGroup
		for i := 0; i < goroutines; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				for j := 0; j < eventsPerGoroutine; j++ {
					env := Envelope{
						Seq:   uint64(i*eventsPerGoroutine + j),
						Event: Event{Type: "console.log", Category: CategoryConsole, Source: Source{Kind: KindCDP}, Ts: 1},
					}
					envData, err := json.Marshal(env)
					require.NoError(t, err)
					require.NoError(t, fw.Write("console.log", envData))
				}
			}(i)
		}
		wg.Wait()

		data, err := os.ReadFile(filepath.Join(dir, "console.log"))
		require.NoError(t, err)

		lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
		assert.Len(t, lines, goroutines*eventsPerGoroutine)
		for _, line := range lines {
			assert.True(t, json.Valid([]byte(line)), "invalid JSON line: %s", line)
		}
	})

	t.Run("lazy_open", func(t *testing.T) {
		dir := t.TempDir()
		fw, err := newFileWriter(dir)
		require.NoError(t, err)
		defer fw.Close()

		entries, err := os.ReadDir(dir)
		require.NoError(t, err)
		assert.Empty(t, entries, "files opened before first Write")

		env := Envelope{Seq: 1, Event: Event{Type: "console.log", Category: CategoryConsole, Source: Source{Kind: KindCDP}, Ts: 1}}
		envData, err := json.Marshal(env)
		require.NoError(t, err)
		require.NoError(t, fw.Write("console.log", envData))

		entries, err = os.ReadDir(dir)
		require.NoError(t, err)
		assert.Len(t, entries, 1, "expected exactly one file after first Write")
		assert.Equal(t, "console.log", entries[0].Name())
	})
}

func TestCaptureSession(t *testing.T) {
	newCaptureSession := func(t *testing.T) (*CaptureSession, string) {
		t.Helper()
		dir := t.TempDir()
		p, err := NewCaptureSession(CaptureSessionConfig{LogDir: dir, RingCapacity: 100})
		require.NoError(t, err)
		p.Start("test-session", CaptureConfig{})
		t.Cleanup(func() { p.Close() })
		return p, dir
	}

	t.Run("concurrent_publish_seq_order", func(t *testing.T) {
		const goroutines = 8
		const eventsEach = 50
		const total = goroutines * eventsEach

		p, err := NewCaptureSession(CaptureSessionConfig{LogDir: t.TempDir(), RingCapacity: total})
		require.NoError(t, err)
		p.Start("test-concurrent", CaptureConfig{})
		t.Cleanup(func() { p.Close() })
		reader := p.NewReader(0)

		var wg sync.WaitGroup
		for i := 0; i < goroutines; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < eventsEach; j++ {
					p.Publish(cdpEvent("console.log", CategoryConsole))
				}
			}()
		}
		wg.Wait()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		for want := uint64(1); want <= total; want++ {
			env := readEnvelope(t, reader, ctx)
			assert.Equal(t, want, env.Seq, "events must arrive in seq order")
		}
	})

	t.Run("publish_increments_seq", func(t *testing.T) {
		p, _ := newCaptureSession(t)
		reader := p.NewReader(0)

		for i := 0; i < 3; i++ {
			p.Publish(Event{Type: "page.navigation", Category: CategoryPage, Source: Source{Kind: KindCDP}, Ts: 1})
		}

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		for want := uint64(1); want <= 3; want++ {
			env := readEnvelope(t, reader, ctx)
			assert.Equal(t, want, env.Seq, "expected seq %d got %d", want, env.Seq)
		}
	})

	t.Run("publish_sets_ts", func(t *testing.T) {
		p, _ := newCaptureSession(t)
		reader := p.NewReader(0)

		before := time.Now().UnixMicro()
		p.Publish(Event{Type: "page.navigation", Category: CategoryPage, Source: Source{Kind: KindCDP}})
		after := time.Now().UnixMicro()

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		env := readEnvelope(t, reader, ctx)
		assert.GreaterOrEqual(t, env.Event.Ts, before)
		assert.LessOrEqual(t, env.Event.Ts, after)
	})

	t.Run("publish_writes_file", func(t *testing.T) {
		p, dir := newCaptureSession(t)

		p.Publish(Event{Type: "console.log", Category: CategoryConsole, Source: Source{Kind: KindCDP}, Ts: 1})

		data, err := os.ReadFile(filepath.Join(dir, "console.log"))
		require.NoError(t, err)

		lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
		require.Len(t, lines, 1)
		assert.True(t, json.Valid([]byte(lines[0])))
		assert.Contains(t, lines[0], `"console.log"`)
	})

	t.Run("publish_writes_ring", func(t *testing.T) {
		p, _ := newCaptureSession(t)

		reader := p.NewReader(0)
		p.Publish(Event{Type: "page.navigation", Category: CategoryPage, Source: Source{Kind: KindCDP}, Ts: 1})

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		env := readEnvelope(t, reader, ctx)
		assert.Equal(t, "page.navigation", env.Event.Type)
		assert.Equal(t, CategoryPage, env.Event.Category)
	})

	t.Run("start_sets_capture_session_id", func(t *testing.T) {
		p, _ := newCaptureSession(t)
		p.Start("test-uuid", CaptureConfig{})

		reader := p.NewReader(0)
		p.Publish(Event{Type: "page.navigation", Category: CategoryPage, Source: Source{Kind: KindCDP}, Ts: 1})

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		env := readEnvelope(t, reader, ctx)
		assert.Equal(t, "test-uuid", env.CaptureSessionID)
	})

	t.Run("truncation_applied", func(t *testing.T) {
		p, dir := newCaptureSession(t)
		reader := p.NewReader(0)

		largeData := strings.Repeat("x", 1_100_000)
		rawData, err := json.Marshal(map[string]string{"payload": largeData})
		require.NoError(t, err)

		p.Publish(Event{
			Type:     "page.navigation",
			Category: CategoryPage,
			Source:   Source{Kind: KindCDP},
			Ts:       1,
			Data:     json.RawMessage(rawData),
		})

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		env := readEnvelope(t, reader, ctx)
		assert.True(t, env.Event.Truncated)
		assert.True(t, json.Valid(env.Event.Data))

		marshaled, err := json.Marshal(env)
		require.NoError(t, err)
		assert.LessOrEqual(t, len(marshaled), maxS2RecordBytes)

		data, err := os.ReadFile(filepath.Join(dir, "page.log"))
		require.NoError(t, err)
		lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
		require.Len(t, lines, 1)
		assert.Contains(t, lines[0], `"truncated":true`)
	})

}

func TestRingBufferResetWithActiveReader(t *testing.T) {
	rb := newTestRingBuffer(t,10)
	reader := rb.newReader(0)

	// Publish some events so the reader advances.
	for i := uint64(1); i <= 5; i++ {
		rb.publish(mkEnv(i, cdpEvent("console.log", CategoryConsole)))
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
	rb.publish(mkEnv(1, cdpEvent("page.navigation", CategoryPage)))
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

