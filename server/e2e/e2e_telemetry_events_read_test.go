package e2e

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	instanceoapi "github.com/kernel/kernel-images/server/lib/oapi"
)

// s2FlushWait is how long to wait after publishing for the S2 storage writer to
// flush to the durable stream (batcher linger of 100ms plus network), before
// reading the archive back.
const s2FlushWait = 2 * time.Second

// startTelemetryReadContainer boots a headless container wired to S2 on a
// per-test stream (so tests sharing the S2_STREAM env don't pollute each other)
// and starts a telemetry session. Skips when S2 creds or docker are absent.
func startTelemetryReadContainer(t *testing.T, ctx context.Context) *instanceoapi.ClientWithResponses {
	t.Helper()
	basin := os.Getenv("S2_BASIN")
	accessToken := os.Getenv("S2_ACCESS_TOKEN")
	stream := os.Getenv("S2_STREAM")
	if basin == "" || accessToken == "" || stream == "" {
		t.Skip("S2_BASIN, S2_ACCESS_TOKEN, and S2_STREAM must be set to run this test")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker not available: %v", err)
	}

	c := NewTestContainer(t, headlessImage)
	require.NoError(t, c.Start(ctx, ContainerConfig{
		Env: map[string]string{
			"S2_BASIN":        basin,
			"S2_ACCESS_TOKEN": accessToken,
			"S2_STREAM":       fmt.Sprintf("%s-%s", stream, t.Name()),
		},
	}), "failed to start container")
	t.Cleanup(func() { c.Stop(context.Background()) })

	require.NoError(t, c.WaitReady(ctx), "api not ready")

	client, err := c.APIClient()
	require.NoError(t, err)

	startResp, err := client.PutTelemetryWithResponse(ctx, instanceoapi.PutTelemetryJSONRequestBody{})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, startResp.StatusCode(), "put telemetry: %s", string(startResp.Body))
	return client
}

// TestReadTelemetryEvents publishes a known set of events and reads them back
// through GET /telemetry/events against a real S2 stream, exercising the full
// archive read path rather than the in-memory ring buffer.
//
// Skips automatically when S2_BASIN, S2_ACCESS_TOKEN, or S2_STREAM are unset.
func TestReadTelemetryEvents(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	client := startTelemetryReadContainer(t, ctx)

	// Publish a deterministic set of events across two enabled categories.
	const systemCount, connectionCount = 3, 2
	for i := 0; i < systemCount; i++ {
		publishEvent(t, ctx, client, "test.system", instanceoapi.TelemetryEventCategorySystem)
	}
	for i := 0; i < connectionCount; i++ {
		publishEvent(t, ctx, client, "test.connection", instanceoapi.TelemetryEventCategoryConnection)
	}

	// Give the storage writer time to flush to S2 (batcher linger + network).
	time.Sleep(s2FlushWait)

	// Bound every read tightly: a correct handler caps the S2 read, so these
	// return promptly. A hang here means the read is unbounded.
	readCtx, readCancel := context.WithTimeout(ctx, 10*time.Second)
	defer readCancel()

	// Full read returns at least everything we published.
	all, _, _ := readEventsPage(t, readCtx, client, &instanceoapi.ReadTelemetryEventsParams{})
	assert.GreaterOrEqual(t, len(all), systemCount+connectionCount)

	// Category filter returns only the requested category.
	systemCat := []instanceoapi.TelemetryEventCategory{instanceoapi.TelemetryEventCategorySystem}
	systemOnly, _, _ := readEventsPage(t, readCtx, client, &instanceoapi.ReadTelemetryEventsParams{Category: &systemCat})
	assert.GreaterOrEqual(t, len(systemOnly), systemCount)
	for _, e := range systemOnly {
		require.NotNil(t, e.Event.Category)
		assert.Equal(t, instanceoapi.TelemetryEventCategorySystem, *e.Event.Category)
	}

	// An empty window returns [] (not null) with no cursor.
	pastSince, pastUntil := "2000-01-01T00:00:00Z", "2000-01-02T00:00:00Z"
	empty, err := client.ReadTelemetryEventsWithResponse(readCtx, &instanceoapi.ReadTelemetryEventsParams{Since: &pastSince, Until: &pastUntil})
	require.NoError(t, err)
	require.NotNil(t, empty.JSON200)
	assert.Empty(t, *empty.JSON200)
	assert.JSONEq(t, `[]`, string(empty.Body), "empty result must be [] not null")
	assert.Equal(t, "false", empty.HTTPResponse.Header.Get("X-Has-More"))
}

// TestReadTelemetryEventsPagination publishes more events than the page size and
// walks every page via the X-Next-Offset cursor, asserting the full set comes
// back exactly once in ascending order.
func TestReadTelemetryEventsPagination(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	client := startTelemetryReadContainer(t, ctx)

	const published = 5
	for i := 0; i < published; i++ {
		publishEvent(t, ctx, client, "test.system", instanceoapi.TelemetryEventCategorySystem)
	}
	time.Sleep(s2FlushWait)

	readCtx, readCancel := context.WithTimeout(ctx, 20*time.Second)
	defer readCancel()

	const pageLimit = 2
	var collected []instanceoapi.TelemetryEnvelope
	var offset *int64
	pages := 0
	for {
		pages++
		require.LessOrEqual(t, pages, 50, "pagination did not terminate")
		limit := pageLimit
		page, hasMore, next := readEventsPage(t, readCtx, client, &instanceoapi.ReadTelemetryEventsParams{Limit: &limit, Offset: offset})
		require.LessOrEqual(t, len(page), pageLimit, "a page must not exceed the limit")
		collected = append(collected, page...)
		if !hasMore {
			break
		}
		offset = &next
	}

	require.GreaterOrEqual(t, pages, 3, "5 events at limit 2 should span multiple pages")
	require.GreaterOrEqual(t, len(collected), published)
	// Strictly ascending seqs prove the cursor neither skips nor re-reads across
	// page boundaries.
	for i := 1; i < len(collected); i++ {
		assert.Greater(t, collected[i].Seq, collected[i-1].Seq, "events must be strictly ascending with no dupes across pages")
	}

	// Reads must be side-effect-free: reading telemetry must not emit an api_call
	// event back into the stream, or pagination could never catch the tail. Two
	// full reads must return the same count.
	first, _, _ := readEventsPage(t, readCtx, client, &instanceoapi.ReadTelemetryEventsParams{})
	second, _, _ := readEventsPage(t, readCtx, client, &instanceoapi.ReadTelemetryEventsParams{})
	assert.Equal(t, len(first), len(second), "a read must not append to the stream it reads")
}

// TestReadTelemetryEventsFilteredPagination verifies that paginating with a
// category filter still returns the complete matching set even when intermediate
// pages come back empty. The filter is applied after the cursor-bounded read, so
// a page can be empty while X-Has-More is true; a correct client follows the
// cursor rather than stopping on an empty page.
func TestReadTelemetryEventsFilteredPagination(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	client := startTelemetryReadContainer(t, ctx)

	// A run of connection events (each also emits a control api_call), then a few
	// system events at the tail. Filtering for system means the early pages, which
	// scan only connection/control records, come back empty with more remaining.
	const systemCount = 3
	for i := 0; i < 15; i++ {
		publishEvent(t, ctx, client, "test.connection", instanceoapi.TelemetryEventCategoryConnection)
	}
	for i := 0; i < systemCount; i++ {
		publishEvent(t, ctx, client, "test.system", instanceoapi.TelemetryEventCategorySystem)
	}
	time.Sleep(s2FlushWait)

	readCtx, readCancel := context.WithTimeout(ctx, 20*time.Second)
	defer readCancel()

	systemCat := []instanceoapi.TelemetryEventCategory{instanceoapi.TelemetryEventCategorySystem}
	const pageLimit = 2
	var collected []instanceoapi.TelemetryEnvelope
	var offset *int64
	pages, emptyPages := 0, 0
	for {
		pages++
		require.LessOrEqual(t, pages, 100, "filtered pagination did not terminate")
		limit := pageLimit
		page, hasMore, next := readEventsPage(t, readCtx, client, &instanceoapi.ReadTelemetryEventsParams{Limit: &limit, Category: &systemCat, Offset: offset})
		if len(page) == 0 {
			emptyPages++
		}
		for _, e := range page {
			require.NotNil(t, e.Event.Category)
			assert.Equal(t, instanceoapi.TelemetryEventCategorySystem, *e.Event.Category)
		}
		collected = append(collected, page...)
		if !hasMore {
			break
		}
		offset = &next
	}

	assert.GreaterOrEqual(t, len(collected), systemCount, "cursor walk must collect every matching event across empty pages")
	assert.Positive(t, emptyPages, "the connection run should yield empty system-filtered pages (the sparse-filter edge)")
}

// TestReadTelemetryEventsWindowedPagination verifies that an until-bounded read
// terminates even when the stream extends past the window: it returns the
// in-window events and stops, rather than chasing the physical tail.
func TestReadTelemetryEventsWindowedPagination(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	client := startTelemetryReadContainer(t, ctx)

	// First batch is in-window; capture the boundary; a second batch lands after
	// it, so the physical tail sits past `until`.
	const inWindow = 6
	for i := 0; i < inWindow; i++ {
		publishEvent(t, ctx, client, "test.system", instanceoapi.TelemetryEventCategorySystem)
	}
	time.Sleep(s2FlushWait)
	until := time.Now().UTC().Format(time.RFC3339Nano)
	for i := 0; i < 6; i++ {
		publishEvent(t, ctx, client, "test.system", instanceoapi.TelemetryEventCategorySystem)
	}
	time.Sleep(s2FlushWait)

	readCtx, readCancel := context.WithTimeout(ctx, 20*time.Second)
	defer readCancel()

	// Page the window; it must terminate via X-Has-More=false rather than walk
	// page after page toward the tail that lives past `until`.
	const pageLimit = 2
	var collected []instanceoapi.TelemetryEnvelope
	var offset *int64
	pages := 0
	for {
		pages++
		require.LessOrEqual(t, pages, 50, "windowed pagination did not terminate")
		limit := pageLimit
		page, hasMore, next := readEventsPage(t, readCtx, client, &instanceoapi.ReadTelemetryEventsParams{Limit: &limit, Until: &until, Offset: offset})
		collected = append(collected, page...)
		if !hasMore {
			break
		}
		offset = &next
	}

	// The in-window events (and their api_call pairs) come back; the run
	// terminates without chasing the post-until tail.
	assert.GreaterOrEqual(t, len(collected), inWindow)
}

// readEventsPage calls the endpoint and returns the page plus its cursor state.
func readEventsPage(t *testing.T, ctx context.Context, client *instanceoapi.ClientWithResponses, params *instanceoapi.ReadTelemetryEventsParams) (page []instanceoapi.TelemetryEnvelope, hasMore bool, next int64) {
	t.Helper()
	resp, err := client.ReadTelemetryEventsWithResponse(ctx, params)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode(), "read events: %s", string(resp.Body))
	require.NotNil(t, resp.JSON200)

	hasMore = resp.HTTPResponse.Header.Get("X-Has-More") == "true"
	if hasMore {
		nextStr := resp.HTTPResponse.Header.Get("X-Next-Offset")
		require.NotEmpty(t, nextStr, "X-Next-Offset must be set when X-Has-More is true")
		next, err = strconv.ParseInt(nextStr, 10, 64)
		require.NoError(t, err)
	}
	return *resp.JSON200, hasMore, next
}

func publishEvent(t *testing.T, ctx context.Context, client *instanceoapi.ClientWithResponses, eventType string, category instanceoapi.TelemetryEventCategory) {
	t.Helper()
	resp, err := client.PublishTelemetryEventWithResponse(ctx, instanceoapi.PublishTelemetryEventJSONRequestBody{
		Type:     eventType,
		Category: &category,
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode(), "publish %s: %s", eventType, string(resp.Body))
}
