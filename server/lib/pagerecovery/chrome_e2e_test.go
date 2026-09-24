package pagerecovery

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/kernel/kernel-images/server/lib/cdpclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These exercise the whole path against real Chromium: interception, the
// replay, and what a client driving the browser sees while it happens. They
// need a browser, so they skip unless asked for, matching the other
// real-Chromium suites in this repo.
//
//	KERNEL_PAGERECOVERY_CHROME_E2E=1 go test ./lib/pagerecovery -count=1 -v

func requireChromeE2E(t *testing.T) {
	t.Helper()
	if os.Getenv("KERNEL_PAGERECOVERY_CHROME_E2E") == "" {
		t.Skip("set KERNEL_PAGERECOVERY_CHROME_E2E=1")
	}
}

// refusingOrigin answers the first refusals requests for each /item path with
// status and then serves the real page, which is the shape of a short throttle.
// The count is per path so concurrent tabs are each refused once.
func refusingOrigin(t *testing.T, refusals int, status int) (string, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	var mu sync.Mutex
	perPath := make(map[string]int)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/item") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		hits.Add(1)
		mu.Lock()
		perPath[r.URL.Path]++
		refused := perPath[r.URL.Path] <= refusals
		mu.Unlock()

		w.Header().Set("Content-Type", "text/html")
		if refused {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `<title>Too Many Requests</title>`)
			return
		}
		_, _ = io.WriteString(w, `<title>Item</title><p id="price">2.99</p>`)
	}))
	t.Cleanup(server.Close)
	return server.URL + "/item", &hits
}

func TestRefusedNavigationIsReplayedTransparently(t *testing.T) {
	requireChromeE2E(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	url, hits := refusingOrigin(t, 2, http.StatusTooManyRequests)
	devtools := launchChromium(t, ctx)
	recoverer := startRecoverer(t, ctx, devtools, Config{MaxAttempts: 3, Budget: 20 * time.Second})

	client, session := attachToPage(t, ctx, devtools)
	navigate(t, ctx, client, session, url)

	// One navigation, ending on the page the caller asked for. The refusals
	// are redirect hops inside it, so the client never had a chance to read one.
	assert.Equal(t, "Item", evalString(t, ctx, client, session, "document.title"))
	assert.Equal(t, "2.99", evalString(t, ctx, client, session, "document.getElementById('price').textContent"))
	assert.Equal(t, int32(3), hits.Load())

	snapshot := recoverer.SnapshotMetrics()
	assert.Equal(t, uint64(2), snapshot.Retries)
	assert.Equal(t, uint64(1), snapshot.Recovered)
	assert.Zero(t, snapshot.Exhausted)
	assert.True(t, snapshot.Up)
}

func TestRefusalSurvivesAnExhaustedBudget(t *testing.T) {
	requireChromeE2E(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// More refusals than the budget allows: the caller must end up looking at
	// the site's answer rather than at a navigation that never returns.
	url, hits := refusingOrigin(t, 50, http.StatusTooManyRequests)
	devtools := launchChromium(t, ctx)
	recoverer := startRecoverer(t, ctx, devtools, Config{MaxAttempts: 2, Budget: 20 * time.Second})

	client, session := attachToPage(t, ctx, devtools)
	navigate(t, ctx, client, session, url)

	assert.Equal(t, "Too Many Requests", evalString(t, ctx, client, session, "document.title"))
	assert.Equal(t, int32(3), hits.Load(), "the original request plus its two replays")

	snapshot := recoverer.SnapshotMetrics()
	assert.Equal(t, uint64(2), snapshot.Retries)
	assert.Equal(t, uint64(1), snapshot.Exhausted)
	assert.Zero(t, snapshot.Recovered)
}

func TestUnrefusedNavigationIsUntouched(t *testing.T) {
	requireChromeE2E(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	url, hits := refusingOrigin(t, 0, http.StatusTooManyRequests)
	devtools := launchChromium(t, ctx)
	recoverer := startRecoverer(t, ctx, devtools, Config{})

	client, session := attachToPage(t, ctx, devtools)
	navigate(t, ctx, client, session, url)

	assert.Equal(t, "Item", evalString(t, ctx, client, session, "document.title"))
	assert.Equal(t, int32(1), hits.Load())
	assert.Zero(t, recoverer.SnapshotMetrics().Retries)
}

func TestSettledVerdictIsNotReplayed(t *testing.T) {
	requireChromeE2E(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// A 403 is as often a settled answer as a throttle. Replaying it would add
	// latency and requests without changing anything.
	url, hits := refusingOrigin(t, 5, http.StatusForbidden)
	devtools := launchChromium(t, ctx)
	recoverer := startRecoverer(t, ctx, devtools, Config{})

	client, session := attachToPage(t, ctx, devtools)
	navigate(t, ctx, client, session, url)

	assert.Equal(t, int32(1), hits.Load())
	assert.Zero(t, recoverer.SnapshotMetrics().Retries)
}

func TestClientInterceptionStillSeesEveryRequest(t *testing.T) {
	requireChromeE2E(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// Playwright's page.route is Fetch interception on the client's own session.
	// Kernel's interception must not displace it.
	url, _ := refusingOrigin(t, 1, http.StatusTooManyRequests)
	devtools := launchChromium(t, ctx)
	startRecoverer(t, ctx, devtools, Config{MaxAttempts: 2, Budget: 20 * time.Second})

	client, session := attachToPage(t, ctx, devtools)
	var paused atomic.Int32
	go func() {
		for message := range client.Events() {
			if message.Method != "Fetch.requestPaused" || message.SessionID != session {
				continue
			}
			var event struct {
				RequestID string `json:"requestId"`
			}
			if json.Unmarshal(message.Params, &event) != nil {
				continue
			}
			paused.Add(1)
			_, _ = client.Send(ctx, "Fetch.continueRequest", map[string]any{"requestId": event.RequestID}, session)
		}
	}()
	_, err := client.Send(ctx, "Fetch.enable", map[string]any{
		"patterns": []map[string]any{{"urlPattern": "*", "resourceType": "Document"}},
	}, session)
	require.NoError(t, err, "a client interceptor must be able to coexist with Kernel's")

	navigate(t, ctx, client, session, url)
	assert.Equal(t, "Item", evalString(t, ctx, client, session, "document.title"))
	assert.GreaterOrEqual(t, paused.Load(), int32(2), "the client saw the original request and the replay")
}

func TestConcurrentTabsEachRecover(t *testing.T) {
	requireChromeE2E(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// Every tab is refused once at the same moment. A replay that waited on the
	// connection's event delivery instead of its own goroutine would stall the
	// others behind it.
	const tabs = 6
	base, _ := refusingOrigin(t, 1, http.StatusTooManyRequests)
	devtools := launchChromium(t, ctx)
	recoverer := startRecoverer(t, ctx, devtools, Config{MaxAttempts: 2, Budget: 20 * time.Second})

	client, _ := attachToPage(t, ctx, devtools)
	sessions := make(map[string]string, tabs)
	for i := 0; i < tabs; i++ {
		url := fmt.Sprintf("%s/%d", base, i)
		raw, err := client.Send(ctx, "Target.createTarget", map[string]any{"url": "about:blank"}, "")
		require.NoError(t, err)
		var created struct {
			TargetID string `json:"targetId"`
		}
		require.NoError(t, json.Unmarshal(raw, &created))
		raw, err = client.Send(ctx, "Target.attachToTarget", map[string]any{"targetId": created.TargetID, "flatten": true}, "")
		require.NoError(t, err)
		var attached struct {
			SessionID string `json:"sessionId"`
		}
		require.NoError(t, json.Unmarshal(raw, &attached))
		sessions[attached.SessionID] = url
	}
	// Interception is installed per session on attach; navigating before it is
	// in place would test nothing.
	require.Eventually(t, func() bool { return recoverer.trackedSessions() >= tabs }, 15*time.Second, 25*time.Millisecond)

	for session, url := range sessions {
		_, err := client.Send(ctx, "Page.navigate", map[string]any{"url": url}, session)
		require.NoError(t, err)
	}
	for session, url := range sessions {
		require.Eventually(t, func() bool {
			return evalString(t, ctx, client, session, "location.href") == url &&
				evalString(t, ctx, client, session, "document.title") == "Item"
		}, 30*time.Second, 50*time.Millisecond, "tab %s never reached %s", session, url)
	}
	assert.Equal(t, uint64(tabs), recoverer.SnapshotMetrics().Recovered)
}

// --- harness ---

type staticUpstream struct {
	url     string
	updates chan string
}

func (s staticUpstream) Current() string { return s.url }
func (s staticUpstream) Subscribe() (<-chan string, func()) {
	return s.updates, func() {}
}

func startRecoverer(t *testing.T, ctx context.Context, devtools string, cfg Config) *Recoverer {
	t.Helper()
	recoverer := New(staticUpstream{url: devtools, updates: make(chan string)}, cfg,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, recoverer.Start(ctx))
	t.Cleanup(recoverer.Stop)
	require.Eventually(t, func() bool { return recoverer.SnapshotMetrics().Up }, 15*time.Second, 25*time.Millisecond)
	return recoverer
}

func attachToPage(t *testing.T, ctx context.Context, devtools string) (*cdpclient.Client, string) {
	t.Helper()
	client, err := cdpclient.DialWithEvents(ctx, devtools)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	raw, err := client.Send(ctx, "Target.getTargets", nil, "")
	require.NoError(t, err)
	var targets struct {
		TargetInfos []struct {
			TargetID string `json:"targetId"`
			Type     string `json:"type"`
		} `json:"targetInfos"`
	}
	require.NoError(t, json.Unmarshal(raw, &targets))
	var targetID string
	for _, target := range targets.TargetInfos {
		if target.Type == "page" {
			targetID = target.TargetID
			break
		}
	}
	require.NotEmpty(t, targetID, "chromium started without a page target")

	raw, err = client.Send(ctx, "Target.attachToTarget", map[string]any{"targetId": targetID, "flatten": true}, "")
	require.NoError(t, err)
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	require.NoError(t, json.Unmarshal(raw, &attached))
	_, err = client.Send(ctx, "Page.enable", nil, attached.SessionID)
	require.NoError(t, err)
	return client, attached.SessionID
}

// navigate drives one navigation and waits for the document it settles on,
// which is what a client's goto is waiting for.
func navigate(t *testing.T, ctx context.Context, client *cdpclient.Client, session, url string) {
	t.Helper()
	raw, err := client.Send(ctx, "Page.navigate", map[string]any{"url": url}, session)
	require.NoError(t, err)
	var result struct {
		ErrorText string `json:"errorText"`
	}
	require.NoError(t, json.Unmarshal(raw, &result))
	require.Empty(t, result.ErrorText, "the replay must not surface as a failed navigation")
	// The document the caller is waiting for is the one at the URL it asked
	// for: about:blank reports itself complete from the first poll.
	require.Eventually(t, func() bool {
		return evalString(t, ctx, client, session, "location.href") == url &&
			evalString(t, ctx, client, session, "document.readyState") == "complete"
	}, 30*time.Second, 50*time.Millisecond)
}

func evalString(t *testing.T, ctx context.Context, client *cdpclient.Client, session, expression string) string {
	t.Helper()
	raw, err := client.Send(ctx, "Runtime.evaluate", map[string]any{
		"expression": expression, "returnByValue": true,
	}, session)
	if err != nil {
		return ""
	}
	var result struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(raw, &result))
	return result.Result.Value
}

var devtoolsListening = regexp.MustCompile(`DevTools listening on (ws://\S+)`)

func launchChromium(t *testing.T, ctx context.Context) string {
	t.Helper()
	var binary string
	for _, name := range []string{"chromium", "chromium-browser", "google-chrome", "chrome"} {
		if path, err := exec.LookPath(name); err == nil {
			binary = path
			break
		}
	}
	if binary == "" {
		t.Skip("no chromium binary found on PATH")
	}

	cmd := exec.CommandContext(ctx, binary,
		"--headless=new", "--no-sandbox", "--disable-gpu",
		"--remote-debugging-port=0", "--user-data-dir="+t.TempDir(), "about:blank")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stderr, stderrWriter, err := os.Pipe()
	require.NoError(t, err)
	defer stderrWriter.Close()
	t.Cleanup(func() { _ = stderr.Close() })
	cmd.Stderr = stderrWriter
	require.NoError(t, cmd.Start())
	_ = stderrWriter.Close()
	t.Cleanup(func() {
		// Renderer children outlive the parent otherwise, and TempDir cleanup
		// then races them.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})

	deadline := time.Now().Add(time.Minute)
	if parent, ok := ctx.Deadline(); ok && parent.Before(deadline) {
		deadline = parent
	}
	require.NoError(t, stderr.SetReadDeadline(deadline))
	var buffer []byte
	chunk := make([]byte, 512)
	for {
		n, err := stderr.Read(chunk)
		buffer = append(buffer, chunk[:n]...)
		if match := devtoolsListening.FindSubmatch(buffer); match != nil {
			require.NoError(t, stderr.SetReadDeadline(time.Time{}))
			go func() { _, _ = io.Copy(io.Discard, stderr) }()
			return strings.TrimSpace(string(match[1]))
		}
		require.NoError(t, err, "chromium did not report a DevTools endpoint")
	}
}
