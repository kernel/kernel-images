// Reproduce the control-plane fast configure path for POST /browsers with a
// custom viewport.
//
// For an org feature-flagged into the batched configure path, the control plane
// sends kernel-images a multipart POST /configure request with a "display" JSON
// part:
//
//	{"width":1365,"height":768,"refresh_rate":60,"restart_chromium":true,"require_idle":true}
//
// This harness starts a fresh headful container for every iteration, waits for
// API/CDP readiness, sends one configure request, immediately performs a CDP
// round-trip after configure returns, then saves container logs and telemetry
// artifacts for later inspection.
//
// Usage:
//
//	cd server
//	go run ./scripts/custom_viewport_config_test -iterations 50
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kernel/kernel-images/server/lib/cdpclient"
)

type config struct {
	image          string
	namePrefix     string
	iterations     int
	parallelism    int
	width          int
	height         int
	refreshRate    int
	buildImage     bool
	keepContainers bool
	outDir         string
	timeout        time.Duration
}

type containerInfo struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	APIPort          int    `json:"api_port"`
	CDPPort          int    `json:"cdp_port"`
	ChromedriverPort int    `json:"chromedriver_port"`
}

type iterationResult struct {
	Iteration        int           `json:"iteration"`
	Width            int           `json:"width"`
	Height           int           `json:"height"`
	RefreshRate      int           `json:"refresh_rate"`
	Container        containerInfo `json:"container"`
	ContainerStartMs int64         `json:"container_start_ms"`
	ReadyMs          int64         `json:"ready_ms"`
	PreCDPMs         int64         `json:"pre_cdp_ms"`
	ConfigureMs      int64         `json:"configure_ms"`
	VerifyMs         int64         `json:"verify_ms"`
	PostCDPMs        int64         `json:"post_cdp_ms"`
	ConfigureStatus  int           `json:"configure_status"`
	XrandrWidth      int           `json:"xrandr_width,omitempty"`
	XrandrHeight     int           `json:"xrandr_height,omitempty"`
	WindowWidth      int           `json:"window_width,omitempty"`
	WindowHeight     int           `json:"window_height,omitempty"`
	WindowMaximized  bool          `json:"window_maximized,omitempty"`
	Error            string        `json:"error,omitempty"`
	LogPath          string        `json:"log_path,omitempty"`
	InspectPath      string        `json:"inspect_path,omitempty"`
	TelemetryPath    string        `json:"telemetry_path,omitempty"`
	StartedAt        time.Time     `json:"started_at"`
	FinishedAt       time.Time     `json:"finished_at"`
}

func main() {
	cfg := parseFlags()
	ctx := context.Background()

	if cfg.buildImage {
		if err := runStream(ctx, "docker", "build", "-f", "../images/chromium-headful/Dockerfile", "-t", cfg.image, ".."); err != nil {
			fatalf("build image: %v", err)
		}
	}

	if err := os.MkdirAll(cfg.outDir, 0o755); err != nil {
		fatalf("create output dir: %v", err)
	}
	for _, subdir := range []string{"logs", "inspect", "telemetry"} {
		if err := os.MkdirAll(filepath.Join(cfg.outDir, subdir), 0o755); err != nil {
			fatalf("create output subdir: %v", err)
		}
	}
	fmt.Printf("Writing artifacts to %s\n", cfg.outDir)

	resultsPath := filepath.Join(cfg.outDir, "results.jsonl")
	resultsFile, err := os.Create(resultsPath)
	if err != nil {
		fatalf("create results file: %v", err)
	}
	defer resultsFile.Close()
	enc := json.NewEncoder(resultsFile)

	results := runIterations(ctx, cfg, enc)

	printSummary(cfg, results)
}

func parseFlags() config {
	var cfg config
	flag.StringVar(&cfg.image, "image", "kernel-headful-test", "Docker image to run")
	flag.StringVar(&cfg.namePrefix, "name-prefix", "kernel-headful-custom-viewport-repro", "Docker container name prefix")
	flag.IntVar(&cfg.iterations, "iterations", 50, "Number of fresh-container configure/CDP simulations")
	flag.IntVar(&cfg.parallelism, "parallelism", 1, "Maximum number of fresh containers to run concurrently")
	flag.IntVar(&cfg.width, "width", 1280, "Custom viewport width")
	flag.IntVar(&cfg.height, "height", 720, "Custom viewport height")
	flag.IntVar(&cfg.refreshRate, "refresh-rate", 60, "Viewport refresh rate sent by the control-plane path")
	flag.BoolVar(&cfg.buildImage, "build", false, "Build the headful image before running")
	flag.BoolVar(&cfg.keepContainers, "keep-containers", false, "Leave per-iteration Docker containers running after collecting artifacts")
	flag.StringVar(&cfg.outDir, "out", "", "Artifact output directory (default: /tmp/kernel-images-custom-viewport-config-<timestamp>)")
	flag.DurationVar(&cfg.timeout, "timeout", 90*time.Second, "Readiness and per-operation timeout")
	flag.Parse()

	if cfg.iterations <= 0 {
		fatalf("iterations must be positive")
	}
	if cfg.parallelism <= 0 {
		fatalf("parallelism must be positive")
	}
	if cfg.width <= 0 || cfg.height <= 0 {
		fatalf("viewport dimensions must be positive")
	}
	if cfg.refreshRate <= 0 {
		fatalf("refresh-rate must be positive")
	}
	if cfg.outDir == "" {
		cfg.outDir = filepath.Join(os.TempDir(), "kernel-images-custom-viewport-config-"+time.Now().Format("20060102-150405"))
	}
	return cfg
}

func runIterations(ctx context.Context, cfg config, enc *json.Encoder) []iterationResult {
	jobs := make(chan int)
	resultsCh := make(chan iterationResult)

	var wg sync.WaitGroup
	for worker := 0; worker < cfg.parallelism; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for iter := range jobs {
				resultsCh <- runIteration(ctx, cfg, iter)
			}
		}()
	}

	go func() {
		for i := 1; i <= cfg.iterations; i++ {
			jobs <- i
		}
		close(jobs)
		wg.Wait()
		close(resultsCh)
	}()

	results := make([]iterationResult, 0, cfg.iterations)
	for res := range resultsCh {
		results = append(results, res)
		if err := enc.Encode(res); err != nil {
			fatalf("write result: %v", err)
		}
		if res.Error != "" {
			fmt.Printf("iter=%03d container=%s api=%d cdp=%d ready_ms=%d configure_ms=%d verify_ms=%d post_cdp_ms=%d status=%d xrandr=%dx%d window=%dx%d maximized=%t error=%s\n",
				res.Iteration, shortID(res.Container.ID), res.Container.APIPort, res.Container.CDPPort, res.ReadyMs, res.ConfigureMs, res.VerifyMs, res.PostCDPMs, res.ConfigureStatus, res.XrandrWidth, res.XrandrHeight, res.WindowWidth, res.WindowHeight, res.WindowMaximized, res.Error)
			continue
		}
		fmt.Printf("iter=%03d container=%s api=%d cdp=%d ready_ms=%d pre_cdp_ms=%d configure_ms=%d verify_ms=%d post_cdp_ms=%d status=%d xrandr=%dx%d window=%dx%d maximized=%t\n",
			res.Iteration, shortID(res.Container.ID), res.Container.APIPort, res.Container.CDPPort, res.ReadyMs, res.PreCDPMs, res.ConfigureMs, res.VerifyMs, res.PostCDPMs, res.ConfigureStatus, res.XrandrWidth, res.XrandrHeight, res.WindowWidth, res.WindowHeight, res.WindowMaximized)
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].Iteration < results[j].Iteration
	})
	return results
}

func runIteration(ctx context.Context, cfg config, iter int) (res iterationResult) {
	res = iterationResult{
		Iteration:   iter,
		Width:       cfg.width,
		Height:      cfg.height,
		RefreshRate: cfg.refreshRate,
		StartedAt:   time.Now(),
	}
	defer func() {
		res.FinishedAt = time.Now()
	}()

	name := fmt.Sprintf("%s-%03d-%d", cfg.namePrefix, iter, time.Now().UnixNano())

	start := time.Now()
	info, err := startContainer(ctx, cfg, name)
	res.ContainerStartMs = time.Since(start).Milliseconds()
	if err != nil {
		res.Error = fmt.Sprintf("start container: %v", err)
		return res
	}
	res.Container = info
	defer func() {
		res.LogPath = saveDockerOutput(ctx, cfg.outDir, "logs", iter, info, "docker", "logs", info.ID)
		res.InspectPath = saveDockerOutput(ctx, cfg.outDir, "inspect", iter, info, "docker", "inspect", info.ID)
		res.TelemetryPath = saveTelemetry(ctx, cfg.outDir, iter, info)
		if !cfg.keepContainers {
			_ = exec.CommandContext(context.Background(), "docker", "rm", "-f", info.ID).Run()
		}
	}()

	apiURL := fmt.Sprintf("http://127.0.0.1:%d", info.APIPort)
	cdpHTTPURL := fmt.Sprintf("http://127.0.0.1:%d/json/version", info.CDPPort)

	start = time.Now()
	preCDPMs, err := waitReady(ctx, apiURL, cdpHTTPURL, cfg.timeout)
	res.ReadyMs = time.Since(start).Milliseconds()
	res.PreCDPMs = preCDPMs
	if err != nil {
		res.Error = fmt.Sprintf("wait ready: %v", err)
		return res
	}

	start = time.Now()
	status, body, err := postConfigureDisplay(ctx, apiURL, cfg.width, cfg.height, cfg.refreshRate, cfg.timeout)
	res.ConfigureMs = time.Since(start).Milliseconds()
	res.ConfigureStatus = status
	if err != nil {
		res.Error = fmt.Sprintf("configure request: %v", err)
		return res
	}
	if status < 200 || status >= 300 {
		res.Error = fmt.Sprintf("configure status %d: %s", status, strings.TrimSpace(string(body)))
		return res
	}

	start = time.Now()
	xrandrW, xrandrH, windowW, windowH, maximized, err := verifyViewportResized(ctx, info, cfg.width, cfg.height)
	res.VerifyMs = time.Since(start).Milliseconds()
	res.XrandrWidth = xrandrW
	res.XrandrHeight = xrandrH
	res.WindowWidth = windowW
	res.WindowHeight = windowH
	res.WindowMaximized = maximized
	if err != nil {
		res.Error = fmt.Sprintf("verify viewport resize: %v", err)
		return res
	}

	start = time.Now()
	wsURL, err := getCDPWebSocketURL(ctx, cdpHTTPURL)
	if err != nil {
		res.Error = fmt.Sprintf("post-config cdp url: %v", err)
		return res
	}
	if err := cdpGetVersion(ctx, wsURL, 10*time.Second); err != nil {
		res.Error = fmt.Sprintf("post-config cdp op: %v", err)
		return res
	}
	res.PostCDPMs = time.Since(start).Milliseconds()
	return res
}

func startContainer(ctx context.Context, cfg config, name string) (containerInfo, error) {
	args := []string{
		"run", "-d",
		"--name", name,
		"--privileged",
		"--tmpfs", "/dev/shm:size=2g",
		"--memory", "8192m",
		"-p", "127.0.0.1::10001",
		"-p", "127.0.0.1::9222",
		"-p", "127.0.0.1::9224",
		"-e", "DISPLAY_NUM=1",
		"-e", "HEIGHT=1080",
		"-e", "WIDTH=1920",
		"-e", "TZ=America/Los_Angeles",
		"-e", "RUN_AS_ROOT=false",
		"-e", "ENABLE_WEBRTC=true",
		"-e", "CHROMIUM_FLAGS=--user-data-dir=/home/kernel/user-data --disable-dev-shm-usage --disable-gpu --start-maximized --disable-software-rasterizer --remote-allow-origins=*",
		cfg.image,
	}
	out, err := runOutput(ctx, "docker", args...)
	if err != nil {
		return containerInfo{}, err
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		return containerInfo{}, fmt.Errorf("docker run returned empty container id")
	}
	info := containerInfo{ID: id, Name: name}
	if err := waitForDockerPorts(ctx, &info, cfg.timeout); err != nil {
		return info, err
	}
	return info, nil
}

func waitForDockerPorts(ctx context.Context, info *containerInfo, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		apiPort, err := dockerHostPort(ctx, info.ID, "10001/tcp")
		if err != nil {
			lastErr = err
			time.Sleep(100 * time.Millisecond)
			continue
		}
		cdpPort, err := dockerHostPort(ctx, info.ID, "9222/tcp")
		if err != nil {
			lastErr = err
			time.Sleep(100 * time.Millisecond)
			continue
		}
		chromedriverPort, err := dockerHostPort(ctx, info.ID, "9224/tcp")
		if err != nil {
			lastErr = err
			time.Sleep(100 * time.Millisecond)
			continue
		}
		info.APIPort = apiPort
		info.CDPPort = cdpPort
		info.ChromedriverPort = chromedriverPort
		return nil
	}
	return fmt.Errorf("ports not assigned after %s: %v", timeout, lastErr)
}

func dockerHostPort(ctx context.Context, containerID, privatePort string) (int, error) {
	out, err := runOutput(ctx, "docker", "port", containerID, privatePort)
	if err != nil {
		return 0, err
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return 0, fmt.Errorf("empty docker port output for %s", privatePort)
	}
	if i := strings.LastIndex(line, ":"); i >= 0 {
		line = line[i+1:]
	}
	port, err := strconv.Atoi(line)
	if err != nil {
		return 0, fmt.Errorf("parse docker port %q: %w", line, err)
	}
	return port, nil
}

func waitReady(ctx context.Context, apiURL, cdpHTTPURL string, timeout time.Duration) (int64, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := apiExecTrue(ctx, apiURL); err != nil {
			lastErr = fmt.Errorf("api: %w", err)
			time.Sleep(500 * time.Millisecond)
			continue
		}
		wsURL, err := getCDPWebSocketURL(ctx, cdpHTTPURL)
		if err != nil {
			lastErr = fmt.Errorf("cdp http: %w", err)
			time.Sleep(500 * time.Millisecond)
			continue
		}
		start := time.Now()
		if err := cdpGetVersion(ctx, wsURL, 5*time.Second); err != nil {
			lastErr = fmt.Errorf("cdp ws: %w", err)
			time.Sleep(500 * time.Millisecond)
			continue
		}
		return time.Since(start).Milliseconds(), nil
	}
	return 0, fmt.Errorf("not ready after %s: %v", timeout, lastErr)
}

func postConfigureDisplay(ctx context.Context, apiURL string, width, height, refreshRate int, timeout time.Duration) (int, []byte, error) {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	display := map[string]any{
		"width":            width,
		"height":           height,
		"refresh_rate":     refreshRate,
		"restart_chromium": true,
		"require_idle":     true,
	}
	displayJSON, err := json.Marshal(display)
	if err != nil {
		return 0, nil, err
	}
	if err := writer.WriteField("display", string(displayJSON)); err != nil {
		return 0, nil, err
	}
	if err := writer.Close(); err != nil {
		return 0, nil, err
	}

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, apiURL+"/configure", &buf)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return resp.StatusCode, nil, readErr
	}
	return resp.StatusCode, body, nil
}

func apiExecTrue(ctx context.Context, apiURL string) error {
	body := bytes.NewBufferString(`{"command":"true"}`)
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, apiURL+"/process/exec", body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

func getCDPWebSocketURL(ctx context.Context, cdpHTTPURL string) (string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, cdpHTTPURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var version struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.Unmarshal(body, &version); err != nil {
		return "", err
	}
	if version.WebSocketDebuggerURL == "" {
		return "", fmt.Errorf("missing webSocketDebuggerUrl in /json/version")
	}
	return version.WebSocketDebuggerURL, nil
}

func cdpGetVersion(ctx context.Context, wsURL string, timeout time.Duration) error {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	client, err := cdpclient.Dial(reqCtx, wsURL)
	if err != nil {
		return err
	}
	defer client.Close()
	_, err = client.GetBrowserVersion(reqCtx)
	return err
}

func verifyViewportResized(ctx context.Context, info containerInfo, wantWidth, wantHeight int) (xrandrW, xrandrH, windowW, windowH int, maximized bool, err error) {
	xrandrW, xrandrH, err = readXrandrScreenSize(ctx, info)
	if err != nil {
		return xrandrW, xrandrH, windowW, windowH, maximized, err
	}
	windowW, windowH, maximized, err = readLargestChromiumWindowSize(ctx, info)
	if err != nil {
		return xrandrW, xrandrH, windowW, windowH, maximized, err
	}
	if xrandrW != wantWidth || xrandrH != wantHeight {
		return xrandrW, xrandrH, windowW, windowH, maximized, fmt.Errorf("xrandr screen is %dx%d, want %dx%d", xrandrW, xrandrH, wantWidth, wantHeight)
	}
	if windowW != wantWidth || windowH != wantHeight {
		return xrandrW, xrandrH, windowW, windowH, maximized, fmt.Errorf("largest Chromium window is %dx%d, want %dx%d", windowW, windowH, wantWidth, wantHeight)
	}
	if !maximized {
		return xrandrW, xrandrH, windowW, windowH, maximized, fmt.Errorf("largest Chromium window is not maximized")
	}
	return xrandrW, xrandrH, windowW, windowH, maximized, nil
}

func readXrandrScreenSize(ctx context.Context, info containerInfo) (int, int, error) {
	out, err := runOutput(ctx, "docker", "exec", info.ID, "bash", "-lc", `DISPLAY=:1 xrandr --query | awk '/^Screen / { gsub(/,/, "", $10); print $8, $10; exit }'`)
	if err != nil {
		return 0, 0, err
	}
	return parseTwoInts(strings.TrimSpace(string(out)), "xrandr screen size")
}

func readLargestChromiumWindowSize(ctx context.Context, info containerInfo) (int, int, bool, error) {
	const script = `export DISPLAY=:1
best_id=""
best_w=0
best_h=0
best_area=-1
for wid in $(xdotool search --class chromium 2>/dev/null || true); do
  geom="$(xdotool getwindowgeometry --shell "$wid" 2>/dev/null || true)"
  WIDTH=0
  HEIGHT=0
  eval "$geom"
  area=$((WIDTH * HEIGHT))
  if [ "$area" -gt "$best_area" ]; then
    best_area=$area
    best_id=$wid
    best_w=$WIDTH
    best_h=$HEIGHT
  fi
done
if [ "$best_area" -lt 0 ]; then
  echo "no Chromium windows found" >&2
  exit 1
fi
if ! state="$(xprop -id "$best_id" _NET_WM_STATE 2>/dev/null)"; then
  echo "failed to read Chromium _NET_WM_STATE with xprop" >&2
  exit 1
fi
maximized=0
case "$state" in
  *_NET_WM_STATE_MAXIMIZED_VERT*_NET_WM_STATE_MAXIMIZED_HORZ*|*_NET_WM_STATE_MAXIMIZED_HORZ*_NET_WM_STATE_MAXIMIZED_VERT*) maximized=1 ;;
esac
echo "$best_w $best_h $maximized"`
	out, err := runOutput(ctx, "docker", "exec", info.ID, "bash", "-lc", script)
	if err != nil {
		return 0, 0, false, err
	}
	width, height, maximized, err := parseWindowGeometry(strings.TrimSpace(string(out)))
	return width, height, maximized, err
}

func parseTwoInts(s, label string) (int, int, error) {
	fields := strings.Fields(s)
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("parse %s: expected two fields, got %q", label, s)
	}
	first, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, 0, fmt.Errorf("parse %s first field %q: %w", label, fields[0], err)
	}
	second, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, fmt.Errorf("parse %s second field %q: %w", label, fields[1], err)
	}
	return first, second, nil
}

func parseWindowGeometry(s string) (int, int, bool, error) {
	fields := strings.Fields(s)
	if len(fields) != 3 {
		return 0, 0, false, fmt.Errorf("parse Chromium window geometry: expected three fields, got %q", s)
	}
	width, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, 0, false, fmt.Errorf("parse Chromium window width %q: %w", fields[0], err)
	}
	height, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, false, fmt.Errorf("parse Chromium window height %q: %w", fields[1], err)
	}
	maximized, err := strconv.Atoi(fields[2])
	if err != nil {
		return 0, 0, false, fmt.Errorf("parse Chromium maximized flag %q: %w", fields[2], err)
	}
	return width, height, maximized == 1, nil
}

func saveDockerOutput(ctx context.Context, outDir, subdir string, iter int, info containerInfo, args ...string) string {
	path := filepath.Join(outDir, subdir, artifactName(iter, info, ".txt"))
	if subdir == "inspect" {
		path = filepath.Join(outDir, subdir, artifactName(iter, info, ".json"))
	}
	out, err := runOutput(ctx, args[0], args[1:]...)
	if err != nil {
		out = append(out, []byte("\nERROR: "+err.Error()+"\n")...)
	}
	_ = os.WriteFile(path, out, 0o644)
	return path
}

func saveTelemetry(ctx context.Context, outDir string, iter int, info containerInfo) string {
	path := filepath.Join(outDir, "telemetry", artifactName(iter, info, ".json"))
	telemetry := map[string]any{
		"container":    info,
		"collected_at": time.Now().Format(time.RFC3339Nano),
	}
	commands := map[string][]string{
		"supervisor_status":  {"docker", "exec", info.ID, "supervisorctl", "-c", "/etc/supervisor/supervisord.conf", "status"},
		"chromium_processes": {"docker", "exec", info.ID, "ps", "-C", "chromium", "-o", "pid,ppid,pgid,sid,stat,comm,args"},
		"xrandr":             {"docker", "exec", info.ID, "bash", "-lc", "DISPLAY=:1 xrandr"},
		"api_display":        {"docker", "exec", info.ID, "bash", "-lc", "curl -fsS http://127.0.0.1:10001/display || true"},
		"cdp_version":        {"docker", "exec", info.ID, "bash", "-lc", "curl -fsS http://127.0.0.1:9222/json/version || true"},
	}
	for key, cmd := range commands {
		out, err := runOutput(ctx, cmd[0], cmd[1:]...)
		telemetry[key] = map[string]string{
			"output": string(out),
			"error":  errorString(err),
		}
	}
	b, err := json.MarshalIndent(telemetry, "", "  ")
	if err != nil {
		_ = os.WriteFile(path, []byte(err.Error()), 0o644)
		return path
	}
	_ = os.WriteFile(path, b, 0o644)
	return path
}

func artifactName(iter int, info containerInfo, ext string) string {
	return fmt.Sprintf("iter-%03d-%s%s", iter, shortID(info.ID), ext)
}

func printSummary(cfg config, results []iterationResult) {
	ready := make([]int64, 0, len(results))
	configure := make([]int64, 0, len(results))
	verify := make([]int64, 0, len(results))
	postCDP := make([]int64, 0, len(results))
	failures := 0
	for _, r := range results {
		if r.Error != "" {
			failures++
			continue
		}
		ready = append(ready, r.ReadyMs)
		configure = append(configure, r.ConfigureMs)
		verify = append(verify, r.VerifyMs)
		postCDP = append(postCDP, r.PostCDPMs)
	}

	summaryPath := filepath.Join(cfg.outDir, "summary.json")
	summary := map[string]any{
		"total":    len(results),
		"success":  len(configure),
		"failures": failures,
		"out_dir":  cfg.outDir,
		"results":  results,
	}
	if b, err := json.MarshalIndent(summary, "", "  "); err == nil {
		_ = os.WriteFile(summaryPath, b, 0o644)
	}

	fmt.Printf("\nSummary: total=%d success=%d failures=%d out=%s\n", len(results), len(configure), failures, cfg.outDir)
	printLatencySummary("ready_ms", ready)
	printLatencySummary("configure_ms", configure)
	printLatencySummary("verify_ms", verify)
	printLatencySummary("post_cdp_ms", postCDP)
	if failures > 0 {
		fmt.Println("\nFailures:")
		for _, r := range results {
			if r.Error != "" {
				fmt.Printf("  iter=%03d container=%s status=%d error=%s log=%s telemetry=%s\n",
					r.Iteration, shortID(r.Container.ID), r.ConfigureStatus, r.Error, r.LogPath, r.TelemetryPath)
			}
		}
		os.Exit(1)
	}
}

func printLatencySummary(name string, values []int64) {
	if len(values) == 0 {
		fmt.Printf("%s: no successful samples\n", name)
		return
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	sum := int64(0)
	for _, v := range values {
		sum += v
	}
	fmt.Printf("%s: min=%d median=%d p95=%d p99=%d max=%d mean=%.1f\n",
		name,
		values[0],
		percentile(values, 50),
		percentile(values, 95),
		percentile(values, 99),
		values[len(values)-1],
		float64(sum)/float64(len(values)),
	)
}

func percentile(sorted []int64, p int) int64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := ((len(sorted) - 1) * p) / 100
	return sorted[idx]
}

func runStream(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func runOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func shortID(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
