package recorder

import (
	"os"
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

func TestBuildChapterMetadata_DropsNegativeOffsets(t *testing.T) {
	start := time.Unix(1000, 0)
	out := filepath.Join(t.TempDir(), "rec.mp4")
	markers := []Marker{
		{Name: "before-start", At: start.Add(-3 * time.Second)},
		{Name: "kept", At: start.Add(4 * time.Second)},
	}

	path, ok, err := buildChapterMetadata(out, markers, start, 8000)
	require.NoError(t, err)
	require.True(t, ok)

	meta := readMeta(t, path)
	assert.NotContains(t, meta, "before-start")
	assert.Contains(t, meta, "title=kept")
	// Sentinel + one kept marker = two chapters.
	assert.Equal(t, 2, strings.Count(meta, "[CHAPTER]"))
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

	_, _, err = rec.Mark(strings.Repeat("界", maxMarkerNameLen))
	assert.NoError(t, err)

	_, _, err = rec.Mark(strings.Repeat("界", maxMarkerNameLen+1))
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

// finalizeRemuxArgs mirrors how finalizeRecording assembles the remux command
// so the backward-compat assertion below can lock both shapes.
func finalizeRemuxArgs(outputPath, tempPath, metaPath string) []string {
	args := []string{"-i", outputPath}
	args = append(args, chapterInputArgs(metaPath)...)
	args = append(args,
		"-c", "copy",
		"-movflags", "+faststart",
		"-f", "mp4",
		"-y",
		tempPath,
	)
	return args
}

func TestFinalizeRemuxArgs_BackwardsCompat(t *testing.T) {
	// No markers: byte-identical to the pre-feature remux command.
	preFeature := []string{
		"-i", "/r/out.mp4",
		"-c", "copy",
		"-movflags", "+faststart",
		"-f", "mp4",
		"-y",
		"/r/out.mp4.tmp",
	}
	assert.Equal(t, preFeature, finalizeRemuxArgs("/r/out.mp4", "/r/out.mp4.tmp", ""))

	// Markers present: the chapter input and mapping are injected.
	withChapters := finalizeRemuxArgs("/r/out.mp4", "/r/out.mp4.tmp", "/r/out.mp4.ffmeta")
	assert.Contains(t, withChapters, "-map_chapters")
	assert.Contains(t, withChapters, "/r/out.mp4.ffmeta")
}
