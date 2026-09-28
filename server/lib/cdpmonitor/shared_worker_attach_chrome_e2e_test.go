package cdpmonitor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Chromium's browser process segfaults when a client calls browser-level
// Target.setAutoAttach while another client still holds a session on a shared
// worker that has exited. The monitor must not be that other client.
func TestNewClientAutoAttachAfterSharedWorkerExits(t *testing.T) {
	if os.Getenv("KERNEL_CDPMONITOR_CHROME_E2E") == "" {
		t.Skip("set KERNEL_CDPMONITOR_CHROME_E2E=1 to run real-Chromium shared worker tests")
	}
	for _, worker := range []struct {
		name, create string
	}{
		{"blob", `new SharedWorker(URL.createObjectURL(new Blob([` + "`" + sharedWorkerEcho + "`" + `], {type: 'text/javascript'})))`},
		{"url", `new SharedWorker('/shared.js')`},
	} {
		// closed: the worker exits on its own while its page stays open, so
		// the monitor's session outlives the worker.
		for _, closed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/closed=%t", worker.name, closed), func(t *testing.T) {
				testNewClientAutoAttach(t, worker.create, closed)
			})
		}
	}
}

const sharedWorkerEcho = `self.onconnect = event => {
	const port = event.ports[0];
	port.onmessage = m => m.data === 'close' ? self.close() : port.postMessage(m.data);
	port.start();
};`

func testNewClientAutoAttach(t *testing.T, create string, closed bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/shared.js" {
			w.Header().Set("Content-Type", "text/javascript")
			fmt.Fprint(w, sharedWorkerEcho)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<html><body><script>
			window.sharedWorker = %s;
			sharedWorker.port.start();
		</script></body></html>`, create)
	}))
	defer stub.Close()

	browser := launchCrashObservableChromium(t, findChromium(t))
	ec := newEventCollector()
	m := New(&staticUpstream{url: browser.wsURL}, ec.publishFn(), 99, discardLogger, nil)
	require.NoError(t, m.Start(ctx))
	defer m.Stop()

	cdp := dialCDP(t, ctx, browser.wsURL)
	defer cdp.close()
	targetID := cdp.call(t, ctx, "", "Target.createTarget", map[string]any{"url": stub.URL}).targetID(t)
	require.Eventually(t, func() bool {
		return sharedWorkerExists(ctx, cdp)
	}, 10*time.Second, 50*time.Millisecond, "shared worker was not created")
	// Give the monitor time to attach if it tracks shared workers.
	time.Sleep(500 * time.Millisecond)
	if closed {
		sessionID := cdp.call(t, ctx, "", "Target.attachToTarget", map[string]any{"targetId": targetID, "flatten": true}).sessionID(t)
		evaluateNetworkScript(t, ctx, cdp, sessionID, `sharedWorker.port.postMessage('close'), true`)
		require.Eventually(t, func() bool {
			return !sharedWorkerExists(ctx, cdp)
		}, 10*time.Second, 50*time.Millisecond, "shared worker did not exit")
	}

	// Reconnect repeatedly with the browser-level auto-attach that
	// Playwright's connectOverCDP sends.
	for i := range 20 {
		client := dialCDP(t, ctx, browser.wsURL)
		callCtx, cancelCall := context.WithTimeout(ctx, 5*time.Second)
		_, err := client.roundtrip(callCtx, "", "Target.setAutoAttach", map[string]any{
			"autoAttach": true, "waitForDebuggerOnStart": false, "flatten": true,
		})
		cancelCall()
		time.Sleep(150 * time.Millisecond)
		client.close()
		select {
		case <-browser.exited:
			t.Fatalf("chromium exited on reconnect %d (setAutoAttach err=%v); stderr tail:\n%s", i+1, err, browser.stderrTail())
		case <-time.After(250 * time.Millisecond):
		}
		require.NoError(t, err, "reconnect %d", i+1)
	}
}

func sharedWorkerExists(ctx context.Context, cdp *cdpConn) bool {
	raw, err := cdp.roundtrip(ctx, "", "Target.getTargets", nil)
	if err != nil {
		return false
	}
	var result struct {
		Result struct {
			Targets []struct {
				Type string `json:"type"`
			} `json:"targetInfos"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &result) != nil {
		return false
	}
	for _, target := range result.Result.Targets {
		if target.Type == "shared_worker" {
			return true
		}
	}
	return false
}

type crashObservableChromium struct {
	wsURL  string
	exited chan struct{}
	mu     sync.Mutex
	stderr bytes.Buffer
}

func (c *crashObservableChromium) stderrTail() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	lines := strings.Split(c.stderr.String(), "\n")
	return strings.Join(lines[max(0, len(lines)-40):], "\n")
}

func (c *crashObservableChromium) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stderr.Write(p)
}

// launchCrashObservableChromium is launchChromium plus exit and stderr capture,
// so a browser-process crash fails the test with Chromium's signal report.
// KERNEL_CDPMONITOR_CHROME_HEADFUL=1 drops --headless for use under Xvfb.
func launchCrashObservableChromium(t *testing.T, chrome string) *crashObservableChromium {
	t.Helper()
	args := []string{
		"--no-sandbox", "--disable-gpu", "--no-first-run", "--password-store=basic",
		"--remote-debugging-port=0", "--user-data-dir=" + t.TempDir(),
	}
	if os.Getenv("KERNEL_CDPMONITOR_CHROME_HEADFUL") == "" {
		args = append(args, "--headless=new")
	}
	cmd := exec.Command(chrome, append(args, "about:blank")...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stderr, stderrWriter, err := os.Pipe()
	require.NoError(t, err)
	cmd.Stderr = stderrWriter
	require.NoError(t, cmd.Start())
	_ = stderrWriter.Close()
	c := &crashObservableChromium{exited: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(c.exited)
	}()
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-c.exited
		_ = stderr.Close()
	})
	c.wsURL, err = readDevToolsURL(stderr, time.Now().Add(time.Minute))
	require.NoError(t, err)
	go func() { _, _ = io.Copy(c, stderr) }()
	return c
}
