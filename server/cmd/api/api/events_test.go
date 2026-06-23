package api

import (
	"bufio"
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kernel/kernel-images/server/lib/events"
	oapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEventLifecycle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := newTestService(t, newMockRecordManager())

	// Start a telemetry session.
	startResp, err := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{})
	require.NoError(t, err)
	require.IsType(t, oapi.PutTelemetry201JSONResponse{}, startResp)

	// Open an SSE stream (5s budget covers the three 2s selects below).
	streamCtx, streamCancel := context.WithTimeout(ctx, 5*time.Second)
	defer streamCancel()
	streamResp, err := svc.StreamTelemetryEvents(streamCtx, oapi.StreamTelemetryEventsRequestObject{})
	require.NoError(t, err)
	r200, ok := streamResp.(oapi.StreamTelemetryEvents200TexteventStreamResponse)
	require.True(t, ok)

	// Drain SSE frames into a channel.
	received := make(chan events.Envelope, 4)
	go func() {
		defer close(received)
		rd := bufio.NewReader(r200.Body)
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				return
			}
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data: "))
			var env events.Envelope
			if err := json.Unmarshal([]byte(payload), &env); err != nil {
				continue
			}
			received <- env
		}
	}()

	// Publish a custom event. Unknown types must carry an explicit category.
	sys := oapi.PublishEventRequestCategorySystem
	resp, err := svc.PublishTelemetryEvent(ctx, oapi.PublishTelemetryEventRequestObject{
		Body: &oapi.PublishEventRequest{Type: "test.event", Category: &sys},
	})
	require.NoError(t, err)
	r200pub, ok := resp.(publishTelemetryEventOKResponse)
	require.True(t, ok, "expected 200 response")
	assert.Equal(t, "test.event", r200pub.env.Event.Type)
	assert.Greater(t, r200pub.env.Seq, uint64(0))

	// Verify the published event arrives on the stream with the same seq.
	select {
	case env := <-received:
		assert.Equal(t, "test.event", env.Event.Type)
		assert.Equal(t, r200pub.env.Seq, env.Seq)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for test.event")
	}

	// Stop telemetry by disabling every category.
	stopResp, err := svc.PatchTelemetry(ctx, oapi.PatchTelemetryRequestObject{
		Body: &oapi.BrowserTelemetryConfig{Browser: allCategoriesDisabled()},
	})
	require.NoError(t, err)
	assert.IsType(t, oapi.PatchTelemetry200JSONResponse{}, stopResp)
}

func TestPublishDroppedWhenTelemetryInactive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := newTestService(t, newMockRecordManager())

	sys := oapi.PublishEventRequestCategorySystem
	resp, err := svc.PublishTelemetryEvent(ctx, oapi.PublishTelemetryEventRequestObject{
		Body: &oapi.PublishEventRequest{Type: "test.event", Category: &sys},
	})
	require.NoError(t, err)
	assert.IsType(t, oapi.PublishTelemetryEvent204Response{}, resp, "filtered events should return 204")
}

func TestPublishRequiresCategoryForUnknownType(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := newTestService(t, newMockRecordManager())
	_, err := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{})
	require.NoError(t, err)

	resp, err := svc.PublishTelemetryEvent(ctx, oapi.PublishTelemetryEventRequestObject{
		Body: &oapi.PublishEventRequest{Type: "custom.unknown"},
	})
	require.NoError(t, err)
	assert.IsType(t, oapi.PublishTelemetryEvent400JSONResponse{}, resp, "unknown type without a category must 400")
}

func TestPublishKnownTypeCategoryIsServerAuthoritative(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := newTestService(t, newMockRecordManager())
	_, err := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{})
	require.NoError(t, err)

	// api_call is a known type that maps to the control category. A caller
	// supplying a different category must be overridden by the server.
	console := oapi.PublishEventRequestCategoryConsole
	resp, err := svc.PublishTelemetryEvent(ctx, oapi.PublishTelemetryEventRequestObject{
		Body: &oapi.PublishEventRequest{Type: "api_call", Category: &console},
	})
	require.NoError(t, err)
	okResp, ok := resp.(publishTelemetryEventOKResponse)
	require.True(t, ok, "expected 200, got %T", resp)
	assert.Equal(t, events.Control, okResp.env.Event.Category)
}

func TestPublishDroppedWhenCategoryDisabled(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := newTestService(t, newMockRecordManager())

	// Start a session that only enables the console category. A page event
	// should be filtered out and return 204.
	tr, f := true, false
	_, err := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{
		Body: &oapi.BrowserTelemetryConfig{
			Browser: &oapi.BrowserTelemetryCategoriesConfig{
				Console:     &oapi.BrowserTelemetryCategoryConfig{Enabled: &tr},
				Network:     &oapi.BrowserTelemetryCategoryConfig{Enabled: &f},
				Page:        &oapi.BrowserTelemetryCategoryConfig{Enabled: &f},
				Interaction: &oapi.BrowserTelemetryCategoryConfig{Enabled: &f},
			},
		},
	})
	require.NoError(t, err)

	page := oapi.PublishEventRequestCategoryPage
	resp, err := svc.PublishTelemetryEvent(ctx, oapi.PublishTelemetryEventRequestObject{
		Body: &oapi.PublishEventRequest{Type: "test.page", Category: &page},
	})
	require.NoError(t, err)
	assert.IsType(t, oapi.PublishTelemetryEvent204Response{}, resp, "events in disabled categories should return 204")
}

// publishTestEvents publishes n system events through an already-started
// telemetry session. Seqs run 1..n on a fresh stream.
func publishTestEvents(ctx context.Context, t *testing.T, svc *ApiService, n int) {
	t.Helper()
	sys := oapi.PublishEventRequestCategorySystem
	for i := 0; i < n; i++ {
		resp, err := svc.PublishTelemetryEvent(ctx, oapi.PublishTelemetryEventRequestObject{
			Body: &oapi.PublishEventRequest{Type: "test.event", Category: &sys},
		})
		require.NoError(t, err)
		require.IsType(t, publishTelemetryEventOKResponse{}, resp, "publish %d expected 200", i)
	}
}

// streamFirstID opens the stream with the given params and returns the id of
// the first SSE frame. The stream context is bounded so the read cannot hang.
func streamFirstID(t *testing.T, svc *ApiService, params oapi.StreamTelemetryEventsParams) uint64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	resp, err := svc.StreamTelemetryEvents(ctx, oapi.StreamTelemetryEventsRequestObject{Params: params})
	require.NoError(t, err)
	r200, ok := resp.(oapi.StreamTelemetryEvents200TexteventStreamResponse)
	require.True(t, ok, "expected SSE response, got %T", resp)

	rd := bufio.NewReader(r200.Body)
	for {
		line, err := rd.ReadString('\n')
		require.NoError(t, err, "stream closed before any id frame")
		if !strings.HasPrefix(line, "id: ") {
			continue
		}
		id, err := strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(line, "id: ")), 10, 64)
		require.NoError(t, err)
		return id
	}
}

func TestResolveStartSeq(t *testing.T) {
	t.Parallel()
	const current = 10
	all := oapi.All
	other := oapi.StreamTelemetryEventsParamsReplay("foo")
	cases := []struct {
		name        string
		lastEventID *string
		replay      *oapi.StreamTelemetryEventsParamsReplay
		want        uint64
	}{
		{"fresh connection is from-now", nil, nil, current},
		{"replay=all starts from oldest", nil, &all, 0},
		{"non-all replay falls back to from-now", nil, &other, current},
		{"Last-Event-ID resumes after seq", ptrOf("5"), nil, 5},
		{"Last-Event-ID wins over replay=all", ptrOf("5"), &all, 5},
		{"Last-Event-ID 0 stays from-now even with replay=all", ptrOf("0"), &all, current},
		{"empty Last-Event-ID is treated as absent", ptrOf(""), &all, 0},
		{"unparseable Last-Event-ID falls back to from-now", ptrOf("abc"), &all, current},
		{"negative Last-Event-ID falls back to from-now", ptrOf("-1"), &all, current},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, resolveStartSeq(tc.lastEventID, tc.replay, current))
		})
	}
}

func TestStreamReplayAllFromOldest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := newTestService(t, newMockRecordManager())
	_, err := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{})
	require.NoError(t, err)

	publishTestEvents(ctx, t, svc, 5)

	replay := oapi.All
	id := streamFirstID(t, svc, oapi.StreamTelemetryEventsParams{Replay: &replay})
	assert.Equal(t, uint64(1), id, "replay=all on an unfilled buffer should start at the lowest seq")
}

func TestStreamReplayAllAfterEviction(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := newTestService(t, newMockRecordManager())
	_, err := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{})
	require.NoError(t, err)

	// Overfill the ring so the head is evicted. The oldest retained seq is
	// latestSeq - cap + 1, surfaced as a first id greater than 1.
	total := testRingCapacity + 36
	publishTestEvents(ctx, t, svc, total)

	replay := oapi.All
	id := streamFirstID(t, svc, oapi.StreamTelemetryEventsParams{Replay: &replay})
	assert.Equal(t, uint64(total-testRingCapacity+1), id, "replay=all after eviction should start at the oldest retained seq")
}

func TestStreamReplayAllAtCapacityBoundary(t *testing.T) {
	t.Parallel()
	// oldestSeq() switches at latestSeq == cap: a buffer filled to exactly cap
	// still starts at seq 1, while one event past cap starts at seq 2. Guards
	// the <= comparison in ringBuffer.oldestSeq against an off-by-one.
	cases := []struct {
		name      string
		published int
		wantID    uint64
	}{
		{"exactly full starts at seq 1", testRingCapacity, 1},
		{"one past full starts at seq 2", testRingCapacity + 1, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			svc := newTestService(t, newMockRecordManager())
			_, err := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{})
			require.NoError(t, err)

			publishTestEvents(ctx, t, svc, tc.published)

			replay := oapi.All
			id := streamFirstID(t, svc, oapi.StreamTelemetryEventsParams{Replay: &replay})
			assert.Equal(t, tc.wantID, id)
		})
	}
}

func TestStreamResumeAfterLastEventIDUnchanged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := newTestService(t, newMockRecordManager())
	_, err := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{})
	require.NoError(t, err)

	publishTestEvents(ctx, t, svc, 10)

	id := streamFirstID(t, svc, oapi.StreamTelemetryEventsParams{LastEventID: ptrOf("5")})
	assert.Equal(t, uint64(6), id, "Last-Event-ID without replay must behave as before and resume after seq 5")
}
