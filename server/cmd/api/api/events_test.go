package api

import (
	"bufio"
	"context"
	"encoding/json"
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

	// Publish an event.
	resp, err := svc.PublishTelemetryEvent(ctx, oapi.PublishTelemetryEventRequestObject{
		Body: &oapi.PublishEventRequest{Type: "test.event"},
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

	// Stop telemetry by disabling all categories.
	f := false
	stopResp, err := svc.PatchTelemetry(ctx, oapi.PatchTelemetryRequestObject{
		Body: &oapi.BrowserTelemetryConfig{
			Browser: &oapi.BrowserTelemetryCategoriesConfig{
				Console:     &oapi.BrowserTelemetryCategoryConfig{Enabled: &f},
				Network:     &oapi.BrowserTelemetryCategoryConfig{Enabled: &f},
				Page:        &oapi.BrowserTelemetryCategoryConfig{Enabled: &f},
				Interaction: &oapi.BrowserTelemetryCategoryConfig{Enabled: &f},
			},
		},
	})
	require.NoError(t, err)
	assert.IsType(t, oapi.PatchTelemetry200JSONResponse{}, stopResp)
}
