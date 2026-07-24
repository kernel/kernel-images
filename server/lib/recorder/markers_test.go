package recorder

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kernel/kernel-images/server/lib/scaletozero"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func readMeta(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(b)
}

func TestBuildChapterMetadata_SentinelAndOrdering(t *testing.T) {
	start := time.Unix(1000, 0)
	out := filepath.Join(t.TempDir(), "rec.mp4")
	markers := []Marker{
		{Name: "second", At: start.Add(5 * time.Second)},
		{Name: "first", At: start.Add(2 * time.Second)},
	}

	path, ok, err := buildChapterMetadata(out, markers, start, 10000)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, out+".ffmeta", path)

	meta := readMeta(t, path)
	assert.True(t, strings.HasPrefix(meta, "FFMETADATA1\n"), "must start with the ffmetadata header")

	// Sentinel covers [0, firstRealStart); markers are sorted ascending and the
	// final chapter ends at durationMs.
	expected := "FFMETADATA1\n" +
		"[CHAPTER]\nTIMEBASE=1/1000\nSTART=0\nEND=2000\ntitle=" + sentinelChapterName + "\n" +
		"[CHAPTER]\nTIMEBASE=1/1000\nSTART=2000\nEND=5000\ntitle=first\n" +
		"[CHAPTER]\nTIMEBASE=1/1000\nSTART=5000\nEND=10000\ntitle=second\n"
	assert.Equal(t, expected, meta)
}

func TestBuildChapterMetadata_ClampsNegativeOffsetsToZero(t *testing.T) {
	start := time.Unix(1000, 0)
	out := filepath.Join(t.TempDir(), "rec.mp4")
	markers := []Marker{
		{Name: "before-start", At: start.Add(-3 * time.Second)},
		{Name: "kept", At: start.Add(4 * time.Second)},
	}

	path, ok, err := buildChapterMetadata(out, markers, start, 8000)
	require.NoError(t, err)
	require.True(t, ok)

	// The early marker is clamped to media t=0 instead of silently dropped,
	// and takes the sentinel's place as the first chapter.
	meta := readMeta(t, path)
	assert.Contains(t, meta, "START=0\nEND=4000\ntitle=before-start")
	assert.Contains(t, meta, "START=4000\nEND=8000\ntitle=kept")
	assert.NotContains(t, meta, sentinelChapterName)
	assert.Equal(t, 2, strings.Count(meta, "[CHAPTER]"))
}

func TestStart_RemovesStaleOutput(t *testing.T) {
	tempDir := t.TempDir()
	outputPath := filepath.Join(tempDir, "stale.mp4")
	require.NoError(t, os.WriteFile(outputPath, []byte("leftover from a previous run"), 0o644))

	rec := &FFmpegRecorder{
		id:         "stale-output",
		binaryPath: mockBin,
		params:     defaultParams(tempDir),
		outputPath: outputPath,
		stz:        scaletozero.NewOncer(scaletozero.NewNoopController()),
	}
	require.NoError(t, rec.Start(t.Context()))
	t.Cleanup(func() { _ = rec.ForceStop(t.Context()) })

	// The stale file is gone (the mock ffmpeg never writes one), so the
	// capture anchor cannot be stamped from leftover bytes.
	_, statErr := os.Stat(outputPath)
	assert.True(t, os.IsNotExist(statErr), "stale output should be removed at start")
	time.Sleep(50 * time.Millisecond)
	rec.mu.Lock()
	assert.True(t, rec.captureAnchor.IsZero(), "anchor must not come from stale bytes")
	rec.mu.Unlock()
}

func TestBuildChapterMetadata_EscapesSpecialChars(t *testing.T) {
	start := time.Unix(1000, 0)
	out := filepath.Join(t.TempDir(), "rec.mp4")
	markers := []Marker{
		{Name: "a=b;c#d\\e", At: start.Add(1 * time.Second)},
	}

	path, ok, err := buildChapterMetadata(out, markers, start, 4000)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Contains(t, readMeta(t, path), `title=a\=b\;c\#d\\e`)
}

func TestBuildChapterMetadata_ClampsMarkersPastDurationToEnd(t *testing.T) {
	start := time.Unix(1000, 0)
	out := filepath.Join(t.TempDir(), "rec.mp4")
	markers := []Marker{
		{Name: "kept", At: start.Add(1 * time.Second)},
		{Name: "past-duration", At: start.Add(9 * time.Second)},
	}

	path, ok, err := buildChapterMetadata(out, markers, start, 4000)
	require.NoError(t, err)
	require.True(t, ok)

	// A marker timestamped past the media end (e.g. marked during the stop
	// drain) is clamped to a zero-length chapter at the end rather than
	// silently lost: Mark already returned 201 for it.
	meta := readMeta(t, path)
	assert.Contains(t, meta, "START=1000\nEND=4000\ntitle=kept")
	assert.Contains(t, meta, "START=4000\nEND=4000\ntitle=past-duration")
	// Sentinel + the two markers.
	assert.Equal(t, 3, strings.Count(meta, "[CHAPTER]"))
}

func TestBuildChapterMetadata_NonPositiveDurationFallsBack(t *testing.T) {
	start := time.Unix(1000, 0)
	out := filepath.Join(t.TempDir(), "rec.mp4")
	markers := []Marker{{Name: "m", At: start.Add(1 * time.Second)}}

	for _, durationMs := range []int64{0, -250} {
		path, ok, err := buildChapterMetadata(out, markers, start, durationMs)
		require.NoError(t, err)
		assert.False(t, ok, "duration %d must fall back to the plain remux", durationMs)
		assert.Empty(t, path)
	}
}

func TestBuildChapterMetadata_NoUsableMarkers(t *testing.T) {
	start := time.Unix(1000, 0)
	out := filepath.Join(t.TempDir(), "rec.mp4")

	path, ok, err := buildChapterMetadata(out, nil, start, 4000)
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Empty(t, path)
	_, statErr := os.Stat(out + ".ffmeta")
	assert.True(t, os.IsNotExist(statErr), "no metadata file should be written")
}

func TestBuildChapterMetadata_NoSentinelWhenFirstAtZero(t *testing.T) {
	start := time.Unix(1000, 0)
	out := filepath.Join(t.TempDir(), "rec.mp4")
	markers := []Marker{
		{Name: "at-zero", At: start},
	}

	path, ok, err := buildChapterMetadata(out, markers, start, 4000)
	require.NoError(t, err)
	require.True(t, ok)
	meta := readMeta(t, path)
	assert.NotContains(t, meta, sentinelChapterName)
	assert.Equal(t, 1, strings.Count(meta, "[CHAPTER]"))
	assert.Contains(t, meta, "START=0\nEND=4000\ntitle=at-zero")
}

func TestMark_NotRecording(t *testing.T) {
	rec := &FFmpegRecorder{
		id:       "mark-idle",
		exitCode: exitCodeInitValue,
		stz:      scaletozero.NewOncer(scaletozero.NewNoopController()),
	}
	_, _, err := rec.Mark("anything")
	assert.ErrorIs(t, err, ErrNotRecording)
}

func TestMark_AppendsAndReturnsOffset(t *testing.T) {
	tempDir := t.TempDir()
	rec := &FFmpegRecorder{
		id:         "mark-active",
		binaryPath: mockBin,
		params:     defaultParams(tempDir),
		outputPath: filepath.Join(tempDir, "mark-active.mp4"),
		stz:        scaletozero.NewOncer(scaletozero.NewNoopController()),
	}
	require.NoError(t, rec.Start(t.Context()))
	t.Cleanup(func() { _ = rec.ForceStop(t.Context()) })

	name, offset, err := rec.Mark("  checkpoint  ")
	require.NoError(t, err)
	assert.Equal(t, "checkpoint", name, "name is trimmed")
	assert.GreaterOrEqual(t, offset, int64(0))

	rec.mu.Lock()
	markers := rec.markers
	rec.mu.Unlock()
	require.Len(t, markers, 1)
	assert.Equal(t, "checkpoint", markers[0].Name)
}

func TestMark_RejectsBadNames(t *testing.T) {
	tempDir := t.TempDir()
	rec := &FFmpegRecorder{
		id:         "mark-bad",
		binaryPath: mockBin,
		params:     defaultParams(tempDir),
		outputPath: filepath.Join(tempDir, "mark-bad.mp4"),
		stz:        scaletozero.NewOncer(scaletozero.NewNoopController()),
	}
	require.NoError(t, rec.Start(t.Context()))
	t.Cleanup(func() { _ = rec.ForceStop(t.Context()) })

	_, _, err := rec.Mark("   ")
	assert.ErrorIs(t, err, ErrInvalidMarkerName)

	_, _, err = rec.Mark(strings.Repeat("x", maxMarkerNameLen+1))
	assert.ErrorIs(t, err, ErrInvalidMarkerName)
}

func TestMark_ConcurrentCallsAreSafe(t *testing.T) {
	tempDir := t.TempDir()
	rec := &FFmpegRecorder{
		id:         "mark-race",
		binaryPath: mockBin,
		params:     defaultParams(tempDir),
		outputPath: filepath.Join(tempDir, "mark-race.mp4"),
		stz:        scaletozero.NewOncer(scaletozero.NewNoopController()),
	}
	require.NoError(t, rec.Start(t.Context()))
	t.Cleanup(func() { _ = rec.ForceStop(t.Context()) })

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, _, _ = rec.Mark("m")
		}()
	}
	wg.Wait()

	rec.mu.Lock()
	count := len(rec.markers)
	rec.mu.Unlock()
	assert.Equal(t, n, count)
}

// chapterInputArgs is the only thing finalize adds for chapters; locking its
// output both ways guarantees the no-marker remux is byte-identical to before.
func TestChapterInputArgs(t *testing.T) {
	assert.Nil(t, chapterInputArgs(""))
	assert.Equal(t, []string{"-f", "ffmetadata", "-i", "/tmp/x.ffmeta", "-map", "0", "-map_chapters", "1"}, chapterInputArgs("/tmp/x.ffmeta"))
}

func TestRemuxArgs_BackwardsCompat(t *testing.T) {
	// No markers: byte-identical to the pre-feature remux command.
	preFeature := []string{
		"-i", "/r/out.mp4",
		"-c", "copy",
		"-movflags", "+faststart",
		"-f", "mp4",
		"-y",
		"/r/out.mp4.tmp",
	}
	assert.Equal(t, preFeature, remuxArgs("/r/out.mp4", "/r/out.mp4.tmp", ""))

	// Markers present: the chapter input and mapping are injected.
	withChapters := remuxArgs("/r/out.mp4", "/r/out.mp4.tmp", "/r/out.mp4.ffmeta")
	assert.Contains(t, withChapters, "-map_chapters")
	assert.Contains(t, withChapters, "/r/out.mp4.ffmeta")
}

// TestFinalize_RetriesWithoutChaptersOnRemuxFailure uses a fake ffmpeg that
// rejects the chaptered remux but succeeds on the plain one, proving a bad
// chapter input can never cost the recording its finalize.
func TestFinalize_RetriesWithoutChaptersOnRemuxFailure(t *testing.T) {
	tempDir := t.TempDir()
	outputPath := filepath.Join(tempDir, "rec.mp4")
	require.NoError(t, os.WriteFile(outputPath, []byte("recording"), 0o644))

	// Fails when the chapter metadata input is present, otherwise writes the
	// output file (the last argument) like the real remux would.
	script := "#!/usr/bin/env bash\n" +
		"for a in \"$@\"; do if [[ \"$a\" == ffmetadata ]]; then exit 1; fi; done\n" +
		"printf remuxed > \"${@: -1}\"\n"
	bin := filepath.Join(tempDir, "fake_ffmpeg.sh")
	require.NoError(t, os.WriteFile(bin, []byte(script), 0o755))

	start := time.Now()
	rec := &FFmpegRecorder{
		id:         "retry-no-chapters",
		binaryPath: bin,
		outputPath: outputPath,
		startTime:  start,
		endTime:    start.Add(4 * time.Second),
		exitCode:   0,
		markers:    []Marker{{Name: "m", At: start.Add(1 * time.Second)}},
		stz:        scaletozero.NewOncer(scaletozero.NewNoopController()),
	}

	require.NoError(t, rec.finalizeRecording(context.Background()))

	b, err := os.ReadFile(outputPath)
	require.NoError(t, err)
	assert.Equal(t, "remuxed", string(b), "plain retry output should replace the recording")

	_, statErr := os.Stat(outputPath + ".ffmeta")
	assert.True(t, os.IsNotExist(statErr), "metadata temp file should be removed")
}

func TestMark_NameLimitCountsCharactersNotBytes(t *testing.T) {
	tempDir := t.TempDir()
	rec := &FFmpegRecorder{
		id:         "mark-runes",
		binaryPath: mockBin,
		params:     defaultParams(tempDir),
		outputPath: filepath.Join(tempDir, "mark-runes.mp4"),
		stz:        scaletozero.NewOncer(scaletozero.NewNoopController()),
	}
	require.NoError(t, rec.Start(t.Context()))
	t.Cleanup(func() { _ = rec.ForceStop(t.Context()) })

	// 200 multibyte characters (600 bytes) is within the 200-character limit.
	name, _, err := rec.Mark(strings.Repeat("日", maxMarkerNameLen))
	require.NoError(t, err)
	assert.Equal(t, strings.Repeat("日", maxMarkerNameLen), name)

	// 201 characters is over the limit, multibyte or not.
	_, _, err = rec.Mark(strings.Repeat("日", maxMarkerNameLen+1))
	assert.ErrorIs(t, err, ErrInvalidMarkerName)
}

func TestWatchCaptureStart_AnchorsToFirstOutputBytes(t *testing.T) {
	rec := &FFmpegRecorder{
		id:         "anchor-watch",
		outputPath: filepath.Join(t.TempDir(), "anchor-watch.mp4"),
		exitCode:   exitCodeInitValue,
		exited:     make(chan struct{}),
	}
	defer close(rec.exited)
	go rec.watchCaptureStart()

	// No output yet: the anchor must stay zero.
	time.Sleep(50 * time.Millisecond)
	rec.mu.Lock()
	assert.True(t, rec.captureAnchor.IsZero(), "anchor set before any output bytes")
	rec.mu.Unlock()

	// First bytes appear: the anchor is stamped at/after the write.
	beforeWrite := time.Now()
	require.NoError(t, os.WriteFile(rec.outputPath, []byte("ftyp"), 0o644))
	require.Eventually(t, func() bool {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		return !rec.captureAnchor.IsZero()
	}, 2*time.Second, 5*time.Millisecond)
	rec.mu.Lock()
	assert.False(t, rec.captureAnchor.Before(beforeWrite), "anchor stamped at/after first write")
	rec.mu.Unlock()
}

func TestWatchCaptureStart_StopsOnExitWithoutOutput(t *testing.T) {
	rec := &FFmpegRecorder{
		id:         "anchor-exit",
		outputPath: filepath.Join(t.TempDir(), "never.mp4"),
		exitCode:   exitCodeInitValue,
		exited:     make(chan struct{}),
	}
	close(rec.exited)
	rec.watchCaptureStart() // must return promptly instead of polling forever
	rec.mu.Lock()
	defer rec.mu.Unlock()
	assert.True(t, rec.captureAnchor.IsZero())
}

func TestWatchCaptureStart_NeverStampsAfterExit(t *testing.T) {
	outputPath := filepath.Join(t.TempDir(), "late.mp4")
	require.NoError(t, os.WriteFile(outputPath, []byte("bytes"), 0o644))

	// Process already reaped (exitCode set, exited closed) but the file has
	// bytes: stamping now would postdate endTime and zero out the duration.
	rec := &FFmpegRecorder{
		id:         "anchor-late",
		outputPath: outputPath,
		exitCode:   0,
		exited:     make(chan struct{}),
	}
	close(rec.exited)
	rec.watchCaptureStart()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	assert.True(t, rec.captureAnchor.IsZero(), "anchor must never be stamped after exit")
}

func TestMark_OffsetUsesCaptureAnchor(t *testing.T) {
	start := time.Now()
	rec := &FFmpegRecorder{
		id:            "anchor-offset",
		cmd:           &exec.Cmd{},
		exitCode:      exitCodeInitValue,
		startTime:     start.Add(-10 * time.Second),
		captureAnchor: start.Add(-1 * time.Second),
	}
	_, offset, err := rec.Mark("m")
	require.NoError(t, err)
	assert.GreaterOrEqual(t, offset, int64(1000))
	assert.Less(t, offset, int64(5000), "offset measured from captureAnchor, not startTime")
}
