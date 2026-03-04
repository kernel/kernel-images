package api

import (
	"context"
	"log/slog"
	"os"
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
		require.NotNil(t, stopped[0].metadata, "metadata should be captured")
		assert.False(t, stopped[0].metadata.StartTime.IsZero(), "start time should be set")

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

func TestAdjustParamsForRemainingBudget(t *testing.T) {
	log := slog.Default()

	t.Run("reduces MaxDurationInSeconds by elapsed time", func(t *testing.T) {
		maxDur := 60
		fr := 5
		disp := 0
		size := 500
		dir := t.TempDir()

		info := stoppedRecordingInfo{
			id: "dur-test",
			params: recorder.FFmpegRecordingParams{
				FrameRate:            &fr,
				DisplayNum:           &disp,
				MaxSizeInMB:          &size,
				MaxDurationInSeconds: &maxDur,
				OutputDir:            &dir,
			},
			metadata: &recorder.RecordingMetadata{
				StartTime: time.Now().Add(-25 * time.Second),
				EndTime:   time.Now(),
			},
		}

		adjusted := adjustParamsForRemainingBudget(log, info)
		require.NotNil(t, adjusted.MaxDurationInSeconds)
		assert.InDelta(t, 35, *adjusted.MaxDurationInSeconds, 2, "remaining duration should be ~35s")
		assert.Equal(t, 60, maxDur, "original param should not be mutated")
	})

	t.Run("clamps remaining duration to 1 when budget exhausted", func(t *testing.T) {
		maxDur := 10
		fr := 5
		disp := 0
		size := 500
		dir := t.TempDir()

		info := stoppedRecordingInfo{
			id: "exhausted-dur",
			params: recorder.FFmpegRecordingParams{
				FrameRate:            &fr,
				DisplayNum:           &disp,
				MaxSizeInMB:          &size,
				MaxDurationInSeconds: &maxDur,
				OutputDir:            &dir,
			},
			metadata: &recorder.RecordingMetadata{
				StartTime: time.Now().Add(-30 * time.Second),
				EndTime:   time.Now(),
			},
		}

		adjusted := adjustParamsForRemainingBudget(log, info)
		require.NotNil(t, adjusted.MaxDurationInSeconds)
		assert.Equal(t, 1, *adjusted.MaxDurationInSeconds)
	})

	t.Run("reduces MaxSizeInMB by consumed file size", func(t *testing.T) {
		maxSize := 10
		fr := 5
		disp := 0
		dir := t.TempDir()

		segmentFile := filepath.Join(dir, "size-test.mp4")
		data := make([]byte, 3*1024*1024) // 3 MB
		require.NoError(t, os.WriteFile(segmentFile, data, 0644))

		info := stoppedRecordingInfo{
			id: "size-test",
			params: recorder.FFmpegRecordingParams{
				FrameRate:   &fr,
				DisplayNum:  &disp,
				MaxSizeInMB: &maxSize,
				OutputDir:   &dir,
			},
			metadata: &recorder.RecordingMetadata{},
		}

		adjusted := adjustParamsForRemainingBudget(log, info)
		require.NotNil(t, adjusted.MaxSizeInMB)
		assert.Equal(t, 7, *adjusted.MaxSizeInMB)
		assert.Equal(t, 10, maxSize, "original param should not be mutated")
	})

	t.Run("clamps remaining size to 1 when budget exhausted", func(t *testing.T) {
		maxSize := 2
		fr := 5
		disp := 0
		dir := t.TempDir()

		segmentFile := filepath.Join(dir, "big-test.mp4")
		data := make([]byte, 5*1024*1024) // 5 MB > 2 MB limit
		require.NoError(t, os.WriteFile(segmentFile, data, 0644))

		info := stoppedRecordingInfo{
			id: "big-test",
			params: recorder.FFmpegRecordingParams{
				FrameRate:   &fr,
				DisplayNum:  &disp,
				MaxSizeInMB: &maxSize,
				OutputDir:   &dir,
			},
			metadata: &recorder.RecordingMetadata{},
		}

		adjusted := adjustParamsForRemainingBudget(log, info)
		require.NotNil(t, adjusted.MaxSizeInMB)
		assert.Equal(t, 1, *adjusted.MaxSizeInMB)
	})

	t.Run("rounds up fractional MB when computing consumed size", func(t *testing.T) {
		maxSize := 10
		fr := 5
		disp := 0
		dir := t.TempDir()

		segmentFile := filepath.Join(dir, "frac-test.mp4")
		data := make([]byte, 3*1024*1024+512*1024) // 3.5 MB → rounds up to 4 MB consumed
		require.NoError(t, os.WriteFile(segmentFile, data, 0644))

		info := stoppedRecordingInfo{
			id: "frac-test",
			params: recorder.FFmpegRecordingParams{
				FrameRate:   &fr,
				DisplayNum:  &disp,
				MaxSizeInMB: &maxSize,
				OutputDir:   &dir,
			},
			metadata: &recorder.RecordingMetadata{},
		}

		adjusted := adjustParamsForRemainingBudget(log, info)
		require.NotNil(t, adjusted.MaxSizeInMB)
		assert.Equal(t, 6, *adjusted.MaxSizeInMB) // 10 - 4 = 6
	})

	t.Run("no adjustment when limits are nil", func(t *testing.T) {
		fr := 5
		disp := 0
		size := 500
		dir := t.TempDir()

		info := stoppedRecordingInfo{
			id: "no-limits",
			params: recorder.FFmpegRecordingParams{
				FrameRate:   &fr,
				DisplayNum:  &disp,
				MaxSizeInMB: &size,
				OutputDir:   &dir,
			},
			metadata: &recorder.RecordingMetadata{
				StartTime: time.Now().Add(-10 * time.Second),
				EndTime:   time.Now(),
			},
		}

		adjusted := adjustParamsForRemainingBudget(log, info)
		assert.Nil(t, adjusted.MaxDurationInSeconds, "should remain nil when not set")
	})
}
