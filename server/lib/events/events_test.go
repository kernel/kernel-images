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

// TestBrowserEvent: construct BrowserEvent with all SCHEMA-01 fields; marshal to JSON;
// assert all snake_case keys present.
func TestBrowserEvent(t *testing.T) {
	ev := BrowserEvent{
		CaptureSessionID: "test-session-id",
		Seq:              1,
		Ts:               1234567890000,
		Type:             "console_log",
		TargetID:         "target-1",
		CDPSessionID:     "cdp-session-1",
		FrameID:          "frame-1",
		ParentFrameID:    "parent-frame-1",
		URL:              "https://example.com",
		Data:             json.RawMessage(`{"message":"hello"}`),
		Truncated:        false,
	}

	b, err := json.Marshal(ev)
	require.NoError(t, err)

	s := string(b)
	assert.Contains(t, s, `"capture_session_id"`)
	assert.Contains(t, s, `"seq"`)
	assert.Contains(t, s, `"ts"`)
	assert.Contains(t, s, `"type"`)
	assert.Contains(t, s, `"target_id"`)
	assert.Contains(t, s, `"cdp_session_id"`)
	assert.Contains(t, s, `"frame_id"`)
	assert.Contains(t, s, `"parent_frame_id"`)
	assert.Contains(t, s, `"url"`)
	assert.Contains(t, s, `"data"`)
}

// TestBrowserEventData: embed a pre-serialized JSON object in Data field; marshal outer event;
// assert Data appears verbatim (no double-encoding).
func TestBrowserEventData(t *testing.T) {
	rawData := json.RawMessage(`{"key":"value","num":42}`)
	ev := BrowserEvent{
		CaptureSessionID: "test-session",
		Seq:              1,
		Ts:               1000,
		Type:             "cdp_event",
		Data:             rawData,
	}

	b, err := json.Marshal(ev)
	require.NoError(t, err)

	s := string(b)
	// Data must appear verbatim — no double-encoding (should not be escaped string)
	assert.Contains(t, s, `"data":{"key":"value","num":42}`)
	assert.NotContains(t, s, `"data":"{`) // would indicate double-encoding
}

// TestCategoryFor: table-driven; assert prefix routing is correct.
func TestCategoryFor(t *testing.T) {
	cases := []struct {
		eventType string
		expected  EventCategory
	}{
		{"console_log", CategoryConsole},
		{"network_request", CategoryNetwork},
		{"liveview_click", CategoryLiveview},
		{"captcha_solve", CategoryCaptcha},
		{"cdp_nav", CategoryCDP},
		{"unknown_type", CategoryCDP},
	}

	for _, tc := range cases {
		t.Run(tc.eventType, func(t *testing.T) {
			got := CategoryFor(tc.eventType)
			assert.Equal(t, tc.expected, got)
		})
	}
}

// TestRingBuffer: publish 3 events; reader reads all 3 in order.
func TestRingBuffer(t *testing.T) {
	rb := NewRingBuffer(10)
	reader := rb.NewReader()

	events := []BrowserEvent{
		{Seq: 1, Type: "cdp_event_1"},
		{Seq: 2, Type: "cdp_event_2"},
		{Seq: 3, Type: "cdp_event_3"},
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
	}
}

// TestRingBufferOverflow: ring capacity 2; publish 3 events with no reader;
// assert write returns immediately (no block); reader receives events_dropped then newest events.
func TestRingBufferOverflow(t *testing.T) {
	rb := NewRingBuffer(2)

	// Publish 3 events with no reader — must not block
	done := make(chan struct{})
	go func() {
		rb.Publish(BrowserEvent{Seq: 1, Type: "cdp_event_1"})
		rb.Publish(BrowserEvent{Seq: 2, Type: "cdp_event_2"})
		rb.Publish(BrowserEvent{Seq: 3, Type: "cdp_event_3"})
		close(done)
	}()

	select {
	case <-done:
		// good — did not block
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Publish blocked with no readers")
	}

	// Create reader after overflow; should get events_dropped then available events
	reader := rb.NewReader()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	first, err := reader.Read(ctx)
	require.NoError(t, err)
	assert.Equal(t, "events_dropped", first.Type)
}

// TestEventsDropped: ring capacity 2; reader gets notify channel; publish 3 events;
// reader reads; assert first result is events_dropped BrowserEvent.
func TestEventsDropped(t *testing.T) {
	rb := NewRingBuffer(2)
	reader := rb.NewReader()

	// Publish 3 events, overflowing the ring (capacity 2)
	rb.Publish(BrowserEvent{Seq: 1, Type: "cdp_event_1"})
	rb.Publish(BrowserEvent{Seq: 2, Type: "cdp_event_2"})
	rb.Publish(BrowserEvent{Seq: 3, Type: "cdp_event_3"})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	first, err := reader.Read(ctx)
	require.NoError(t, err)
	assert.Equal(t, "events_dropped", first.Type)

	// Data must be valid JSON with a "dropped" count
	require.NotNil(t, first.Data)
	assert.True(t, json.Valid(first.Data))
	assert.Contains(t, string(first.Data), `"dropped"`)
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

	// Publish events after readers are created
	for i := 0; i < numEvents; i++ {
		rb.Publish(BrowserEvent{Seq: uint64(i + 1), Type: "cdp_event"})
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
				require.NoError(t, err)
				evs = append(evs, ev)
			}
			results[idx] = evs
		}(i, r)
	}

	wg.Wait()

	// Each reader must have received all 5 events
	for i, evs := range results {
		assert.Len(t, evs, numEvents, "reader %d", i)
		for j, ev := range evs {
			assert.Equal(t, uint64(j+1), ev.Seq, "reader %d event %d", i, j)
		}
	}
}

// TestFileWriter: per-category JSONL appender tests.
func TestFileWriter(t *testing.T) {
	t.Run("writes_to_correct_file", func(t *testing.T) {
		dir := t.TempDir()
		fw := NewFileWriter(dir)
		defer fw.Close()

		ev := BrowserEvent{
			CaptureSessionID: "sess-1",
			Seq:              1,
			Ts:               1000,
			Type:             "console_log",
			Data:             json.RawMessage(`{"message":"hello"}`),
		}
		require.NoError(t, fw.Write(ev))

		data, err := os.ReadFile(filepath.Join(dir, "console.log"))
		require.NoError(t, err)

		lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
		require.Len(t, lines, 1)
		assert.True(t, json.Valid([]byte(lines[0])))
		assert.Contains(t, lines[0], `"capture_session_id"`)
		assert.Contains(t, lines[0], `"console_log"`)
	})

	t.Run("category_routing", func(t *testing.T) {
		dir := t.TempDir()
		fw := NewFileWriter(dir)
		defer fw.Close()

		typeToFile := map[string]string{
			"console_log":     "console.log",
			"network_request": "network.log",
			"liveview_click":  "liveview.log",
			"captcha_solve":   "captcha.log",
			"cdp_navigation":  "cdp.log",
		}

		for typ := range typeToFile {
			require.NoError(t, fw.Write(BrowserEvent{Type: typ, Seq: 1, Ts: 1}))
		}

		for typ, file := range typeToFile {
			data, err := os.ReadFile(filepath.Join(dir, file))
			require.NoError(t, err, "missing file for type %s", typ)
			assert.True(t, json.Valid(bytes.TrimRight(data, "\n")))
		}
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
						Seq:  uint64(i*eventsPerGoroutine + j),
						Type: "console_log",
						Ts:   1,
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

		// No writes yet — directory should be empty.
		entries, err := os.ReadDir(dir)
		require.NoError(t, err)
		assert.Empty(t, entries, "files opened before first Write")

		require.NoError(t, fw.Write(BrowserEvent{Type: "console_log", Seq: 1, Ts: 1}))

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
			p.Publish(BrowserEvent{Type: "cdp_event", Ts: 1})
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
		p.Publish(BrowserEvent{Type: "cdp_event"}) // Ts == 0
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

		p.Publish(BrowserEvent{Type: "console_log", Ts: 1})

		data, err := os.ReadFile(filepath.Join(dir, "console.log"))
		require.NoError(t, err)

		lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
		require.Len(t, lines, 1)
		assert.True(t, json.Valid([]byte(lines[0])))
		assert.Contains(t, lines[0], `"console_log"`)
	})

	t.Run("publish_writes_ring", func(t *testing.T) {
		p, _ := newPipeline(t)

		// Subscribe reader BEFORE publish.
		reader := p.NewReader()
		p.Publish(BrowserEvent{Type: "cdp_event", Ts: 1})

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		ev, err := reader.Read(ctx)
		require.NoError(t, err)
		assert.Equal(t, "cdp_event", ev.Type)
	})

	t.Run("start_sets_capture_session_id", func(t *testing.T) {
		p, _ := newPipeline(t)
		p.Start("test-uuid")

		reader := p.NewReader()
		p.Publish(BrowserEvent{Type: "cdp_event", Ts: 1})

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
			Type: "cdp_event",
			Ts:   1,
			Data: json.RawMessage(rawData),
		})

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		// Ring buffer event must have Truncated==true.
		ev, err := reader.Read(ctx)
		require.NoError(t, err)
		assert.True(t, ev.Truncated)

		// File must contain valid JSON with truncated==true.
		data, err := os.ReadFile(filepath.Join(dir, "cdp.log"))
		require.NoError(t, err)
		lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
		require.Len(t, lines, 1)
		assert.True(t, json.Valid([]byte(lines[0])))
		assert.Contains(t, lines[0], `"truncated":true`)
	})
}

// TestTruncation: construct event with Data = 1.1MB JSON bytes; call truncateIfNeeded;
// assert Truncated==true and json.Valid(result.Data)==true and len(marshal(result)) <= 1_000_000.
func TestTruncation(t *testing.T) {
	// Build a Data field that is ~1.1MB
	largeData := strings.Repeat("x", 1_100_000)
	rawData, err := json.Marshal(map[string]string{"payload": largeData})
	require.NoError(t, err)

	ev := BrowserEvent{
		CaptureSessionID: "test-session",
		Seq:              1,
		Ts:               1000,
		Type:             "cdp_event",
		Data:             json.RawMessage(rawData),
	}

	result := truncateIfNeeded(ev)

	assert.True(t, result.Truncated)
	assert.True(t, json.Valid(result.Data))

	marshaled, err := json.Marshal(result)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(marshaled), 1_000_000)
}
