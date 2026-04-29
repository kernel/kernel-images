package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultImage       = "kernel-cdpmonitor-review:latest"
	defaultContainer   = "kernel-cdpmonitor-review"
	defaultNgrokDomain = "raf-kernel-images.ngrok.app"
	defaultLogFile     = "cdpmonitor-review.log"
)

var captureCategories = []string{"console", "network", "page", "interaction", "system"}
var logFiles = []string{"console.log", "network.log", "page.log", "interaction.log", "system.log"}

type config struct {
	image       string
	container   string
	ngrokDomain string
	ngrokBin    string
	skipBuild   bool
	keep        bool
	raw         bool
	dataLimit   int
	logFile     string
}

type ports struct {
	API      int
	CDP      int
	AuxCDP   int
	Liveview int
}

type envelope struct {
	CaptureSessionID string `json:"capture_session_id"`
	Seq              uint64 `json:"seq"`
	Event            event  `json:"event"`
}

type event struct {
	Ts        int64           `json:"ts"`
	Type      string          `json:"type"`
	Category  string          `json:"category"`
	Source    source          `json:"source"`
	Data      json.RawMessage `json:"data,omitempty"`
	Truncated bool            `json:"truncated,omitempty"`
}

type source struct {
	Kind     string            `json:"kind"`
	Event    string            `json:"event,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

type managedProcess struct {
	cmd  *exec.Cmd
	done chan error
}

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.w.Write(p)
}

var output = &lockedWriter{w: os.Stdout}

func main() {
	cfg := parseFlags()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg); err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		_, _ = fmt.Fprintf(output, "cdpmonitor-review: %v\n", err)
		os.Exit(1)
	}
}

func parseFlags() config {
	cfg := config{}
	flag.StringVar(&cfg.image, "image", defaultImage, "Docker image tag to build and run")
	flag.StringVar(&cfg.container, "container", defaultContainer, "Docker container name")
	flag.StringVar(&cfg.ngrokDomain, "ngrok-domain", defaultNgrokDomain, "Reserved ngrok domain for the live view")
	flag.StringVar(&cfg.ngrokBin, "ngrok-bin", "ngrok", "ngrok executable")
	flag.BoolVar(&cfg.skipBuild, "skip-build", false, "Skip docker build and run the existing image tag")
	flag.BoolVar(&cfg.keep, "keep-container", false, "Leave the review container running on exit")
	flag.BoolVar(&cfg.raw, "raw", false, "Print redacted raw event envelopes instead of compact summaries")
	flag.IntVar(&cfg.dataLimit, "data-limit", 1200, "Maximum characters of event data to print per line")
	flag.StringVar(&cfg.logFile, "log-file", defaultLogFile, "File to overwrite with a copy of the run output; relative paths are resolved from the repo root")
	flag.Parse()
	return cfg
}

func run(ctx context.Context, cfg config) error {
	repoRoot, err := findRepoRoot()
	if err != nil {
		return err
	}
	closeLog, logPath, err := configureRunLog(repoRoot, cfg.logFile)
	if err != nil {
		return err
	}
	defer closeLog()
	if logPath != "" {
		outPrintf("Writing a copy of this run to %s\n", logPath)
	}

	if !cfg.skipBuild {
		if err := runLoggedCmd(ctx, repoRoot, "docker", "build", "-f", "images/chromium-headful/Dockerfile", "-t", cfg.image, "."); err != nil {
			return err
		}
	}

	_ = runLoggedCmd(context.Background(), repoRoot, "docker", "rm", "-f", cfg.container)

	if err := runLoggedCmd(ctx, repoRoot, "docker", dockerRunArgs(cfg)...); err != nil {
		return err
	}

	var apiPort int
	defer func() {
		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancelCleanup()
		_ = stopCapture(cleanupCtx, apiPort)
		if !cfg.keep {
			_ = runLoggedCmd(cleanupCtx, repoRoot, "docker", "rm", "-f", cfg.container)
		}
	}()

	mapped, err := inspectPorts(ctx, cfg.container)
	if err != nil {
		return err
	}
	apiPort = mapped.API

	apiBase := fmt.Sprintf("http://127.0.0.1:%d", mapped.API)
	if err := waitForHTTP(ctx, apiBase+"/spec.json", 90*time.Second); err != nil {
		return err
	}
	if err := waitForTCP(ctx, "127.0.0.1", mapped.Liveview, 30*time.Second); err != nil {
		return fmt.Errorf("wait for live view: %w", err)
	}

	ngrokCmd, err := startNgrok(ctx, cfg, mapped.Liveview)
	if err != nil {
		return err
	}
	defer stopProcess(ngrokCmd)

	if err := startCapture(ctx, apiBase); err != nil {
		return err
	}

	tailCtx, stopTail := context.WithCancel(ctx)
	defer stopTail()
	tailErr := make(chan error, 1)
	go func() {
		tailErr <- streamEvents(tailCtx, cfg, cfg.container)
	}()

	printReady(cfg, mapped, apiBase)

	select {
	case <-ctx.Done():
		return nil
	case err := <-ngrokCmd.done:
		if err != nil {
			return fmt.Errorf("ngrok exited: %w", err)
		}
		return fmt.Errorf("ngrok exited")
	case err := <-tailErr:
		return err
	}
}

func dockerRunArgs(cfg config) []string {
	args := []string{
		"run", "-d",
		"--name", cfg.container,
		"--privileged",
		"--tmpfs", "/dev/shm:size=2g",
		"--memory", "8192m",
		"-p", "127.0.0.1::10001",
		"-p", "127.0.0.1::9222",
		"-p", "127.0.0.1::9224",
		"-p", "127.0.0.1::8080",
		"-e", "ENABLE_WEBRTC=true",
		"-e", "DISPLAY_NUM=1",
		"-e", "HEIGHT=1080",
		"-e", "WIDTH=1920",
		"-e", "RUN_AS_ROOT=false",
		"-e", "TZ=America/Los_Angeles",
	}
	if iceServers := os.Getenv("NEKO_ICESERVERS"); iceServers != "" {
		args = append(args, "-e", "NEKO_ICESERVERS="+iceServers)
	} else {
		// Match the local headful runner defaults for direct localhost access.
		args = append(args,
			"-e", "NEKO_WEBRTC_EPR=56000-56100",
			"-e", "NEKO_WEBRTC_NAT1TO1=127.0.0.1",
			"-p", "56000-56100:56000-56100/udp",
		)
	}
	args = append(args, cfg.image)
	return args
}

func findRepoRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for dir := wd; ; dir = filepath.Dir(dir) {
		if fileExists(filepath.Join(dir, "images", "chromium-headful", "Dockerfile")) &&
			fileExists(filepath.Join(dir, "server", "go.mod")) {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("could not find repo root from %s", wd)
		}
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func configureRunLog(repoRoot, logFile string) (func(), string, error) {
	if logFile == "" {
		return func() {}, "", nil
	}
	path := logFile
	if !filepath.IsAbs(path) {
		path = filepath.Join(repoRoot, path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, "", fmt.Errorf("create log directory: %w", err)
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, "", fmt.Errorf("create log file %s: %w", path, err)
	}
	output.mu.Lock()
	output.w = io.MultiWriter(os.Stdout, f)
	output.mu.Unlock()
	return func() {
		output.mu.Lock()
		output.w = os.Stdout
		output.mu.Unlock()
		_ = f.Close()
	}, path, nil
}

func outPrintf(format string, args ...any) {
	_, _ = fmt.Fprintf(output, format, args...)
}

func outPrintln(args ...any) {
	_, _ = fmt.Fprintln(output, args...)
}

func runLoggedCmd(ctx context.Context, dir, name string, args ...string) error {
	outPrintf("$ %s %s\n", name, strings.Join(args, " "))
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s failed: %w", name, err)
	}
	return nil
}

func runOutput(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var out bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s %s failed: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

func inspectPorts(ctx context.Context, container string) (ports, error) {
	var out ports
	for containerPort, dest := range map[string]*int{
		"10001/tcp": &out.API,
		"9222/tcp":  &out.CDP,
		"9224/tcp":  &out.AuxCDP,
		"8080/tcp":  &out.Liveview,
	} {
		text, err := runOutput(ctx, "docker", "port", container, containerPort)
		if err != nil {
			return out, err
		}
		port, err := parseDockerPort(text)
		if err != nil {
			return out, fmt.Errorf("parse docker port %s=%q: %w", containerPort, text, err)
		}
		*dest = port
	}
	return out, nil
}

func parseDockerPort(text string) (int, error) {
	line := strings.TrimSpace(strings.Split(text, "\n")[0])
	if line == "" {
		return 0, fmt.Errorf("empty docker port output")
	}
	_, portText, err := net.SplitHostPort(line)
	if err != nil {
		idx := strings.LastIndex(line, ":")
		if idx < 0 {
			return 0, err
		}
		portText = line[idx+1:]
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return 0, err
	}
	return port, nil
}

func waitForHTTP(ctx context.Context, url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := http.Client{Timeout: 2 * time.Second}
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 500 {
				return nil
			}
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("timeout waiting for %s: %w", url, err)
			}
			return fmt.Errorf("timeout waiting for %s", url)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func waitForTCP(ctx context.Context, host string, port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	for {
		var d net.Dialer
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func startNgrok(ctx context.Context, cfg config, liveviewPort int) (*managedProcess, error) {
	if _, err := exec.LookPath(cfg.ngrokBin); err != nil {
		return nil, fmt.Errorf("ngrok executable %q not found: %w", cfg.ngrokBin, err)
	}
	args := []string{"http", "--url=" + cfg.ngrokDomain, strconv.Itoa(liveviewPort)}
	outPrintf("$ %s %s\n", cfg.ngrokBin, strings.Join(args, " "))
	cmd := exec.CommandContext(ctx, cfg.ngrokBin, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ngrok: %w", err)
	}
	go prefixOutput("ngrok", stdout)
	go prefixOutput("ngrok", stderr)

	proc := &managedProcess{
		cmd:  cmd,
		done: make(chan error, 1),
	}
	go func() {
		proc.done <- cmd.Wait()
		close(proc.done)
	}()

	select {
	case err := <-proc.done:
		if err != nil {
			return nil, fmt.Errorf("ngrok exited early: %w", err)
		}
		return nil, fmt.Errorf("ngrok exited early")
	case <-time.After(1200 * time.Millisecond):
	}
	return proc, nil
}

func prefixOutput(prefix string, r io.Reader) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		outPrintf("[%s] %s\n", prefix, scanner.Text())
	}
}

func stopProcess(proc *managedProcess) {
	if proc == nil || proc.cmd == nil || proc.cmd.Process == nil {
		return
	}
	_ = proc.cmd.Process.Signal(os.Interrupt)
	select {
	case <-proc.done:
	case <-time.After(3 * time.Second):
		_ = proc.cmd.Process.Kill()
		<-proc.done
	}
}

func startCapture(ctx context.Context, apiBase string) error {
	body := map[string]any{
		"config": map[string]any{
			"categories": captureCategories,
		},
	}
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiBase+"/events/capture_session", bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("start capture: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("start capture: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

func stopCapture(ctx context.Context, apiPort int) error {
	if apiPort == 0 {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, fmt.Sprintf("http://127.0.0.1:%d/events/capture_session", apiPort), nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}

func streamEvents(ctx context.Context, cfg config, container string) error {
	cmd := exec.CommandContext(ctx, "docker", "exec", container, "sh", "-lc", tailCommand())
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start event tailer: %w", err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		prefixOutput("tail", stderr)
	}()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var env envelope
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			outPrintf("[event?] %s\n", line)
			continue
		}
		outPrintln(formatEnvelope(env, cfg))
	}
	if err := scanner.Err(); err != nil && !errors.Is(ctx.Err(), context.Canceled) {
		return err
	}
	err = cmd.Wait()
	wg.Wait()
	if ctx.Err() == nil {
		if err != nil {
			return fmt.Errorf("event tailer exited: %w", err)
		}
		return fmt.Errorf("event tailer exited")
	}
	return nil
}

func tailCommand() string {
	paths := make([]string, 0, len(logFiles))
	for _, f := range logFiles {
		paths = append(paths, "/var/log/kernel/"+f)
	}
	return fmt.Sprintf("mkdir -p /var/log/kernel; touch %s; tail -q -n +1 -F %s",
		strings.Join(paths, " "),
		strings.Join(paths, " "),
	)
}

func formatEnvelope(env envelope, cfg config) string {
	if cfg.raw {
		redacted := redactScreenshot(env)
		b, err := marshalCompact(redacted)
		if err != nil {
			return fmt.Sprintf("seq=%d <marshal error: %v>", env.Seq, err)
		}
		return truncate(string(b), cfg.dataLimit)
	}

	ts := formatUnixMicros(env.Event.Ts)
	srcEvent := env.Event.Source.Event
	if srcEvent == "" {
		srcEvent = env.Event.Source.Kind
	}
	session := ""
	if env.Event.Source.Metadata != nil {
		if id := env.Event.Source.Metadata["cdp_session_id"]; id != "" {
			session = " session=" + id
		}
	}
	return fmt.Sprintf("%s seq=%-5d %-11s %-26s source=%s%s data=%s",
		ts,
		env.Seq,
		env.Event.Category,
		env.Event.Type,
		srcEvent,
		session,
		summarizeData(env.Event.Data, cfg.dataLimit),
	)
}

func formatUnixMicros(us int64) string {
	if us == 0 {
		return time.Now().Format("15:04:05.000")
	}
	return time.UnixMicro(us).Format("15:04:05.000")
}

func summarizeData(raw json.RawMessage, limit int) string {
	if len(raw) == 0 {
		return "{}"
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return truncate(compactJSON(raw), limit)
	}
	if png, ok := data["png"].(string); ok {
		data["png"] = fmt.Sprintf("<redacted %d base64 chars>", len(png))
	}
	trimLargeFields(data)
	b, err := marshalCompact(data)
	if err != nil {
		return truncate(compactJSON(raw), limit)
	}
	return truncate(string(b), limit)
}

func trimLargeFields(data map[string]any) {
	for _, key := range []string{"headers", "stack_trace"} {
		if value, ok := data[key]; ok {
			b, _ := json.Marshal(value)
			data[key] = fmt.Sprintf("<%s %d bytes>", key, len(b))
		}
	}
}

func compactJSON(raw []byte) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
}

func marshalCompact(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSpace(buf.Bytes()), nil
}

func redactScreenshot(env envelope) envelope {
	if len(env.Event.Data) == 0 {
		return env
	}
	var data map[string]any
	if err := json.Unmarshal(env.Event.Data, &data); err != nil {
		return env
	}
	if png, ok := data["png"].(string); ok {
		data["png"] = fmt.Sprintf("<redacted %d base64 chars>", len(png))
		if b, err := marshalCompact(data); err == nil {
			env.Event.Data = b
		}
	}
	return env
}

func truncate(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	return s[:limit] + "...[truncated]"
}

func printReady(cfg config, mapped ports, apiBase string) {
	outPrintln()
	outPrintln("CDP monitor review harness is running.")
	outPrintf("  container:      %s\n", cfg.container)
	outPrintf("  image:          %s\n", cfg.image)
	outPrintf("  live view:      https://%s\n", cfg.ngrokDomain)
	outPrintf("  local liveview: http://127.0.0.1:%d\n", mapped.Liveview)
	outPrintf("  local API:      %s\n", apiBase)
	outPrintf("  local CDP:      http://127.0.0.1:%d\n", mapped.CDP)
	outPrintf("  auxiliary CDP:  http://127.0.0.1:%d\n", mapped.AuxCDP)
	outPrintln()
	outPrintln("Open the live view, log in as admin/admin if prompted, then navigate/click/type/scroll manually.")
	outPrintln("Captured events will stream below. Press Ctrl-C to stop capture, ngrok, and the container.")
	outPrintln()
}

func init() {
	sort.Strings(captureCategories)
}
