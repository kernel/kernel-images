package recorder

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/kernel/kernel-images/server/lib/scaletozero"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ffprobeChapters is the subset of `ffprobe -show_chapters -print_format json`
// we assert on.
type ffprobeChapters struct {
	Chapters []struct {
		TimeBase string `json:"time_base"`
		Start    int64  `json:"start"`
		End      int64  `json:"end"`
		Tags     struct {
			Title string `json:"title"`
		} `json:"tags"`
	} `json:"chapters"`
}

// TestFinalize_InjectsChaptersWithRealFFmpeg records a short synthetic clip with
// real ffmpeg, marks at known offsets, finalizes, and asserts via ffprobe that
// the chapters land at the expected millisecond offsets. It uses lavfi testsrc
// (not x11grab) so it needs no display, only ffmpeg/ffprobe on PATH.
func TestFinalize_InjectsChaptersWithRealFFmpeg(t *testing.T) {
	ffmpegBin, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skipf("ffmpeg not available: %v", err)
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skipf("ffprobe not available: %v", err)
	}

	const (
		fps          = 25
		durationSecs = 4
	)
	tempDir := t.TempDir()
	outputPath := filepath.Join(tempDir, "rec.mp4")

	// Produce a fragmented MP4 the same shape finalize expects to remux.
	genArgs := []string{
		"-f", "lavfi",
		"-i", "testsrc=size=320x240:rate=25",
		"-t", "4",
		"-c:v", "libx264",
		"-pix_fmt", "yuv420p",
		"-movflags", "+frag_keyframe+empty_moov",
		"-y", outputPath,
	}
	gen := exec.CommandContext(context.Background(), ffmpegBin, genArgs...)
	out, err := gen.CombinedOutput()
	require.NoError(t, err, "failed to generate test clip: %s", out)

	start := time.Now()
	rec := &FFmpegRecorder{
		id:         "finalize-integration",
		binaryPath: ffmpegBin,
		outputPath: outputPath,
		startTime:  start,
		endTime:    start.Add(durationSecs * time.Second),
		exitCode:   0,
		markers: []Marker{
			{Name: "intro", At: start.Add(1 * time.Second)},
			{Name: "middle", At: start.Add(2500 * time.Millisecond)},
		},
		stz: scaletozero.NewOncer(scaletozero.NewNoopController()),
	}

	require.NoError(t, rec.finalizeRecording(context.Background()))

	// The temp metadata file must be cleaned up after the remux.
	_, statErr := os.Stat(outputPath + ".ffmeta")
	assert.True(t, os.IsNotExist(statErr), "metadata temp file should be removed")

	probe := exec.CommandContext(context.Background(), "ffprobe",
		"-show_chapters", "-print_format", "json", "-v", "quiet", outputPath)
	probeOut, err := probe.Output()
	require.NoError(t, err)

	var parsed ffprobeChapters
	require.NoError(t, json.Unmarshal(probeOut, &parsed))
	require.Len(t, parsed.Chapters, 3, "sentinel + two markers")

	assert.Equal(t, "1/1000", parsed.Chapters[0].TimeBase)

	// One frame interval in ms; chapters should land within ~1.5 frames.
	const tolMs = int64(1000.0 / fps * 1.5)
	assertNear := func(name string, got, want int64) {
		assert.LessOrEqualf(t, abs(got-want), tolMs, "%s: got %dms want ~%dms", name, got, want)
	}

	assert.Equal(t, sentinelChapterName, parsed.Chapters[0].Tags.Title)
	assertNear("sentinel start", parsed.Chapters[0].Start, 0)
	assertNear("sentinel end", parsed.Chapters[0].End, 1000)

	assert.Equal(t, "intro", parsed.Chapters[1].Tags.Title)
	assertNear("intro start", parsed.Chapters[1].Start, 1000)
	assertNear("intro end", parsed.Chapters[1].End, 2500)

	assert.Equal(t, "middle", parsed.Chapters[2].Tags.Title)
	assertNear("middle start", parsed.Chapters[2].Start, 2500)
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
