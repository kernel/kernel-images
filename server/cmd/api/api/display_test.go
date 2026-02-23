package api

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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
	t.Run("stops and deregisters active recording", func(t *testing.T) {
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
		assert.Equal(t, filepath.Join(tempDir, "test-rec.mp4"), stopped[0].outputPath)

		_, exists := mgr.GetRecorder("test-rec")
		assert.False(t, exists, "recorder should be deregistered after stop")
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

		stoppedIDs := map[string]bool{}
		for _, s := range stopped {
			stoppedIDs[s.id] = true
		}
		for _, id := range ids {
			assert.True(t, stoppedIDs[id], "recording %s should have been stopped", id)
			_, exists := mgr.GetRecorder(id)
			assert.False(t, exists, "recorder %s should be deregistered", id)
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

func TestRestartRecordings(t *testing.T) {
	t.Run("renames old file and starts new recording", func(t *testing.T) {
		ctx := context.Background()
		tempDir := t.TempDir()
		factory := testFFmpegFactory(t, tempDir)
		mgr := recorder.NewFFmpegManager()
		svc := newTestServiceWithFactory(t, mgr, factory)

		outputPath := filepath.Join(tempDir, "test-rec.mp4")
		require.NoError(t, os.WriteFile(outputPath, []byte("fake video data"), 0644))

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
			outputPath: outputPath,
		}

		svc.restartRecordings(ctx, []stoppedRecordingInfo{info})

		entries, err := os.ReadDir(tempDir)
		require.NoError(t, err)

		foundRenamed := false
		for _, e := range entries {
			if strings.Contains(e.Name(), "before-resize") {
				foundRenamed = true
				data, readErr := os.ReadFile(filepath.Join(tempDir, e.Name()))
				require.NoError(t, readErr)
				assert.Equal(t, []byte("fake video data"), data)
			}
		}
		assert.True(t, foundRenamed, "pre-resize recording should be preserved with renamed file")

		rec, exists := mgr.GetRecorder("test-rec")
		require.True(t, exists, "restarted recorder should be registered")
		assert.True(t, rec.IsRecording(ctx), "restarted recorder should be recording")

		_ = rec.Stop(ctx)
	})

	t.Run("starts recording even when no old file exists", func(t *testing.T) {
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
			outputPath: filepath.Join(tempDir, "fresh-rec.mp4"),
		}

		svc.restartRecordings(ctx, []stoppedRecordingInfo{info})

		rec, exists := mgr.GetRecorder("fresh-rec")
		require.True(t, exists, "recorder should be registered")
		assert.True(t, rec.IsRecording(ctx))

		_ = rec.Stop(ctx)
	})
}

func TestStopAndRestartRecordings_RoundTrip(t *testing.T) {
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

	// Stop all active recordings
	stopped, err := svc.stopActiveRecordings(ctx)
	require.NoError(t, err)
	require.Len(t, stopped, 1)
	assert.Equal(t, "round-trip", stopped[0].id)

	// Verify the recorder was deregistered
	_, exists := mgr.GetRecorder("round-trip")
	require.False(t, exists)

	// Restart recordings
	svc.restartRecordings(ctx, stopped)

	// Verify the recording resumed with the same ID
	newRec, exists := mgr.GetRecorder("round-trip")
	require.True(t, exists, "recorder should be re-registered after restart")
	assert.True(t, newRec.IsRecording(ctx), "recorder should be actively recording")

	_ = newRec.Stop(ctx)
}
