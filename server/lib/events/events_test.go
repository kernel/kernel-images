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

// TestBrowserEventSerialization: round-trip marshal/unmarshal verifying all SCHEMA-01
// envelope fields serialize with correct JSON keys and values, including provenance.
func TestBrowserEventSerialization(t *testing.T) {
	ev := BrowserEvent{
		CaptureSessionID: "test-session-id",
		Seq:              1,
		Ts:               1234567890000,
		Type:             "console.log",
		Category:         CategoryConsole,
		SourceKind:       SourceCDP,
		SourceEvent:      "Runtime.consoleAPICalled",
		DetailLevel:      DetailDefault,
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
	assert.Equal(t, "cdp", decoded["source_kind"])
	assert.Equal(t, "Runtime.consoleAPICalled", decoded["source_event"])
	assert.Equal(t, "default", decoded["detail_level"])
	assert.Equal(t, "test-session-id", decoded["capture_session_id"])
	assert.Equal(t, float64(1), decoded["seq"])
	assert.Equal(t, "target-1", decoded["target_id"])
	assert.Equal(t, "cdp-session-1", decoded["cdp_session_id"])
	assert.Equal(t, "frame-1", decoded["frame_id"])
	assert.Equal(t, "parent-frame-1", decoded["parent_frame_id"])
	assert.Equal(t, "https://example.com", decoded["url"])
}

// TestBrowserEventData: embed a pre-serialized JSON object in Data field; marshal outer event;
// assert Data appears verbatim (no double-encoding).
func TestBrowserEventData(t *testing.T) {
	rawData := json.RawMessage(`{"key":"value","num":42}`)
	ev := BrowserEvent{
		CaptureSessionID: "test-session",
		Seq:              1,
		Ts:               1000,
		Type:             "page.navigation",
		Category:         CategoryPage,
		SourceKind:       SourceCDP,
		Data:             rawData,
	}

	b, err := json.Marshal(ev)
	require.NoError(t, err)

	s := string(b)
	assert.Contains(t, s, `"data":{"key":"value","num":42}`)
	assert.NotContains(t, s, `"data":"{`) // would indicate double-encoding
}

// TestBrowserEventOmitEmpty: source_event is omitted when empty; detail_level always present.
func TestBrowserEventOmitEmpty(t *testing.T) {
	ev := BrowserEvent{
		CaptureSessionID: "sess",
		Seq:              1,
		Ts:               1000,
		Type:             "console.log",
		Category:         CategoryConsole,
		SourceKind:       SourceCDP,
	}

	b, err := json.Marshal(ev)
	require.NoError(t, err)

	s := string(b)
	assert.NotContains(t, s, `"source_event"`)
	// detail_level is always serialized (not omitempty) — zero value is ""
	assert.Contains(t, s, `"detail_level"`)
}

// TestRingBuffer: publish 3 events; reader reads all 3 in order.
func TestRingBuffer(t *testing.T) {
	rb := NewRingBuffer(10)
	reader := rb.NewReader()

	events := []BrowserEvent{
		{Seq: 1, Type: "console.log", Category: CategoryConsole, SourceKind: SourceCDP},
		{Seq: 2, Type: "network.request", Category: CategoryNetwork, SourceKind: SourceCDP},
		{Seq: 3, Type: "page.navigation", Category: CategoryPage, SourceKind: SourceCDP},
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

// TestRingBufferOverflowNoBlock: writer never blocks even with no readers;
// late-joining reader gets events.dropped with correct envelope fields.
func TestRingBufferOverflowNoBlock(t *testing.T) {
	rb := NewRingBuffer(2)

	done := make(chan struct{})
	go func() {
		rb.Publish(BrowserEvent{Seq: 1, Type: "console.log", Category: CategoryConsole, SourceKind: SourceCDP})
		rb.Publish(BrowserEvent{Seq: 2, Type: "console.log", Category: CategoryConsole, SourceKind: SourceCDP})
		rb.Publish(BrowserEvent{Seq: 3, Type: "console.log", Category: CategoryConsole, SourceKind: SourceCDP})
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
	assert.Equal(t, SourceKernelAPI, first.SourceKind)
}

// TestRingBufferOverflowExistingReader: reader created before overflow
// gets events.dropped with exact count, then continues reading.
func TestRingBufferOverflowExistingReader(t *testing.T) {
	rb := NewRingBuffer(2)
	reader := rb.NewReader()

	rb.Publish(BrowserEvent{Seq: 1, Type: "console.log", Category: CategoryConsole, SourceKind: SourceCDP})
	rb.Publish(BrowserEvent{Seq: 2, Type: "console.log", Category: CategoryConsole, SourceKind: SourceCDP})
	rb.Publish(BrowserEvent{Seq: 3, Type: "console.log", Category: CategoryConsole, SourceKind: SourceCDP})

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
	// (seq 2 and 3, which fit in the capacity-2 buffer).
	second, err := reader.Read(ctx)
	require.NoError(t, err)
	assert.Equal(t, uint64(2), second.Seq)

	third, err := reader.Read(ctx)
	require.NoError(t, err)
	assert.Equal(t, uint64(3), third.Seq)
}

// TestConcurrentPublishRead: readers blocked on Read while a writer publishes
// concurrently — exercises locking and notify paths under go test -race.
func TestConcurrentPublishRead(t *testing.T) {
	const numEvents = 20
	rb := NewRingBuffer(32)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	reader := rb.NewReader()

	var wg sync.WaitGroup

	// Reader goroutine: reads numEvents events.
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

	// Writer goroutine: publishes numEvents events.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 1; i <= numEvents; i++ {
			rb.Publish(BrowserEvent{
				Seq:        uint64(i),
				Type:       "console.log",
				Category:   CategoryConsole,
				SourceKind: SourceCDP,
			})
		}
	}()

	wg.Wait()
}

// TestConcurrentReaders: 3 readers subscribe before publish; publish 5 events;
// each reader independently reads all 5; no reader affects another.
func TestConcurrentReaders(t *testing.T) {
	rb := NewRingBuffer(20)

	numReaders := 3
	numEvents := 5

	readers := make([]*Reader, numReaders)
	for i := range readers {
		readers[i] = rb.NewReader()
	}

	for i := 0; i < numEvents; i++ {
		rb.Publish(BrowserEvent{Seq: uint64(i + 1), Type: "console.log", Category: CategoryConsole, SourceKind: SourceCDP})
	}

	var wg sync.WaitGroup
	results := make([][]BrowserEvent, numReaders)

	for i, r := range readers {
		wg.Add(1)
		go func(idx int, reader *Reader) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			var evs []BrowserEvent
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
			ev       BrowserEvent
			file     string
			category string
		}{
			{BrowserEvent{Type: "console.log", Category: CategoryConsole, SourceKind: SourceCDP, Seq: 1, Ts: 1}, "console.log", "console"},
			{BrowserEvent{Type: "network.request", Category: CategoryNetwork, SourceKind: SourceCDP, Seq: 1, Ts: 1}, "network.log", "network"},
			{BrowserEvent{Type: "liveview.click", Category: CategoryLiveview, SourceKind: SourceKernelAPI, Seq: 1, Ts: 1}, "liveview.log", "liveview"},
			{BrowserEvent{Type: "captcha.solve", Category: CategoryCaptcha, SourceKind: SourceExtension, Seq: 1, Ts: 1}, "captcha.log", "captcha"},
			{BrowserEvent{Type: "page.navigation", Category: CategoryPage, SourceKind: SourceCDP, Seq: 1, Ts: 1}, "page.log", "page"},
			{BrowserEvent{Type: "input.click", Category: CategoryInteraction, SourceKind: SourceCDP, Seq: 1, Ts: 1}, "interaction.log", "interaction"},
			{BrowserEvent{Type: "monitor.connected", Category: CategorySystem, SourceKind: SourceKernelAPI, Seq: 1, Ts: 1}, "system.log", "system"},
		}

		for _, e := range eventsToFile {
			require.NoError(t, fw.Write(e.ev))
		}

		for _, e := range eventsToFile {
			data, err := os.ReadFile(filepath.Join(dir, e.file))
			require.NoError(t, err, "missing file %s for type %s", e.file, e.ev.Type)

			line := bytes.TrimRight(data, "\n")
			require.True(t, json.Valid(line), "invalid JSON in %s", e.file)

			var decoded map[string]any
			require.NoError(t, json.Unmarshal(line, &decoded))
			assert.Equal(t, e.category, decoded["category"], "wrong category in %s", e.file)
			assert.Equal(t, string(e.ev.SourceKind), decoded["source_kind"], "wrong source_kind in %s", e.file)
		}
	})

	t.Run("empty_category_rejected", func(t *testing.T) {
		dir := t.TempDir()
		fw := NewFileWriter(dir)
		defer fw.Close()

		err := fw.Write(BrowserEvent{Type: "mystery", Category: "", SourceKind: SourceCDP, Seq: 1, Ts: 1})
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
					ev := BrowserEvent{
						Seq:        uint64(i*eventsPerGoroutine + j),
						Type:       "console.log",
						Category:   CategoryConsole,
						SourceKind: SourceCDP,
						Ts:         1,
					}
					require.NoError(t, fw.Write(ev))
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

		require.NoError(t, fw.Write(BrowserEvent{Type: "console.log", Category: CategoryConsole, SourceKind: SourceCDP, Seq: 1, Ts: 1}))

		entries, err = os.ReadDir(dir)
		require.NoError(t, err)
		assert.Len(t, entries, 1, "expected exactly one file after first Write")
		assert.Equal(t, "console.log", entries[0].Name())
	})
}

// TestPipeline: Pipeline glue type tests.
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

	t.Run("publish_increments_seq", func(t *testing.T) {
		p, _ := newPipeline(t)
		reader := p.NewReader()

		for i := 0; i < 3; i++ {
			p.Publish(BrowserEvent{Type: "page.navigation", Category: CategoryPage, SourceKind: SourceCDP, Ts: 1})
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
		p.Publish(BrowserEvent{Type: "page.navigation", Category: CategoryPage, SourceKind: SourceCDP}) // Ts == 0
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

		p.Publish(BrowserEvent{Type: "console.log", Category: CategoryConsole, SourceKind: SourceCDP, Ts: 1})

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
		p.Publish(BrowserEvent{Type: "page.navigation", Category: CategoryPage, SourceKind: SourceCDP, Ts: 1})

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
		p.Publish(BrowserEvent{Type: "page.navigation", Category: CategoryPage, SourceKind: SourceCDP, Ts: 1})

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

		p.Publish(BrowserEvent{
			Type:       "page.navigation",
			Category:   CategoryPage,
			SourceKind: SourceCDP,
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

		p.Publish(BrowserEvent{Type: "console.log", Category: CategoryConsole, SourceKind: SourceCDP, Ts: 1})

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		ev, err := reader.Read(ctx)
		require.NoError(t, err)
		assert.Equal(t, DetailDefault, ev.DetailLevel)

		p.Publish(BrowserEvent{Type: "console.log", Category: CategoryConsole, SourceKind: SourceCDP, Ts: 1, DetailLevel: DetailVerbose})
		ev2, err := reader.Read(ctx)
		require.NoError(t, err)
		assert.Equal(t, DetailVerbose, ev2.DetailLevel)
	})
}
