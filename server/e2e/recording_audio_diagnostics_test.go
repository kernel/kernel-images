package e2e

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Capture only the isolated audio fixture's services, never environment variables
// or browser storage. Run before the test container is removed, including on failure.
func captureReplayAudioDiagnostics(t *testing.T, c *TestContainer, outputPath string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		t.Logf("audio diagnostics: %v", err)
		return
	}
	commands := map[string][]string{
		"probe.json":   {"ffprobe", "-v", "error", "-show_format", "-show_streams", "-show_packets", "-of", "json", "/tmp/e2e-recording-audio.mp4"},
		"services.log": {"sh", "-c", "for f in /var/log/supervisord/kernel-images-api /var/log/supervisord/pulseaudio /var/log/supervisord.log; do echo ==== $f; cat $f; done"},
		"pulse.log":    {"sh", "-c", "PULSE_SERVER=unix:/tmp/pulse/native pactl list sinks; PULSE_SERVER=unix:/tmp/pulse/native pactl list source-outputs"},
	}
	for suffix, command := range commands {
		code, out, err := c.Exec(ctx, command)
		if err != nil || code != 0 {
			t.Logf("audio diagnostics %s: exit=%d err=%v", suffix, code, err)
		}
		if err := os.WriteFile(outputPath+"."+suffix, []byte(out), 0o644); err != nil {
			t.Logf("audio diagnostics: %v", err)
		}
	}
}
