package e2e

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	instanceoapi "github.com/kernel/kernel-images/server/lib/oapi"
)

// TestReadTelemetryEvents starts a headless container with S2 credentials,
// publishes a known set of events, and reads them back through
// GET /telemetry/events. It exercises the full archive read path against a real
// S2 stream rather than the in-memory ring buffer.
//
// Skips automatically when S2_BASIN, S2_ACCESS_TOKEN, or S2_STREAM are unset.
func TestReadTelemetryEvents(t *testing.T) {
	basin := os.Getenv("S2_BASIN")
	accessToken := os.Getenv("S2_ACCESS_TOKEN")
	stream := os.Getenv("S2_STREAM")
	if basin == "" || accessToken == "" || stream == "" {
		t.Skip("S2_BASIN, S2_ACCESS_TOKEN, and S2_STREAM must be set to run this test")
	}

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker not available: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	c := NewTestContainer(t, headlessImage)
	require.NoError(t, c.Start(ctx, ContainerConfig{
		Env: map[string]string{
			"S2_BASIN":        basin,
			"S2_ACCESS_TOKEN": accessToken,
			"S2_STREAM":       stream,
		},
	}), "failed to start container")
	defer c.Stop(ctx)

	require.NoError(t, c.WaitReady(ctx), "api not ready")

	client, err := c.APIClient()
	require.NoError(t, err)

	// Start a telemetry session. The default config enables the system and
	// connection categories, which is what we publish into below.
	startResp, err := client.PutTelemetryWithResponse(ctx, instanceoapi.PutTelemetryJSONRequestBody{})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, startResp.StatusCode(), "put telemetry: %s", string(startResp.Body))

	// Publish a deterministic set of events across two enabled categories.
	const systemCount, connectionCount = 3, 2
	for i := 0; i < systemCount; i++ {
		publishEvent(t, ctx, client, "test.system", instanceoapi.PublishEventRequestCategorySystem)
	}
	for i := 0; i < connectionCount; i++ {
		publishEvent(t, ctx, client, "test.connection", instanceoapi.PublishEventRequestCategoryConnection)
	}

	// Give the storage writer time to flush to S2 (batcher linger + network).
	time.Sleep(2 * time.Second)

	// Bound every read tightly: a correct handler caps the S2 read at the tail,
	// so these return promptly. A hang here means the read is unbounded.
	readCtx, readCancel := context.WithTimeout(ctx, 10*time.Second)
	defer readCancel()

	// Full read returns at least everything we published.
	all, err := client.ReadTelemetryEventsWithResponse(readCtx, &instanceoapi.ReadTelemetryEventsParams{})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, all.StatusCode(), "read events: %s", string(all.Body))
	require.NotNil(t, all.JSON200)
	assert.GreaterOrEqual(t, len(all.JSON200.Events), systemCount+connectionCount)

	// Category filter returns only the requested category.
	systemCat := []instanceoapi.TelemetryEventCategory{instanceoapi.TelemetryEventCategorySystem}
	systemOnly, err := client.ReadTelemetryEventsWithResponse(readCtx, &instanceoapi.ReadTelemetryEventsParams{Category: &systemCat})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, systemOnly.StatusCode())
	require.NotNil(t, systemOnly.JSON200)
	assert.GreaterOrEqual(t, len(systemOnly.JSON200.Events), systemCount)
	for _, e := range systemOnly.JSON200.Events {
		require.NotNil(t, e.Event.Category)
		assert.Equal(t, instanceoapi.TelemetryEventCategorySystem, *e.Event.Category)
	}

	// Limit caps the number of returned events.
	limit := 1
	limited, err := client.ReadTelemetryEventsWithResponse(readCtx, &instanceoapi.ReadTelemetryEventsParams{Limit: &limit})
	require.NoError(t, err)
	require.NotNil(t, limited.JSON200)
	assert.Len(t, limited.JSON200.Events, 1)

	// An empty window returns [] not null, or the Python SDK chokes deserializing.
	pastSince, pastUntil := int64(1), int64(2)
	empty, err := client.ReadTelemetryEventsWithResponse(readCtx, &instanceoapi.ReadTelemetryEventsParams{Since: &pastSince, Until: &pastUntil})
	require.NoError(t, err)
	require.NotNil(t, empty.JSON200)
	assert.Empty(t, empty.JSON200.Events)
	assert.Contains(t, string(empty.Body), `"events":[]`)
}

func publishEvent(t *testing.T, ctx context.Context, client *instanceoapi.ClientWithResponses, eventType string, category instanceoapi.PublishEventRequestCategory) {
	t.Helper()
	resp, err := client.PublishTelemetryEventWithResponse(ctx, instanceoapi.PublishTelemetryEventJSONRequestBody{
		Type:     eventType,
		Category: &category,
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode(), "publish %s: %s", eventType, string(resp.Body))
}
