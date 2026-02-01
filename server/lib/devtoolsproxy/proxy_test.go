package devtoolsproxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/onkernel/kernel-images/server/lib/scaletozero"
)

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testLogWriter wraps testing.T for slog output and can be stopped to avoid
// panics when the test goroutine logs after the test has completed.
type testLogWriter struct {
	t       *testing.T
	stopped atomic.Bool
}

func (tw *testLogWriter) Write(p []byte) (n int, err error) {
	if tw.stopped.Load() {
		// Test is done, discard logs to avoid panic
		return len(p), nil
	}
	tw.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}

func (tw *testLogWriter) Stop() {
	tw.stopped.Store(true)
}

func newTestLogWriter(t *testing.T) *testLogWriter {
	return &testLogWriter{t: t}
}

func findBrowserBinary() (string, error) {
	candidates := []string{"chromium", "chromium-browser", "google-chrome", "google-chrome-stable"}
	for _, name := range candidates {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("no chromium/chrome binary found")
}

func getFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func waitForCondition(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return cond()
}

func TestWaitForInitialTimeoutWhenLogMissing(t *testing.T) {
	mgr := NewUpstreamManager("/tmp/not-a-real-file-hopefully", silentLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)
	if _, err := mgr.WaitForInitial(300 * time.Millisecond); err == nil {
		t.Fatalf("expected timeout error, got nil")
	}
}

func TestWebSocketProxyHandler_ProxiesEcho(t *testing.T) {
	// Start an echo websocket server as upstream
	echoSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			OriginPatterns: []string{"*"},
		})
		if err != nil {
			t.Fatalf("accept failed: %v", err)
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")

		ctx := r.Context()
		for {
			mt, msg, err := c.Read(ctx)
			if err != nil {
				return
			}
			// echo back with path+query prefixed to verify preservation
			payload := []byte(r.URL.Path + "?" + r.URL.RawQuery + "|" + string(msg))
			if err := c.Write(ctx, mt, payload); err != nil {
				return
			}
		}
	}))
	defer echoSrv.Close()

	// Build ws URL for upstream
	u, _ := url.Parse(echoSrv.URL)
	u.Scheme = "ws"
	u.Path = "/echo"
	u.RawQuery = "k=v"

	logger := silentLogger()
	mgr := NewUpstreamManager("/dev/null", logger)
	// seed current upstream to echo server including path/query (bypass tailing)
	mgr.setCurrent((&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path, RawQuery: u.RawQuery}).String())

	proxy := WebSocketProxyHandler(mgr, logger, false, scaletozero.NewNoopController())
	proxySrv := httptest.NewServer(proxy)
	defer proxySrv.Close()

	// Connect to proxy with the same path/query and verify echo
	pu, _ := url.Parse(proxySrv.URL)
	pu.Scheme = "ws"
	// Provide a different client path/query; proxy should ignore these
	pu.Path = "/client"
	pu.RawQuery = "x=y"

	ctx := context.Background()
	conn, _, err := websocket.Dial(ctx, pu.String(), nil)
	if err != nil {
		t.Fatalf("dial proxy failed: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	msg := "hello"
	if err := conn.Write(ctx, websocket.MessageText, []byte(msg)); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	_, resp, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	expectedPrefix := u.Path + "?" + u.RawQuery + "|"
	if !strings.HasPrefix(string(resp), expectedPrefix) || !strings.HasSuffix(string(resp), msg) {
		t.Fatalf("unexpected echo: %q", string(resp))
	}
}

func TestUpstreamManagerDetectsChromiumAndRestart(t *testing.T) {
	browser, err := findBrowserBinary()
	if err != nil {
		t.Skip("chromium/chrome not installed in environment")
	}

	logDir := t.TempDir()
	logPath := filepath.Join(logDir, "chromium.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("open log file: %v", err)
	}
	defer logFile.Close()

	logWriter := newTestLogWriter(t)
	logger := slog.New(slog.NewTextHandler(logWriter, &slog.HandlerOptions{Level: slog.LevelDebug}))
	mgr := NewUpstreamManager(logPath, logger)
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		// Stop the log writer to prevent panics from background goroutine
		// logging after test completion
		logWriter.Stop()
		// Give the tail goroutine time to exit after context cancellation
		time.Sleep(100 * time.Millisecond)
	}()
	mgr.Start(ctx)

	startChromium := func(port int) (*exec.Cmd, error) {
		userDir := t.TempDir()
		chromiumArgs := []string{
			"--headless=new",
			"--remote-debugging-address=127.0.0.1",
			fmt.Sprintf("--remote-debugging-port=%d", port),
			"--no-first-run",
			"--no-default-browser-check",
			"--disable-gpu",
			"--disable-software-rasterizer",
			"--disable-dev-shm-usage",
			"--no-sandbox",
			"--disable-setuid-sandbox",
			fmt.Sprintf("--user-data-dir=%s", userDir),
			"about:blank",
		}

		// Use stdbuf to force line-buffering on stderr so the "DevTools listening"
		// line is flushed immediately. Without this, output to a file may be fully
		// buffered and the line might not appear until the buffer fills or the
		// process exits, causing the test to flake in CI.
		var cmd *exec.Cmd
		if stdbufPath, err := exec.LookPath("stdbuf"); err == nil {
			// stdbuf -oL -eL: line-buffer stdout (-oL) and stderr (-eL)
			args := append([]string{"-oL", "-eL", browser}, chromiumArgs...)
			t.Logf("starting chromium via stdbuf: %s %v", stdbufPath, args)
			cmd = exec.Command(stdbufPath, args...)
		} else {
			t.Logf("stdbuf not found, starting chromium directly: %s %v", browser, chromiumArgs)
			cmd = exec.Command(browser, chromiumArgs...)
		}

		cmd.Stdout = logFile
		cmd.Stderr = logFile
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		t.Logf("chromium started with PID %d", cmd.Process.Pid)
		return cmd, nil
	}

	port1, err := getFreePort()
	if err != nil {
		t.Fatalf("get free port: %v", err)
	}
	cmd1, err := startChromium(port1)
	if err != nil {
		t.Fatalf("start chromium 1: %v", err)
	}
	defer func() {
		_ = cmd1.Process.Kill()
		_, _ = cmd1.Process.Wait()
	}()

	// Wait for initial upstream containing port1
	ok := waitForCondition(20*time.Second, func() bool {
		u := mgr.Current()
		return strings.Contains(u, fmt.Sprintf(":%d/", port1))
	})
	if !ok {
		// Read and log the contents of the log file for debugging
		logContents, readErr := os.ReadFile(logPath)
		if readErr != nil {
			t.Logf("failed to read log file: %v", readErr)
		} else {
			t.Logf("chromium log file contents (%d bytes):\n%s", len(logContents), string(logContents))
		}
		t.Fatalf("did not detect initial upstream for port %d; got: %q", port1, mgr.Current())
	}

	// Restart on a new port
	port2, err := getFreePort()
	if err != nil {
		t.Fatalf("get free port 2: %v", err)
	}
	t.Logf("killing first chromium instance to restart on port %d", port2)
	_ = cmd1.Process.Kill()
	_, _ = cmd1.Process.Wait()

	cmd2, err := startChromium(port2)
	if err != nil {
		t.Fatalf("start chromium 2: %v", err)
	}
	defer func() {
		_ = cmd2.Process.Kill()
		_, _ = cmd2.Process.Wait()
	}()

	// Expect manager to update to new port
	ok = waitForCondition(20*time.Second, func() bool {
		u := mgr.Current()
		return strings.Contains(u, fmt.Sprintf(":%d/", port2))
	})
	if !ok {
		// Read and log the contents of the log file for debugging
		logContents, readErr := os.ReadFile(logPath)
		if readErr != nil {
			t.Logf("failed to read log file: %v", readErr)
		} else {
			t.Logf("chromium log file contents after restart (%d bytes):\n%s", len(logContents), string(logContents))
		}
		t.Fatalf("did not update upstream to port %d; got: %q", port2, mgr.Current())
	}
}

func TestUpstreamManagerSubscriberGetsLatest(t *testing.T) {
	logger := silentLogger()
	mgr := NewUpstreamManager("/dev/null", logger)

	updates, cancel := mgr.Subscribe()
	defer cancel()

	// Fill buffer with an older value, then immediately update to a newer value.
	// The subscriber buffer size is 1, so the second send should replace the
	// buffered value instead of being dropped.
	mgr.setCurrent("ws://old")
	mgr.setCurrent("ws://new")

	select {
	case v := <-updates:
		if v != "ws://new" {
			t.Fatalf("expected latest update 'ws://new', got %q", v)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for update")
	}

	// Subsequent updates should still be delivered normally.
	mgr.setCurrent("ws://newer")
	select {
	case v := <-updates:
		if v != "ws://newer" {
			t.Fatalf("expected next update 'ws://newer', got %q", v)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for next update")
	}
}
