package api

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/onkernel/kernel-images/server/lib/recorder"
	"github.com/onkernel/kernel-images/server/lib/scaletozero"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testMockFFmpegBin = filepath.Join("..", "..", "..", "lib", "recorder", "testdata", "mock_ffmpeg.sh")

func testFFmpegFactory(t *testing.T, tempDir string) recorder.FFmpegRecorderFactory {
	t.Helper()
	fr := 5
	disp := 0
	size := 1
	config := recorder.FFmpegRecordingParams{
		FrameRate:   &fr,
		DisplayNum:  &disp,
		MaxSizeInMB: &size,
		OutputDir:   &tempDir,
	}
	return recorder.NewFFmpegRecorderFactory(testMockFFmpegBin, config, scaletozero.NewNoopController())
}

func newTestServiceWithFactory(t *testing.T, mgr recorder.RecordManager, factory recorder.FFmpegRecorderFactory) *ApiService {
	t.Helper()
	svc, err := New(mgr, factory, newTestUpstreamManager(), scaletozero.NewNoopController(), newMockNekoClient(t))
	require.NoError(t, err)
	return svc
}

func TestStopActiveRecordings(t *testing.T) {
	t.Run("stops recording but keeps it registered", func(t *testing.T) {
		ctx := context.Background()
		tempDir := t.TempDir()
		factory := testFFmpegFactory(t, tempDir)
		mgr := recorder.NewFFmpegManager()
		svc := newTestServiceWithFactory(t, mgr, factory)

		rec, err := factory("test-rec", recorder.FFmpegRecordingParams{})
		require.NoError(t, err)
		require.NoError(t, mgr.RegisterRecorder(ctx, rec))
		require.NoError(t, rec.Start(ctx))
		time.Sleep(50 * time.Millisecond)
		require.True(t, rec.IsRecording(ctx))

		stopped, err := svc.stopActiveRecordings(ctx)
		require.NoError(t, err)
		require.Len(t, stopped, 1)
		assert.Equal(t, "test-rec", stopped[0].id)
		assert.NotNil(t, stopped[0].params.FrameRate)

		oldRec, exists := mgr.GetRecorder("test-rec")
		assert.True(t, exists, "old recorder should remain registered")
		assert.False(t, oldRec.IsRecording(ctx), "old recorder should be stopped")
	})

	t.Run("stops multiple active recordings", func(t *testing.T) {
		ctx := context.Background()
		tempDir := t.TempDir()
		factory := testFFmpegFactory(t, tempDir)
		mgr := recorder.NewFFmpegManager()
		svc := newTestServiceWithFactory(t, mgr, factory)

		ids := []string{"rec-a", "rec-b"}
		for _, id := range ids {
			rec, err := factory(id, recorder.FFmpegRecordingParams{})
			require.NoError(t, err)
			require.NoError(t, mgr.RegisterRecorder(ctx, rec))
			require.NoError(t, rec.Start(ctx))
		}
		time.Sleep(50 * time.Millisecond)

		stopped, err := svc.stopActiveRecordings(ctx)
		require.NoError(t, err)
		assert.Len(t, stopped, 2)

		for _, id := range ids {
			oldRec, exists := mgr.GetRecorder(id)
			assert.True(t, exists, "recorder %s should remain registered", id)
			assert.False(t, oldRec.IsRecording(ctx), "recorder %s should be stopped", id)
		}
	})

	t.Run("skips non-recording recorders", func(t *testing.T) {
		ctx := context.Background()
		tempDir := t.TempDir()
		factory := testFFmpegFactory(t, tempDir)
		mgr := recorder.NewFFmpegManager()
		svc := newTestServiceWithFactory(t, mgr, factory)

		mock := &mockRecorder{id: "idle-rec", isRecordingFlag: false}
		require.NoError(t, mgr.RegisterRecorder(ctx, mock))

		stopped, err := svc.stopActiveRecordings(ctx)
		require.NoError(t, err)
		assert.Empty(t, stopped)

		_, exists := mgr.GetRecorder("idle-rec")
		assert.True(t, exists, "non-recording recorder should remain registered")
	})

	t.Run("returns empty when no recorders exist", func(t *testing.T) {
		ctx := context.Background()
		tempDir := t.TempDir()
		factory := testFFmpegFactory(t, tempDir)
		mgr := recorder.NewFFmpegManager()
		svc := newTestServiceWithFactory(t, mgr, factory)

		stopped, err := svc.stopActiveRecordings(ctx)
		require.NoError(t, err)
		assert.Empty(t, stopped)
	})
}

func TestStartNewRecordingSegments(t *testing.T) {
	t.Run("creates new segment with suffixed ID", func(t *testing.T) {
		ctx := context.Background()
		tempDir := t.TempDir()
		factory := testFFmpegFactory(t, tempDir)
		mgr := recorder.NewFFmpegManager()
		svc := newTestServiceWithFactory(t, mgr, factory)

		fr := 5
		disp := 0
		size := 1
		info := stoppedRecordingInfo{
			id: "test-rec",
			params: recorder.FFmpegRecordingParams{
				FrameRate:   &fr,
				DisplayNum:  &disp,
				MaxSizeInMB: &size,
				OutputDir:   &tempDir,
			},
		}

		svc.startNewRecordingSegments(ctx, []stoppedRecordingInfo{info})

		// The new recorder should have a suffixed ID, not the original
		_, existsOld := mgr.GetRecorder("test-rec")
		assert.False(t, existsOld, "original ID should not be re-registered")

		// Find the new segment by iterating active recorders
		var newRec recorder.Recorder
		for _, r := range mgr.ListActiveRecorders(ctx) {
			if r.IsRecording(ctx) {
				newRec = r
				break
			}
		}
		require.NotNil(t, newRec, "a new recording segment should be active")
		assert.Contains(t, newRec.ID(), "test-rec-", "new ID should be prefixed with the original ID")
		assert.True(t, newRec.IsRecording(ctx))

		_ = newRec.Stop(ctx)
	})

	t.Run("starts segment even when no old recorder exists in manager", func(t *testing.T) {
		ctx := context.Background()
		tempDir := t.TempDir()
		factory := testFFmpegFactory(t, tempDir)
		mgr := recorder.NewFFmpegManager()
		svc := newTestServiceWithFactory(t, mgr, factory)

		fr := 5
		disp := 0
		size := 1
		info := stoppedRecordingInfo{
			id: "fresh-rec",
			params: recorder.FFmpegRecordingParams{
				FrameRate:   &fr,
				DisplayNum:  &disp,
				MaxSizeInMB: &size,
				OutputDir:   &tempDir,
			},
		}

		svc.startNewRecordingSegments(ctx, []stoppedRecordingInfo{info})

		var newRec recorder.Recorder
		for _, r := range mgr.ListActiveRecorders(ctx) {
			if r.IsRecording(ctx) {
				newRec = r
				break
			}
		}
		require.NotNil(t, newRec, "new segment should be active")
		assert.Contains(t, newRec.ID(), "fresh-rec-")

		_ = newRec.Stop(ctx)
	})
}

func TestStopAndStartNewSegment_RoundTrip(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()
	factory := testFFmpegFactory(t, tempDir)
	mgr := recorder.NewFFmpegManager()
	svc := newTestServiceWithFactory(t, mgr, factory)

	// Start a recording
	rec, err := factory("round-trip", recorder.FFmpegRecordingParams{})
	require.NoError(t, err)
	require.NoError(t, mgr.RegisterRecorder(ctx, rec))
	require.NoError(t, rec.Start(ctx))
	time.Sleep(50 * time.Millisecond)
	require.True(t, rec.IsRecording(ctx))

	// Stop active recordings (simulating resize)
	stopped, err := svc.stopActiveRecordings(ctx)
	require.NoError(t, err)
	require.Len(t, stopped, 1)
	assert.Equal(t, "round-trip", stopped[0].id)

	// Old recorder should still be registered but stopped
	oldRec, exists := mgr.GetRecorder("round-trip")
	require.True(t, exists, "old recorder should remain registered")
	assert.False(t, oldRec.IsRecording(ctx), "old recorder should be stopped")

	// Start new segments
	svc.startNewRecordingSegments(ctx, stopped)

	// Old recorder should still be there
	oldRec2, exists := mgr.GetRecorder("round-trip")
	require.True(t, exists, "old recorder should still be registered after new segment starts")
	assert.False(t, oldRec2.IsRecording(ctx))

	// New recorder should be active with a different ID
	var newRec recorder.Recorder
	for _, r := range mgr.ListActiveRecorders(ctx) {
		if r.ID() != "round-trip" && r.IsRecording(ctx) {
			newRec = r
			break
		}
	}
	require.NotNil(t, newRec, "new segment recorder should exist")
	assert.Contains(t, newRec.ID(), "round-trip-", "new ID should be suffixed")
	assert.True(t, newRec.IsRecording(ctx))

	_ = newRec.Stop(ctx)
}
