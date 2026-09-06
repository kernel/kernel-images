package recorder

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFFmpegAudioPreservesInputStartOffset(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("separate PulseAudio input is Linux-only")
	}
	for _, binary := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Skipf("%s required: %v", binary, err)
		}
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "audio.mp4")
	params := defaultParams(dir)
	audio := true
	params.RecordAudio = &audio
	args, err := ffmpegArgs(params, path)
	require.NoError(t, err)

	// Model live devices opened three seconds apart on the same clock. Both
	// finish at timestamp 16, so their final output packets must still coincide.
	// Replace only capture devices; retain the production sync/encode/mux flags.
	fixtureArgs := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-framerate":
			i++
		case args[i] == "-f" && (args[i+1] == "x11grab" || args[i+1] == "pulse"):
			fixtureArgs = append(fixtureArgs, "-f", "lavfi")
			i++
		case args[i] == "-i":
			input := "testsrc2=size=320x240:rate=10:duration=6,setpts=PTS+10/TB"
			if args[i+1] == "KernelOutput.monitor" {
				input = "sine=frequency=440:sample_rate=48000:duration=3,asetpts=PTS+13/TB"
			}
			fixtureArgs = append(fixtureArgs, "-i", input)
			i++
		default:
			fixtureArgs = append(fixtureArgs, args[i])
		}
	}
	out, err := exec.CommandContext(t.Context(), "ffmpeg", fixtureArgs...).CombinedOutput()
	require.NoError(t, err, "%s", out)
	for _, name := range []string{"fragmented", "finalized"} {
		t.Run(name, func(t *testing.T) {
			if name == "finalized" {
				finalized := filepath.Join(dir, "finalized.mp4")
				out, err := exec.CommandContext(t.Context(), "ffmpeg", remuxArgs(path, finalized, "")...).CombinedOutput()
				require.NoError(t, err, "%s", out)
				path = finalized
			}
			out, err := exec.CommandContext(t.Context(), "ffprobe", "-v", "error", "-show_packets", "-of", "json", path).Output()
			require.NoError(t, err)
			var probe struct {
				Packets []struct {
					CodecType string `json:"codec_type"`
					PTS       string `json:"pts_time"`
					Duration  string `json:"duration_time"`
				} `json:"packets"`
			}
			require.NoError(t, json.Unmarshal(out, &probe))
			var videoEnd, audioEnd float64
			audioPTS := make([]float64, 0)
			for _, p := range probe.Packets {
				pts, err := strconv.ParseFloat(p.PTS, 64)
				require.NoError(t, err)
				var duration float64
				if p.Duration != "" {
					duration, err = strconv.ParseFloat(p.Duration, 64)
					require.NoError(t, err)
				}
				if p.CodecType == "video" {
					videoEnd = pts + duration
				} else if p.CodecType == "audio" {
					audioEnd = pts + duration
					audioPTS = append(audioPTS, pts)
				}
			}
			t.Logf("video_end=%.6f audio_end=%.6f", videoEnd, audioEnd)
			require.InDelta(t, videoEnd, audioEnd, 0.1, "independently zeroing input timestamps moves audio three seconds early")
			// empty_moov starts the first packet at zero. Subsequent packets must
			// preserve the device offset, without padding the gap with silent samples.
			require.Greater(t, len(audioPTS), 1)
			require.InDelta(t, 3, audioPTS[1], 0.03)
			pcm, err := exec.CommandContext(t.Context(), "ffmpeg", "-v", "error", "-i", path,
				"-map", "0:a:0", "-f", "s16le", "-ac", "2", "-ar", "48000", "-").Output()
			require.NoError(t, err)
			require.InDelta(t, 3, float64(len(pcm))/(48000*2*2), 0.05, "sync must not add silent samples")
		})
	}
}
