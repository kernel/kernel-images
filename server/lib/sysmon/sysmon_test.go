package sysmon

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/kernel/kernel-images/server/lib/events"
	oapi "github.com/kernel/kernel-images/server/lib/oapi"
)

// TestKmsgRoundTrip pipes a synthetic OOM line through a FIFO and verifies an
// event lands in the EventStream with the right schema. Linux only — /dev/kmsg
// semantics don't apply here, but we open a regular FIFO so the goroutine just
// reads bytes; the parser is what we exercise.
func TestKmsgRoundTrip(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "kmsg")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unsupported: %v", err)
	}

	es, err := events.NewEventStream(events.EventStreamConfig{RingCapacity: 16})
	if err != nil {
		t.Fatalf("event stream: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mon := New(es, logger, WithKmsgPath(fifo))
	mon.Start(ctx)

	// Open writer side after the goroutine has had time to open the reader.
	// Opening the FIFO for write blocks until a reader is present, which
	// gives us synchronization for free.
	w, err := os.OpenFile(fifo, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	defer w.Close()

	line := "<4,123,456789,->;Out of memory: Killed process 4242 (renderer) total-vm:200kB, anon-rss:150kB, file-rss:10kB, shmem-rss:5kB, UID:1000 pgtables:1kB oom_score_adj:300\n"
	if _, err := w.Write([]byte(line)); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Poll the stream until the event arrives or we time out.
	reader := es.NewReader(0)
	readCtx, readCancel := context.WithTimeout(ctx, 2*time.Second)
	defer readCancel()
	res, err := reader.Read(readCtx)
	if err != nil {
		t.Fatalf("read envelope: %v", err)
	}
	ev := res.Envelope.Event
	if ev.Type != string(oapi.SystemOomKill) {
		t.Fatalf("Type = %q, want %q", ev.Type, oapi.SystemOomKill)
	}
	if ev.Category != events.System {
		t.Errorf("Category = %q, want system", ev.Category)
	}
	if ev.Source.Kind != oapi.LocalProcess {
		t.Errorf("Source.Kind = %q", ev.Source.Kind)
	}
	if ev.Source.Event == nil || *ev.Source.Event != "linux.oom_kill" {
		t.Errorf("Source.Event = %v", ev.Source.Event)
	}

	var data oapi.BrowserSystemOomKillEventData
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	if data.Pid != 4242 {
		t.Errorf("Pid = %d", data.Pid)
	}
	if data.ProcessName != "renderer" {
		t.Errorf("ProcessName = %q", data.ProcessName)
	}
	if data.RssKb != 165 { // 150+10+5
		t.Errorf("RssKb = %d, want 165", data.RssKb)
	}
	if data.OomScoreAdj == nil || *data.OomScoreAdj != 300 {
		t.Errorf("OomScoreAdj = %v", data.OomScoreAdj)
	}
}
