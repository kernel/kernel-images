package e2e

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"os/exec"
	"testing"
	"time"

	instanceoapi "github.com/onkernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/require"
)

// BenchmarkChromiumRestart benchmarks chromium stop/start time on both headful and headless images.
// Run with: go test -bench=BenchmarkChromiumRestart -benchtime=5x -v ./e2e/...
//
// This benchmark uses supervisorctl to stop and start chromium, measuring:
// 1. Time to stop chromium
// 2. Time to start chromium
// 3. Time until DevTools is ready (via CDP endpoint)
func BenchmarkChromiumRestart(b *testing.B) {
	if _, err := exec.LookPath("docker"); err != nil {
		b.Skip("docker not available")
	}

	benchmarks := []struct {
		name  string
		image string
	}{
		{"Headless", headlessImage},
		{"Headful", headfulImage},
	}

	for _, bm := range benchmarks {
		b.Run(bm.name, func(b *testing.B) {
			runChromiumRestartBenchmark(b, bm.image, bm.name)
		})
	}
}

func runChromiumRestartBenchmark(b *testing.B, image, imageType string) {
	c := NewTestContainer(b, image)

	env := map[string]string{
		"WIDTH":  "1024",
		"HEIGHT": "768",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Start container
	if err := c.Start(ctx, ContainerConfig{Env: env}); err != nil {
		b.Fatalf("failed to start container: %v", err)
	}
	defer c.Stop(ctx)

	b.Logf("[setup] waiting for API at %s/spec.yaml", c.APIBaseURL())
	if err := c.WaitReady(ctx); err != nil {
		b.Fatalf("api not ready: %v", err)
	}

	// Wait for initial DevTools to be ready
	b.Logf("[setup] waiting for DevTools at %s", c.CDPAddr())
	if err := c.WaitDevTools(ctx); err != nil {
		b.Fatalf("DevTools not ready: %v", err)
	}

	client, err := c.APIClient()
	if err != nil {
		b.Fatalf("failed to create API client: %v", err)
	}

	// Warmup - do one restart cycle to ensure everything is ready
	b.Log("[warmup] performing warmup restart")
	if err := doChromiumRestart(ctx, client, b); err != nil {
		b.Fatalf("warmup restart failed: %v", err)
	}

	// Reset timer after setup
	b.ResetTimer()

	var totalStopTime, totalStartTime, totalDevToolsTime time.Duration

	for i := 0; i < b.N; i++ {
		stopTime, startTime, devtoolsTime, err := measureChromiumRestartCycle(ctx, client, b)
		if err != nil {
			b.Fatalf("restart cycle %d failed: %v", i, err)
		}

		totalStopTime += stopTime
		totalStartTime += startTime
		totalDevToolsTime += devtoolsTime

		b.Logf("[iteration] i=%d stop_ms=%d start_ms=%d devtools_ms=%d total_ms=%d",
			i,
			stopTime.Milliseconds(),
			startTime.Milliseconds(),
			devtoolsTime.Milliseconds(),
			(stopTime + startTime + devtoolsTime).Milliseconds(),
		)
	}

	b.StopTimer()

	// Report metrics
	if b.N > 0 {
		avgStop := totalStopTime / time.Duration(b.N)
		avgStart := totalStartTime / time.Duration(b.N)
		avgDevTools := totalDevToolsTime / time.Duration(b.N)
		avgTotal := avgStop + avgStart + avgDevTools

		b.ReportMetric(float64(avgStop.Milliseconds()), "stop_ms/op")
		b.ReportMetric(float64(avgStart.Milliseconds()), "start_ms/op")
		b.ReportMetric(float64(avgDevTools.Milliseconds()), "devtools_ms/op")
		b.ReportMetric(float64(avgTotal.Milliseconds()), "total_ms/op")

		b.Logf("[summary] image=%s iterations=%d avg_stop_ms=%d avg_start_ms=%d avg_devtools_ms=%d avg_total_ms=%d",
			imageType,
			b.N,
			avgStop.Milliseconds(),
			avgStart.Milliseconds(),
			avgDevTools.Milliseconds(),
			avgTotal.Milliseconds(),
		)
	}
}

// measureChromiumRestartCycle performs a full stop/start cycle and returns timing for each phase.
// Returns: stopTime, startTime, devtoolsReadyTime, error
func measureChromiumRestartCycle(ctx context.Context, client *instanceoapi.ClientWithResponses, tb testing.TB) (time.Duration, time.Duration, time.Duration, error) {
	// Phase 1: Stop chromium
	stopStart := time.Now()
	stopDuration, err := execSupervisorctl(ctx, client, "stop", "chromium")
	if err != nil {
		return 0, 0, 0, fmt.Errorf("stop failed: %w", err)
	}
	stopTime := time.Since(stopStart)
	_ = stopDuration // we use wall-clock time instead

	// Phase 2: Start chromium
	startStart := time.Now()
	startDuration, err := execSupervisorctl(ctx, client, "start", "chromium")
	if err != nil {
		return 0, 0, 0, fmt.Errorf("start failed: %w", err)
	}
	startTime := time.Since(startStart)
	_ = startDuration // we use wall-clock time instead

	// Phase 3: Wait for DevTools to be ready
	devtoolsStart := time.Now()
	if err := waitForDevToolsReady(ctx, client); err != nil {
		return 0, 0, 0, fmt.Errorf("devtools not ready: %w", err)
	}
	devtoolsTime := time.Since(devtoolsStart)

	return stopTime, startTime, devtoolsTime, nil
}

// execSupervisorctl executes a supervisorctl command via the process exec API.
// Returns the duration reported by the API and any error.
func execSupervisorctl(ctx context.Context, client *instanceoapi.ClientWithResponses, action, service string) (time.Duration, error) {
	args := []string{"-c", "/etc/supervisor/supervisord.conf", action, service}
	req := instanceoapi.ProcessExecJSONRequestBody{
		Command: "supervisorctl",
		Args:    &args,
	}

	rsp, err := client.ProcessExecWithResponse(ctx, req)
	if err != nil {
		return 0, fmt.Errorf("request error: %w", err)
	}
	if rsp.StatusCode() != http.StatusOK {
		return 0, fmt.Errorf("unexpected status: %s body=%s", rsp.Status(), string(rsp.Body))
	}
	if rsp.JSON200 == nil {
		return 0, fmt.Errorf("nil response")
	}

	// Check exit code
	exitCode := 0
	if rsp.JSON200.ExitCode != nil {
		exitCode = *rsp.JSON200.ExitCode
	}
	if exitCode != 0 {
		var stdout, stderr string
		if rsp.JSON200.StdoutB64 != nil {
			if b, err := base64.StdEncoding.DecodeString(*rsp.JSON200.StdoutB64); err == nil {
				stdout = string(b)
			}
		}
		if rsp.JSON200.StderrB64 != nil {
			if b, err := base64.StdEncoding.DecodeString(*rsp.JSON200.StderrB64); err == nil {
				stderr = string(b)
			}
		}
		return 0, fmt.Errorf("supervisorctl %s %s failed with exit code %d: stdout=%s stderr=%s", action, service, exitCode, stdout, stderr)
	}

	// Return duration reported by the API
	var duration time.Duration
	if rsp.JSON200.DurationMs != nil {
		duration = time.Duration(*rsp.JSON200.DurationMs) * time.Millisecond
	}
	return duration, nil
}

// waitForDevToolsReady polls the CDP endpoint until it responds.
func waitForDevToolsReady(ctx context.Context, client *instanceoapi.ClientWithResponses) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	timeout := time.After(30 * time.Second)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout:
			return fmt.Errorf("timeout waiting for DevTools")
		case <-ticker.C:
			// Try to list CDP targets via curl inside the container
			args := []string{"-s", "-o", "/dev/null", "-w", "%{http_code}", "http://localhost:9223/json/version"}
			req := instanceoapi.ProcessExecJSONRequestBody{
				Command: "curl",
				Args:    &args,
			}
			rsp, err := client.ProcessExecWithResponse(ctx, req)
			if err != nil {
				continue
			}
			if rsp.JSON200 != nil && rsp.JSON200.ExitCode != nil && *rsp.JSON200.ExitCode == 0 {
				// Check if we got a 200 response
				if rsp.JSON200.StdoutB64 != nil {
					if b, err := base64.StdEncoding.DecodeString(*rsp.JSON200.StdoutB64); err == nil {
						if string(b) == "200" {
							return nil
						}
					}
				}
			}
		}
	}
}

// doChromiumRestart performs a full restart cycle (for warmup).
func doChromiumRestart(ctx context.Context, client *instanceoapi.ClientWithResponses, tb testing.TB) error {
	args := []string{"-c", "/etc/supervisor/supervisord.conf", "restart", "chromium"}
	req := instanceoapi.ProcessExecJSONRequestBody{
		Command: "supervisorctl",
		Args:    &args,
	}

	rsp, err := client.ProcessExecWithResponse(ctx, req)
	if err != nil {
		return fmt.Errorf("request error: %w", err)
	}
	if rsp.StatusCode() != http.StatusOK {
		return fmt.Errorf("unexpected status: %s body=%s", rsp.Status(), string(rsp.Body))
	}

	// Wait for DevTools
	return waitForDevToolsReady(ctx, client)
}

// TestChromiumRestartTiming is a non-benchmark test that prints detailed timing info.
// Useful for quick iteration without the full benchmark harness.
// Run with: go test -v -run TestChromiumRestartTiming ./e2e/...
func TestChromiumRestartTiming(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}

	images := []struct {
		name  string
		image string
	}{
		{"Headless", headlessImage},
		{"Headful", headfulImage},
	}

	const iterations = 3

	for _, img := range images {
		t.Run(img.name, func(t *testing.T) {
			c := NewTestContainer(t, img.image)

			env := map[string]string{
				"WIDTH":  "1024",
				"HEIGHT": "768",
			}

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()

			// Start container
			require.NoError(t, c.Start(ctx, ContainerConfig{Env: env}), "failed to start container")
			defer c.Stop(ctx)

			t.Logf("Waiting for API at %s...", c.APIBaseURL())
			require.NoError(t, c.WaitReady(ctx), "api not ready")

			t.Logf("Waiting for DevTools at %s...", c.CDPAddr())
			require.NoError(t, c.WaitDevTools(ctx), "DevTools not ready")

			client, err := c.APIClient()
			require.NoError(t, err, "failed to create API client")

			// Warmup
			t.Logf("Performing warmup restart...")
			require.NoError(t, doChromiumRestart(ctx, client, t), "warmup restart failed")

			// Collect timing data
			var stopTimes, startTimes, devtoolsTimes []time.Duration

			for i := 0; i < iterations; i++ {
				stopTime, startTime, devtoolsTime, err := measureChromiumRestartCycle(ctx, client, t)
				require.NoError(t, err, "restart cycle %d failed", i)

				stopTimes = append(stopTimes, stopTime)
				startTimes = append(startTimes, startTime)
				devtoolsTimes = append(devtoolsTimes, devtoolsTime)

				t.Logf("  Iteration %d: stop=%dms start=%dms devtools=%dms total=%dms",
					i+1,
					stopTime.Milliseconds(),
					startTime.Milliseconds(),
					devtoolsTime.Milliseconds(),
					(stopTime + startTime + devtoolsTime).Milliseconds(),
				)
			}

			// Calculate averages
			avgStop := avg(stopTimes)
			avgStart := avg(startTimes)
			avgDevTools := avg(devtoolsTimes)
			avgTotal := avgStop + avgStart + avgDevTools

			t.Logf("\n=== %s Results (%d iterations) ===", img.name, iterations)
			t.Logf("  Average stop time:     %dms", avgStop.Milliseconds())
			t.Logf("  Average start time:    %dms", avgStart.Milliseconds())
			t.Logf("  Average devtools time: %dms", avgDevTools.Milliseconds())
			t.Logf("  Average total time:    %dms", avgTotal.Milliseconds())
		})
	}
}

func avg(durations []time.Duration) time.Duration {
	if len(durations) == 0 {
		return 0
	}
	var total time.Duration
	for _, d := range durations {
		total += d
	}
	return total / time.Duration(len(durations))
}
