package recorder

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/onkernel/kernel-images/server/lib/scaletozero"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	mockBin = filepath.Join("testdata", "mock_ffmpeg.sh")
)

func defaultParams(tempDir string) FFmpegRecordingParams {
	fr := 5
	disp := 0
	size := 1
	return FFmpegRecordingParams{
		FrameRate:   &fr,
		DisplayNum:  &disp,
		MaxSizeInMB: &size,
		OutputDir:   &tempDir,
	}
}

func TestFFmpegRecorder_StartAndStop(t *testing.T) {
	tempDir := t.TempDir()
	rec := &FFmpegRecorder{
		id:         "startstop",
		binaryPath: mockBin,
		params:     defaultParams(tempDir),
		outputPath: filepath.Join(tempDir, "startstop.mp4"),
		stz:        scaletozero.NewOncer(scaletozero.NewNoopController()),
	}
	require.NoError(t, rec.Start(t.Context()))
	require.True(t, rec.IsRecording(t.Context()))

	time.Sleep(50 * time.Millisecond)

	// Stop proceeds even when ffmpeg exits with non-zero code (e.g., from signal),
	// then attempts finalization which will fail because mock doesn't create a file
	err := rec.Stop(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "recording file does not exist")

	<-rec.exited
	require.False(t, rec.IsRecording(t.Context()))
}

func TestFFmpegRecorder_Params(t *testing.T) {
	tempDir := t.TempDir()
	params := defaultParams(tempDir)
	rec := &FFmpegRecorder{
		id:         "params-test",
		binaryPath: mockBin,
		params:     params,
		outputPath: filepath.Join(tempDir, "params-test.mp4"),
		stz:        scaletozero.NewOncer(scaletozero.NewNoopController()),
	}

	got := rec.Params()
	assert.Equal(t, *params.FrameRate, *got.FrameRate)
	assert.Equal(t, *params.DisplayNum, *got.DisplayNum)
	assert.Equal(t, *params.MaxSizeInMB, *got.MaxSizeInMB)
	assert.Equal(t, *params.OutputDir, *got.OutputDir)
}

func TestFFmpegRecorder_ForceStop(t *testing.T) {
	tempDir := t.TempDir()
	rec := &FFmpegRecorder{
		id:         "forcestop",
		binaryPath: mockBin,
		params:     defaultParams(tempDir),
		outputPath: filepath.Join(tempDir, "forcestop.mp4"),
		stz:        scaletozero.NewOncer(scaletozero.NewNoopController()),
	}
	require.NoError(t, rec.Start(t.Context()))
	require.True(t, rec.IsRecording(t.Context()))

	time.Sleep(50 * time.Millisecond)

	// ForceStop logs a warning on finalization failure but doesn't return error
	err := rec.ForceStop(t.Context())
	require.NoError(t, err)

	<-rec.exited
	require.False(t, rec.IsRecording(t.Context()))
	assert.Contains(t, rec.cmd.ProcessState.String(), "killed")
}
