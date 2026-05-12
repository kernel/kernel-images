package e2e

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/s2-streamstore/s2-sdk-go/s2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	instanceoapi "github.com/kernel/kernel-images/server/lib/oapi"
)

// TestS2StorageWriter starts a headless container with S2 credentials, runs a
// capture session, and verifies that events land in the configured S2 stream.
//
// Skips automatically when S2_BASIN, S2_ACCESS_TOKEN, or S2_STREAM are unset.
func TestS2StorageWriter(t *testing.T) {
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

	// Note the current S2 stream tail seq before we write anything so we only
	// read records produced by this test run.
	s2Client := s2.New(accessToken, nil)
	streamClient := s2Client.Basin(basin).Stream(s2.StreamName(stream))

	checkResp, err := streamClient.CheckTail(ctx)
	require.NoError(t, err, "check tail before test")
	startSeq := checkResp.Tail.SeqNum

	// Start a capture session.
	startResp, err := client.StartCaptureSessionWithResponse(ctx, instanceoapi.StartCaptureSessionJSONRequestBody{})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, startResp.StatusCode(), "start capture session: %s", string(startResp.Body))
	require.NotNil(t, startResp.JSON201)
	sessionID := startResp.JSON201.Id
	t.Logf("capture session started: %s", sessionID)

	// Let the session run briefly so at least one event is published (the
	// session_started system event is emitted on session start).
	time.Sleep(500 * time.Millisecond)

	// Stop the capture session.
	stopResp, err := client.StopCaptureSessionWithResponse(ctx)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, stopResp.StatusCode(), "stop capture session: %s", string(stopResp.Body))
	t.Log("capture session stopped")

	// Give the storage writer time to flush to S2 (batcher linger + network).
	time.Sleep(2 * time.Second)

	// Read records written after the pre-test tail and verify at least one
	// envelope is present.
	readCtx, readCancel := context.WithTimeout(ctx, 10*time.Second)
	defer readCancel()

	readSession, err := streamClient.ReadSession(readCtx, &s2.ReadOptions{
		SeqNum: s2.Uint64(startSeq),
	})
	require.NoError(t, err, "open S2 read session")
	defer readSession.Close()

	var count int
	for readSession.Next() {
		count++
	}
	// EOF is expected once we reach the tail — not an error.
	if err := readSession.Err(); err != nil && readCtx.Err() == nil {
		t.Fatalf("S2 read session error: %v", err)
	}

	assert.Greater(t, count, 0, "expected at least one event record in S2 stream %q", stream)
	t.Logf("found %d record(s) in S2 stream after seq %d", count, startSeq)
}
