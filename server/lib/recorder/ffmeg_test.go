package recorder

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/kernel/kernel-images/server/lib/scaletozero"
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
	audioSource := "KernelOutput.monitor"
	return FFmpegRecordingParams{
		FrameRate:   &fr,
		DisplayNum:  &disp,
		MaxSizeInMB: &size,
		OutputDir:   &tempDir,
		AudioSource: &audioSource,
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
	assert.Equal(t, *params.AudioSource, *got.AudioSource)
}

func TestFFmpegArgs_PadsOddDimensions(t *testing.T) {
	tempDir := t.TempDir()
	args, err := ffmpegArgs(defaultParams(tempDir), filepath.Join(tempDir, "out.mp4"))
	require.NoError(t, err)

	var vf string
	for i, a := range args {
		if a == "-vf" && i+1 < len(args) {
			vf = args[i+1]
			break
		}
	}
	assert.Equal(t, "pad=ceil(iw/2)*2:ceil(ih/2)*2", vf)
}

func TestFFmpegRecordingParams_ValidateAudioConfig(t *testing.T) {
	base := func() FFmpegRecordingParams {
		fr, disp, size := 5, 0, 1
		dir := t.TempDir()
		return FFmpegRecordingParams{FrameRate: &fr, DisplayNum: &disp, MaxSizeInMB: &size, OutputDir: &dir}
	}
	yes := true
	src := "KernelOutput.monitor"
	server := "unix:/tmp/pulse/native"

	t.Run("audio off is always valid", func(t *testing.T) {
		require.NoError(t, base().Validate())
	})
	t.Run("audio on with source and server is valid", func(t *testing.T) {
		p := base()
		p.RecordAudio = &yes
		p.AudioSource = &src
		p.PulseServer = &server
		require.NoError(t, p.Validate())
	})
	t.Run("audio on without source is rejected", func(t *testing.T) {
		p := base()
		p.RecordAudio = &yes
		p.PulseServer = &server
		require.Error(t, p.Validate())
	})
	t.Run("audio on without server is rejected", func(t *testing.T) {
		p := base()
		p.RecordAudio = &yes
		p.AudioSource = &src
		require.Error(t, p.Validate())
	})
}

func TestFFmpegArgs_IncludesPulseAudioWhenEnabled(t *testing.T) {
	tempDir := t.TempDir()
	params := defaultParams(tempDir)
	recordAudio := true
	pulseServer := "unix:/tmp/pulse/native"
	params.RecordAudio = &recordAudio
	params.PulseServer = &pulseServer

	args, err := ffmpegArgs(params, filepath.Join(tempDir, "out.mp4"))
	require.NoError(t, err)

	assert.Contains(t, args, "-f")
	assert.Contains(t, args, "pulse")
	assert.Contains(t, args, "KernelOutput.monitor")
	assert.Contains(t, args, "-map")
	assert.Contains(t, args, "1:a:0")
	assert.Contains(t, args, "-preset")
	assert.Contains(t, args, "veryfast")
	assert.Contains(t, args, "-tune")
	assert.Contains(t, args, "zerolatency")
	assert.Contains(t, args, "-c:a")
	assert.Contains(t, args, "aac")
	assert.NotContains(t, args, "aresample=async=1")
	assert.NotContains(t, args, "aresample=async=1:first_pts=0")
}

func TestFFmpegArgs_VideoOnlyKeepsLegacyFlags(t *testing.T) {
	tempDir := t.TempDir()

	// Video-only must stay identical to the pre-audio behavior: wall-clock
	// timestamps on, and none of the audio-path-only encoder/buffer flags.
	videoArgs, err := ffmpegArgs(defaultParams(tempDir), filepath.Join(tempDir, "v.mp4"))
	require.NoError(t, err)
	assert.Contains(t, videoArgs, "-use_wallclock_as_timestamps")
	assert.NotContains(t, videoArgs, "-thread_queue_size")
	assert.NotContains(t, videoArgs, "-preset")
	assert.NotContains(t, videoArgs, "-tune")

	// Recording audio drops wall-clock stamping (to keep the two inputs synced) and
	// adds the real-time encoder + buffer headroom flags.
	p := defaultParams(tempDir)
	recordAudio := true
	pulseServer := "unix:/tmp/pulse/native"
	p.RecordAudio = &recordAudio
	p.PulseServer = &pulseServer
	audioArgs, err := ffmpegArgs(p, filepath.Join(tempDir, "a.mp4"))
	require.NoError(t, err)
	assert.NotContains(t, audioArgs, "-use_wallclock_as_timestamps")
	assert.Contains(t, audioArgs, "-thread_queue_size")
	assert.Contains(t, audioArgs, "-preset")
	assert.Contains(t, audioArgs, "-tune")
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
