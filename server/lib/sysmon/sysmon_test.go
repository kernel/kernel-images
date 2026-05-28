package sysmon

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/kernel/kernel-images/server/lib/events"
	oapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/kernel/kernel-images/server/lib/telemetry"
)

// newTestPublisher wires up a TelemetrySession-backed publisher with an
// active session so events flow through to the returned reader.
func newTestPublisher(t *testing.T, capacity int) (PublishFunc, *events.Reader) {
	t.Helper()
	es, err := events.NewEventStream(events.EventStreamConfig{RingCapacity: capacity})
	require.NoError(t, err)
	ts := telemetry.NewTelemetrySession(es)
	ts.Start("test-session", telemetry.TelemetryConfig{})
	return ts.Publish, es.NewReader(0)
}

// stubKmsgSource pushes synthetic kmsg messages through an in-memory
// channel. Closing the source via Close() (typically triggered by the
// Monitor's ctx-done watcher) terminates the message channel so the
// reader goroutine exits cleanly.
type stubKmsgSource struct {
	ch     chan KmsgMessage
	closed chan struct{}
}

func newStubKmsgSource() *stubKmsgSource {
	return &stubKmsgSource{
		ch:     make(chan KmsgMessage, 32),
		closed: make(chan struct{}),
	}
}

func (s *stubKmsgSource) Messages() <-chan KmsgMessage { return s.ch }

func (s *stubKmsgSource) Close() error {
	select {
	case <-s.closed:
	default:
		close(s.closed)
		close(s.ch)
	}
	return nil
}

func (s *stubKmsgSource) send(body string, ts time.Time) {
	s.ch <- KmsgMessage{Body: body, Timestamp: ts}
}

func TestMonitorPublishesOomKillEnd2End(t *testing.T) {
	publish, reader := newTestPublisher(t, 16)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	src := newStubKmsgSource()
	mon := New(publish, logger, withKmsgSource(src))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, mon.Start(ctx))

	ts := time.Unix(1_700_000_000, 0)
	for _, line := range canonicalOomDump {
		src.send(line, ts)
	}

	readCtx, readCancel := context.WithTimeout(ctx, 2*time.Second)
	defer readCancel()
	res, err := reader.Read(readCtx)
	require.NoError(t, err)

	ev := res.Envelope.Event
	assert.Equal(t, string(oapi.SystemOomKill), ev.Type)
	assert.Equal(t, events.System, ev.Category)
	assert.Equal(t, oapi.LocalProcess, ev.Source.Kind)
	require.NotNil(t, ev.Source.Event)
	assert.Equal(t, "linux.oom_kill", *ev.Source.Event)
	assert.Equal(t, ts.UnixMicro(), ev.Ts)

	var data oapi.BrowserSystemOomKillEventData
	require.NoError(t, json.Unmarshal(ev.Data, &data))
	assert.Equal(t, "chromium", data.ProcessName)
	assert.Equal(t, 1234, data.Pid)
	assert.Equal(t, 4823900+100+200, data.RssKb)
	require.NotNil(t, data.Constraint)
	assert.Equal(t, oapi.BrowserSystemOomKillEventDataConstraint("none"), *data.Constraint)

	// Mem-Info and Tasks-state fields round-trip through json correctly.
	require.NotNil(t, data.MemTotalKb)
	assert.Equal(t, 524288*4, *data.MemTotalKb)
	require.NotNil(t, data.MemFreeKb)
	assert.Equal(t, 4560*4, *data.MemFreeKb)
	require.NotNil(t, data.TopTasks)
	require.Len(t, *data.TopTasks, 4)
	assert.Equal(t, "chromium", (*data.TopTasks)[0].Name)

	// Trigger fields round-trip too. In the canonical dump the trigger
	// and the victim are the same process.
	require.NotNil(t, data.TriggerProcessName)
	assert.Equal(t, "chromium", *data.TriggerProcessName)
	require.NotNil(t, data.TriggerPid)
	assert.Equal(t, 1234, *data.TriggerPid)
}

func TestMonitorOmitsUnknownConstraint(t *testing.T) {
	// constraintFromKernel passes through unknown labels lowercased so
	// they reach logs, but they would violate the openapi enum if
	// emitted on the wire. The publisher must drop them rather than
	// produce a non-enum value that SDKs may reject.
	dump := []string{
		`x invoked oom-killer: gfp_mask=0, order=0, oom_score_adj=0`,
		`oom-kill:constraint=CONSTRAINT_FUTURE_THING,task=x,pid=1,uid=0`,
		`Out of memory: Killed process 1 (x) total-vm:0kB, anon-rss:1kB, file-rss:0kB, shmem-rss:0kB, UID:0 pgtables:0kB oom_score_adj:0`,
	}

	publish, reader := newTestPublisher(t, 4)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	src := newStubKmsgSource()
	mon := New(publish, logger, withKmsgSource(src))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, mon.Start(ctx))

	ts := time.Unix(1_700_000_000, 0)
	for _, line := range dump {
		src.send(line, ts)
	}

	readCtx, readCancel := context.WithTimeout(ctx, 2*time.Second)
	defer readCancel()
	res, err := reader.Read(readCtx)
	require.NoError(t, err)

	var data oapi.BrowserSystemOomKillEventData
	require.NoError(t, json.Unmarshal(res.Envelope.Event.Data, &data))
	assert.Nil(t, data.Constraint, "unknown constraint must be omitted from the payload")
}

func TestMonitorShutsDownOnContextCancel(t *testing.T) {
	publish, _ := newTestPublisher(t, 4)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	src := newStubKmsgSource()
	mon := New(publish, logger, withKmsgSource(src))

	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, mon.Start(ctx))

	cancel()

	done := make(chan struct{})
	go func() {
		mon.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("monitor did not shut down within 2s of context cancellation")
	}
}
