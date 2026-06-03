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
