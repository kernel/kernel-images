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

func TestEventSerialization(t *testing.T) {
	ev := Event{
		CaptureSessionID: "test-session-id",
		Seq:              1,
		Ts:               1234567890000,
		Type:             "console.log",
		Category:         CategoryConsole,
		Source:       SourceCDP,
		SourceEvent:      "Runtime.consoleAPICalled",
		DetailLevel:      DetailStandard,
		TargetID:         "target-1",
		CDPSessionID:     "cdp-session-1",
		FrameID:          "frame-1",
		ParentFrameID:    "parent-frame-1",
		URL:              "https://example.com",
		Data:             json.RawMessage(`{"message":"hello"}`),
	}

	b, err := json.Marshal(ev)
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(b, &decoded))

	assert.Equal(t, "console.log", decoded["type"])
	assert.Equal(t, "console", decoded["category"])
	assert.Equal(t, "cdp", decoded["source"])
	assert.Equal(t, "Runtime.consoleAPICalled", decoded["source_event"])
	assert.Equal(t, "standard", decoded["detail_level"])
	assert.Equal(t, "test-session-id", decoded["capture_session_id"])
	assert.Equal(t, float64(1), decoded["seq"])
	assert.Equal(t, "target-1", decoded["target_id"])
	assert.Equal(t, "cdp-session-1", decoded["cdp_session_id"])
	assert.Equal(t, "frame-1", decoded["frame_id"])
	assert.Equal(t, "parent-frame-1", decoded["parent_frame_id"])
	assert.Equal(t, "https://example.com", decoded["url"])
}

func TestEventData(t *testing.T) {
	rawData := json.RawMessage(`{"key":"value","num":42}`)
	ev := Event{
		CaptureSessionID: "test-session",
		Seq:              1,
		Ts:               1000,
		Type:             "page.navigation",
		Category:         CategoryPage,
		Source:       SourceCDP,
		Data:             rawData,
	}

	b, err := json.Marshal(ev)
	require.NoError(t, err)

	s := string(b)
	assert.Contains(t, s, `"data":{"key":"value","num":42}`)
	assert.NotContains(t, s, `"data":"{`)
}

func TestEventOmitEmpty(t *testing.T) {
	ev := Event{
		CaptureSessionID: "sess",
		Seq:              1,
		Ts:               1000,
		Type:             "console.log",
		Category:         CategoryConsole,
		Source:       SourceCDP,
	}

	b, err := json.Marshal(ev)
	require.NoError(t, err)

	s := string(b)
	assert.NotContains(t, s, `"source_event"`)
	assert.Contains(t, s, `"detail_level"`)
}

// TestRingBuffer: publish 3 events; reader reads all 3 in order
func TestRingBuffer(t *testing.T) {
	rb := NewRingBuffer(10)
	reader := rb.NewReader()

	events := []Event{
		{Seq: 1, Type: "console.log", Category: CategoryConsole, Source: SourceCDP},
		{Seq: 2, Type: "network.request", Category: CategoryNetwork, Source: SourceCDP},
		{Seq: 3, Type: "page.navigation", Category: CategoryPage, Source: SourceCDP},
	}

	for _, ev := range events {
		rb.Publish(ev)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	for i, expected := range events {
		got, err := reader.Read(ctx)
		require.NoError(t, err, "reading event %d", i)
		assert.Equal(t, expected.Type, got.Type)
		assert.Equal(t, expected.Category, got.Category)
	}
}

// TestRingBufferOverflowNoBlock: writer never blocks even with no readers
func TestRingBufferOverflowNoBlock(t *testing.T) {
	rb := NewRingBuffer(2)

	done := make(chan struct{})
	go func() {
		rb.Publish(Event{Seq: 1, Type: "console.log", Category: CategoryConsole, Source: SourceCDP})
		rb.Publish(Event{Seq: 2, Type: "console.log", Category: CategoryConsole, Source: SourceCDP})
		rb.Publish(Event{Seq: 3, Type: "console.log", Category: CategoryConsole, Source: SourceCDP})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Millisecond):
		t.Fatal("Publish blocked with no readers")
	}

	reader := rb.NewReader()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	first, err := reader.Read(ctx)
	require.NoError(t, err)
	assert.Equal(t, "events.dropped", first.Type)
	assert.Equal(t, CategorySystem, first.Category)
	assert.Equal(t, SourceKernelAPI, first.Source)
}

func TestRingBufferOverflowExistingReader(t *testing.T) {
	rb := NewRingBuffer(2)
	reader := rb.NewReader()

	rb.Publish(Event{Seq: 1, Type: "console.log", Category: CategoryConsole, Source: SourceCDP})
	rb.Publish(Event{Seq: 2, Type: "console.log", Category: CategoryConsole, Source: SourceCDP})
	rb.Publish(Event{Seq: 3, Type: "console.log", Category: CategoryConsole, Source: SourceCDP})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	first, err := reader.Read(ctx)
	require.NoError(t, err)
	assert.Equal(t, "events.dropped", first.Type)
	assert.Equal(t, CategorySystem, first.Category)

	require.NotNil(t, first.Data)
	assert.True(t, json.Valid(first.Data))
	assert.JSONEq(t, `{"dropped":1}`, string(first.Data))

	// After the drop sentinel the reader continues with the surviving events
	second, err := reader.Read(ctx)
	require.NoError(t, err)
	assert.Equal(t, uint64(2), second.Seq)

	third, err := reader.Read(ctx)
	require.NoError(t, err)
	assert.Equal(t, uint64(3), third.Seq)
}

func TestConcurrentPublishRead(t *testing.T) {
	const numEvents = 20
	rb := NewRingBuffer(32)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	reader := rb.NewReader()

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
			rb.Publish(Event{
				Seq:        uint64(i),
				Type:       "console.log",
				Category:   CategoryConsole,
				Source: SourceCDP,
			})
		}
	}()

	wg.Wait()
}

func TestConcurrentReaders(t *testing.T) {
	rb := NewRingBuffer(20)

	numReaders := 3
	numEvents := 5

	readers := make([]*Reader, numReaders)
	for i := range readers {
		readers[i] = rb.NewReader()
	}

	for i := 0; i < numEvents; i++ {
		rb.Publish(Event{Seq: uint64(i + 1), Type: "console.log", Category: CategoryConsole, Source: SourceCDP})
	}

	var wg sync.WaitGroup
	results := make([][]Event, numReaders)

	for i, r := range readers {
		wg.Add(1)
		go func(idx int, reader *Reader) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			var evs []Event
			for j := 0; j < numEvents; j++ {
				ev, err := reader.Read(ctx)
				if !assert.NoError(t, err) {
					break
				}
				evs = append(evs, ev)
			}
			results[idx] = evs
		}(i, r)
	}

	wg.Wait()

	for i, evs := range results {
		assert.Len(t, evs, numEvents, "reader %d", i)
		for j, ev := range evs {
			assert.Equal(t, uint64(j+1), ev.Seq, "reader %d event %d", i, j)
		}
	}
}

// TestFileWriter: per-category JSONL appender tests.
func TestFileWriter(t *testing.T) {
	t.Run("category_routing", func(t *testing.T) {
		dir := t.TempDir()
		fw := NewFileWriter(dir)
		defer fw.Close()

		eventsToFile := []struct {
			ev       Event
			file     string
			category string
		}{
			{Event{Type: "console.log", Category: CategoryConsole, Source: SourceCDP, Seq: 1, Ts: 1}, "console.log", "console"},
			{Event{Type: "network.request", Category: CategoryNetwork, Source: SourceCDP, Seq: 1, Ts: 1}, "network.log", "network"},
			{Event{Type: "liveview.click", Category: CategoryLiveview, Source: SourceKernelAPI, Seq: 1, Ts: 1}, "liveview.log", "liveview"},
			{Event{Type: "captcha.solve", Category: CategoryCaptcha, Source: SourceExtension, Seq: 1, Ts: 1}, "captcha.log", "captcha"},
			{Event{Type: "page.navigation", Category: CategoryPage, Source: SourceCDP, Seq: 1, Ts: 1}, "page.log", "page"},
			{Event{Type: "input.click", Category: CategoryInteraction, Source: SourceCDP, Seq: 1, Ts: 1}, "interaction.log", "interaction"},
			{Event{Type: "monitor.connected", Category: CategorySystem, Source: SourceKernelAPI, Seq: 1, Ts: 1}, "system.log", "system"},
		}

		for _, e := range eventsToFile {
			data, err := json.Marshal(e.ev)
			require.NoError(t, err)
			require.NoError(t, fw.Write(e.ev, data))
		}

		for _, e := range eventsToFile {
			data, err := os.ReadFile(filepath.Join(dir, e.file))
			require.NoError(t, err, "missing file %s for type %s", e.file, e.ev.Type)

			line := bytes.TrimRight(data, "\n")
			require.True(t, json.Valid(line), "invalid JSON in %s", e.file)

			var decoded map[string]any
			require.NoError(t, json.Unmarshal(line, &decoded))
			assert.Equal(t, e.category, decoded["category"], "wrong category in %s", e.file)
			assert.Equal(t, string(e.ev.Source), decoded["source"], "wrong source in %s", e.file)
		}
	})

	t.Run("empty_category_rejected", func(t *testing.T) {
		dir := t.TempDir()
		fw := NewFileWriter(dir)
		defer fw.Close()

		ev := Event{Type: "mystery", Category: "", Source: SourceCDP, Seq: 1, Ts: 1}
		data, _ := json.Marshal(ev)
		err := fw.Write(ev, data)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "empty category")
	})

	t.Run("concurrent_writes", func(t *testing.T) {
		dir := t.TempDir()
		fw := NewFileWriter(dir)
		defer fw.Close()

		const goroutines = 10
		const eventsPerGoroutine = 100

		var wg sync.WaitGroup
		for i := 0; i < goroutines; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				for j := 0; j < eventsPerGoroutine; j++ {
					ev := Event{
						Seq:        uint64(i*eventsPerGoroutine + j),
						Type:       "console.log",
						Category:   CategoryConsole,
						Source: SourceCDP,
						Ts:         1,
					}
					evData, err := json.Marshal(ev)
					require.NoError(t, err)
					require.NoError(t, fw.Write(ev, evData))
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
		fw := NewFileWriter(dir)
		defer fw.Close()

		entries, err := os.ReadDir(dir)
		require.NoError(t, err)
		assert.Empty(t, entries, "files opened before first Write")

		lazyEv := Event{Type: "console.log", Category: CategoryConsole, Source: SourceCDP, Seq: 1, Ts: 1}
		lazyData, err := json.Marshal(lazyEv)
		require.NoError(t, err)
		require.NoError(t, fw.Write(lazyEv, lazyData))

		entries, err = os.ReadDir(dir)
		require.NoError(t, err)
		assert.Len(t, entries, 1, "expected exactly one file after first Write")
		assert.Equal(t, "console.log", entries[0].Name())
	})
}

func TestPipeline(t *testing.T) {
	newPipeline := func(t *testing.T) (*Pipeline, string) {
		t.Helper()
		dir := t.TempDir()
		rb := NewRingBuffer(100)
		fw := NewFileWriter(dir)
		p := NewPipeline(rb, fw)
		t.Cleanup(func() { p.Close() })
		return p, dir
	}

	t.Run("concurrent_publish_seq_order", func(t *testing.T) {
		const goroutines = 8
		const eventsEach = 50
		const total = goroutines * eventsEach

		// Ring must hold all events so no drop sentinels are emitted.
		rb := NewRingBuffer(total)
		fw := NewFileWriter(t.TempDir())
		p := NewPipeline(rb, fw)
		t.Cleanup(func() { p.Close() })
		reader := p.NewReader()

		var wg sync.WaitGroup
		for i := 0; i < goroutines; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < eventsEach; j++ {
					p.Publish(Event{Type: "console.log", Category: CategoryConsole, Source: SourceCDP, Ts: 1})
				}
			}()
		}
		wg.Wait()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		for want := uint64(1); want <= total; want++ {
			ev, err := reader.Read(ctx)
			require.NoError(t, err)
			assert.Equal(t, want, ev.Seq, "events must arrive in seq order")
		}
	})

	t.Run("publish_increments_seq", func(t *testing.T) {
		p, _ := newPipeline(t)
		reader := p.NewReader()

		for i := 0; i < 3; i++ {
			p.Publish(Event{Type: "page.navigation", Category: CategoryPage, Source: SourceCDP, Ts: 1})
		}

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		for want := uint64(1); want <= 3; want++ {
			ev, err := reader.Read(ctx)
			require.NoError(t, err)
			assert.Equal(t, want, ev.Seq, "expected seq %d got %d", want, ev.Seq)
		}
	})

	t.Run("publish_sets_ts", func(t *testing.T) {
		p, _ := newPipeline(t)
		reader := p.NewReader()

		before := time.Now().UnixMilli()
		p.Publish(Event{Type: "page.navigation", Category: CategoryPage, Source: SourceCDP}) // Ts == 0
		after := time.Now().UnixMilli()

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		ev, err := reader.Read(ctx)
		require.NoError(t, err)
		assert.GreaterOrEqual(t, ev.Ts, before)
		assert.LessOrEqual(t, ev.Ts, after)
	})

	t.Run("publish_writes_file", func(t *testing.T) {
		p, dir := newPipeline(t)

		p.Publish(Event{Type: "console.log", Category: CategoryConsole, Source: SourceCDP, Ts: 1})

		data, err := os.ReadFile(filepath.Join(dir, "console.log"))
		require.NoError(t, err)

		lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
		require.Len(t, lines, 1)
		assert.True(t, json.Valid([]byte(lines[0])))
		assert.Contains(t, lines[0], `"console.log"`)
	})

	t.Run("publish_writes_ring", func(t *testing.T) {
		p, _ := newPipeline(t)

		reader := p.NewReader()
		p.Publish(Event{Type: "page.navigation", Category: CategoryPage, Source: SourceCDP, Ts: 1})

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		ev, err := reader.Read(ctx)
		require.NoError(t, err)
		assert.Equal(t, "page.navigation", ev.Type)
		assert.Equal(t, CategoryPage, ev.Category)
	})

	t.Run("start_sets_capture_session_id", func(t *testing.T) {
		p, _ := newPipeline(t)
		p.Start("test-uuid")

		reader := p.NewReader()
		p.Publish(Event{Type: "page.navigation", Category: CategoryPage, Source: SourceCDP, Ts: 1})

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		ev, err := reader.Read(ctx)
		require.NoError(t, err)
		assert.Equal(t, "test-uuid", ev.CaptureSessionID)
	})

	t.Run("truncation_applied", func(t *testing.T) {
		p, dir := newPipeline(t)
		reader := p.NewReader()

		largeData := strings.Repeat("x", 1_100_000)
		rawData, err := json.Marshal(map[string]string{"payload": largeData})
		require.NoError(t, err)

		p.Publish(Event{
			Type:       "page.navigation",
			Category:   CategoryPage,
			Source: SourceCDP,
			Ts:         1,
			Data:       json.RawMessage(rawData),
		})

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		ev, err := reader.Read(ctx)
		require.NoError(t, err)
		assert.True(t, ev.Truncated)
		assert.True(t, json.Valid(ev.Data))

		marshaled, err := json.Marshal(ev)
		require.NoError(t, err)
		assert.LessOrEqual(t, len(marshaled), maxS2RecordBytes)

		data, err := os.ReadFile(filepath.Join(dir, "page.log"))
		require.NoError(t, err)
		lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
		require.Len(t, lines, 1)
		assert.Contains(t, lines[0], `"truncated":true`)
	})

	t.Run("defaults_detail_level", func(t *testing.T) {
		p, _ := newPipeline(t)
		reader := p.NewReader()

		p.Publish(Event{Type: "console.log", Category: CategoryConsole, Source: SourceCDP, Ts: 1})

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		ev, err := reader.Read(ctx)
		require.NoError(t, err)
		assert.Equal(t, DetailStandard, ev.DetailLevel)

		p.Publish(Event{Type: "console.log", Category: CategoryConsole, Source: SourceCDP, Ts: 1, DetailLevel: DetailVerbose})
		ev2, err := reader.Read(ctx)
		require.NoError(t, err)
		assert.Equal(t, DetailVerbose, ev2.DetailLevel)
	})
}
