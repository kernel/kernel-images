// Command camerad runs the browser camera manager (see lib/camera). supervisord
// starts it only when a session is created with the camera capability enabled;
// the wrapper gates it on KERNEL_ENABLE_CAMERA.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/kernel/kernel-images/server/lib/camera"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg := camera.Config{
		OutputDevice:   env("CAMERA_OUTPUT_DEVICE", camera.DefaultOutputDevice),
		IngestDevice:   env("CAMERA_INGEST_DEVICE", camera.DefaultIngestDevice),
		Width:          envInt("CAMERA_WIDTH", camera.DefaultWidth),
		Height:         envInt("CAMERA_HEIGHT", camera.DefaultHeight),
		FPS:            envInt("CAMERA_FPS", camera.DefaultFPS),
		PlaceholderPNG: env("CAMERA_PLACEHOLDER_PNG", "/etc/kernel/camera-placeholder.png"),
		FFmpegPath:     env("FFMPEG_PATH", "ffmpeg"),
		StateAddr:      env("CAMERA_STATE_ADDR", "127.0.0.1:10004"),
		StateFile:      env("CAMERA_STATE_FILE", "/var/www/camera-state.json"),
		LoadModule:     env("CAMERA_LOAD_MODULE", "true") == "true",
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Info("camerad starting", "output", cfg.OutputDevice, "ingest", cfg.IngestDevice)
	if err := camera.New(cfg, log).Run(ctx); err != nil && ctx.Err() == nil {
		log.Error("camerad exited", "err", err)
		os.Exit(1)
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
