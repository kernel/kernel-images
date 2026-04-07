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

	// Start a capture session.
	startResp, err := svc.StartCaptureSession(ctx, oapi.StartCaptureSessionRequestObject{})
	require.NoError(t, err)
	require.IsType(t, oapi.StartCaptureSession201JSONResponse{}, startResp)

	// Open an SSE stream (5s budget covers the three 2s selects below).
	streamCtx, streamCancel := context.WithTimeout(ctx, 5*time.Second)
	defer streamCancel()
	streamResp, err := svc.StreamEvents(streamCtx, oapi.StreamEventsRequestObject{})
	require.NoError(t, err)
	r200, ok := streamResp.(oapi.StreamEvents200TexteventStreamResponse)
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
	resp, err := svc.PublishEvent(ctx, oapi.PublishEventRequestObject{
		Body: &oapi.PublishEventRequest{Type: "test.event"},
	})
	require.NoError(t, err)
	r200pub, ok := resp.(publishEventOKResponse)
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

	// Stop the session.
	stopResp, err := svc.StopCaptureSession(ctx, oapi.StopCaptureSessionRequestObject{})
	require.NoError(t, err)
	assert.IsType(t, oapi.StopCaptureSession200JSONResponse{}, stopResp)
}

func TestStartCaptureConfigFrom(t *testing.T) {
	t.Run("nil body returns defaults", func(t *testing.T) {
		cfg, err := startCaptureConfigFrom(nil)
		require.NoError(t, err)
		assert.Empty(t, cfg.Categories)
		assert.Empty(t, cfg.DetailLevel)
	})

	t.Run("valid categories", func(t *testing.T) {
		cats := []oapi.StartCaptureRequestCategories{oapi.StartCaptureRequestCategoriesConsole, oapi.StartCaptureRequestCategoriesNetwork}
		body := &oapi.StartCaptureRequest{Categories: &cats}
		cfg, err := startCaptureConfigFrom(body)
		require.NoError(t, err)
		assert.Len(t, cfg.Categories, 2)
	})

	t.Run("invalid category returns error", func(t *testing.T) {
		cats := []oapi.StartCaptureRequestCategories{"bogus"}
		body := &oapi.StartCaptureRequest{Categories: &cats}
		_, err := startCaptureConfigFrom(body)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unknown category")
	})

	t.Run("valid detail level", func(t *testing.T) {
		dl := oapi.StartCaptureRequestDetailLevelVerbose
		body := &oapi.StartCaptureRequest{DetailLevel: &dl}
		cfg, err := startCaptureConfigFrom(body)
		require.NoError(t, err)
		assert.Equal(t, "verbose", string(cfg.DetailLevel))
	})

	t.Run("invalid detail level returns error", func(t *testing.T) {
		dl := oapi.StartCaptureRequestDetailLevel("garbage")
		body := &oapi.StartCaptureRequest{DetailLevel: &dl}
		_, err := startCaptureConfigFrom(body)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unknown detail level")
	})

	t.Run("nil categories and nil detail level", func(t *testing.T) {
		body := &oapi.StartCaptureRequest{}
		cfg, err := startCaptureConfigFrom(body)
		require.NoError(t, err)
		assert.Empty(t, cfg.Categories)
		assert.Empty(t, cfg.DetailLevel)
	})
}

func TestStartCapture(t *testing.T) {
	ctx := context.Background()

	t.Run("success with no body", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager())
		resp, err := svc.StartCapture(ctx, oapi.StartCaptureRequestObject{})
		require.NoError(t, err)
		assert.IsType(t, oapi.StartCapture200Response{}, resp)
	})

	t.Run("invalid category returns 400", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager())
		cats := []oapi.StartCaptureRequestCategories{"badcat"}
		resp, err := svc.StartCapture(ctx, oapi.StartCaptureRequestObject{
			Body: &oapi.StartCaptureRequest{Categories: &cats},
		})
		require.NoError(t, err)
		assert.IsType(t, oapi.StartCapture400JSONResponse{}, resp)
	})

	t.Run("invalid detail level returns 400", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager())
		dl := oapi.StartCaptureRequestDetailLevel("garbage")
		resp, err := svc.StartCapture(ctx, oapi.StartCaptureRequestObject{
			Body: &oapi.StartCaptureRequest{DetailLevel: &dl},
		})
		require.NoError(t, err)
		assert.IsType(t, oapi.StartCapture400JSONResponse{}, resp)
	})

	t.Run("restart succeeds", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager())
		_, err := svc.StartCapture(ctx, oapi.StartCaptureRequestObject{})
		require.NoError(t, err)
		resp, err := svc.StartCapture(ctx, oapi.StartCaptureRequestObject{})
		require.NoError(t, err)
		assert.IsType(t, oapi.StartCapture200Response{}, resp)
	})
}

func TestStopCapture(t *testing.T) {
	ctx := context.Background()

	t.Run("stop when nothing running", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager())
		resp, err := svc.StopCapture(ctx, oapi.StopCaptureRequestObject{})
		require.NoError(t, err)
		assert.IsType(t, oapi.StopCapture200Response{}, resp)
	})

	t.Run("stop after start", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager())
		_, err := svc.StartCapture(ctx, oapi.StartCaptureRequestObject{})
		require.NoError(t, err)
		resp, err := svc.StopCapture(ctx, oapi.StopCaptureRequestObject{})
		require.NoError(t, err)
		assert.IsType(t, oapi.StopCapture200Response{}, resp)
	})
}

