package recorder

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// Marker records a named point in time during a recording. At finalize the
// markers are written into the output MP4 as chapters so they can be scrubbed
// to in any tool that reads MP4 chapter metadata.
type Marker struct {
	Name string
	At   time.Time
}

// sentinelChapterName is the synthetic first chapter covering the span before
// the first real marker. ffmpeg forces the first chapter to start at 0, so
// without it the first real marker's timestamp would be clamped to 0.
const sentinelChapterName = "_recording_start"

// buildChapterMetadata writes an ffmetadata file describing one MP4 chapter per
// marker and returns its path. Each marker's chapter starts at its offset from
// startTime, clamped into [0, durationMs]: a marker can be timestamped
// slightly outside the media timeline (e.g. between process spawn and the
// first captured frame, or after capture stopped but before the process was
// reaped), and once Mark returned 201 its chapter must never be silently
// lost. durationMs is the recording length and becomes the END of the final
// chapter. Chapter ENDs are clamped to never precede their START, since
// ffmpeg rejects an END-before-START chapter outright and would fail the
// whole remux.
//
// ok is false when there are no chapters to write (no markers supplied, or a
// degenerate non-positive duration), signalling the caller to fall back to
// the normal no-chapter remux. The metadata file is written next to
// outputPath with a ".ffmeta" suffix.
func buildChapterMetadata(outputPath string, markers []Marker, startTime time.Time, durationMs int64) (string, bool, error) {
	type chapter struct {
		startMs int64
		title   string
	}

	if durationMs <= 0 {
		return "", false, nil
	}

	chapters := make([]chapter, 0, len(markers))
	for _, m := range markers {
		startMs := m.At.Sub(startTime).Milliseconds()
		if startMs < 0 {
			startMs = 0
		}
		if startMs > durationMs {
			startMs = durationMs
		}
		chapters = append(chapters, chapter{startMs: startMs, title: m.Name})
	}
	if len(chapters) == 0 {
		return "", false, nil
	}

	sort.SliceStable(chapters, func(i, j int) bool {
		return chapters[i].startMs < chapters[j].startMs
	})

	// Prepend the sentinel so the first real marker keeps its true start.
	if chapters[0].startMs > 0 {
		chapters = append([]chapter{{startMs: 0, title: sentinelChapterName}}, chapters...)
	}

	var b strings.Builder
	b.WriteString("FFMETADATA1\n")
	for i, c := range chapters {
		endMs := durationMs
		if i+1 < len(chapters) {
			endMs = chapters[i+1].startMs
		}
		if endMs < c.startMs {
			endMs = c.startMs
		}
		b.WriteString("[CHAPTER]\n")
		b.WriteString("TIMEBASE=1/1000\n")
		fmt.Fprintf(&b, "START=%d\n", c.startMs)
		fmt.Fprintf(&b, "END=%d\n", endMs)
		fmt.Fprintf(&b, "title=%s\n", escapeFFMetadata(c.title))
	}

	path := outputPath + ".ffmeta"
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return "", false, fmt.Errorf("failed to write chapter metadata: %w", err)
	}
	return path, true, nil
}

// escapeFFMetadata escapes the characters ffmpeg treats specially in
// ffmetadata values: backslash, equals, semicolon, hash, and newline.
func escapeFFMetadata(s string) string {
	replacer := strings.NewReplacer(
		"\\", "\\\\",
		"=", "\\=",
		";", "\\;",
		"#", "\\#",
		"\n", "\\\n",
	)
	return replacer.Replace(s)
}
