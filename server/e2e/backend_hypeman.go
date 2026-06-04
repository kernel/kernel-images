package e2e

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	hypeman "github.com/kernel/hypeman-go"
	"github.com/kernel/hypeman-go/option"
	instanceoapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/nrednav/cuid2"
	"github.com/testcontainers/testcontainers-go"
)

// Container ports exposed by the kernel-images browser image. These are fixed
// inside the guest; the Docker backend remaps them to random host ports, while
// the Hypeman backend reaches them directly on the instance's network IP.
const (
	hypemanAPIPort          = 10001
	hypemanCDPPort          = 9222
	hypemanChromeDriverPort = 9224
)

// Env var names for configuring the Hypeman backend. Secrets are referenced by
// name only and never hardcoded.
const (
	// envHypemanBaseURL overrides the hypeman dev server URL. If unset, the SDK
	// falls back to its own HYPEMAN_BASE_URL lookup.
	envHypemanBaseURL = "KI_E2E_HYPEMAN_BASE_URL"
	// envHypemanToken is the preferred auth token var. It matches the API
	// service's Railway staging variable name (HYPEMAN_AUTH_TOKEN). The SDK's
	// native HYPEMAN_API_KEY is also honored as a fallback.
	envHypemanToken = "HYPEMAN_AUTH_TOKEN"
	// envHypemanGPUDevices is an optional comma-separated list of device IDs or
	// names to attach for GPU passthrough (used by the GPU image).
	envHypemanGPUDevices = "KI_E2E_HYPEMAN_GPU_DEVICES"
	// envHypemanInstanceSize optionally overrides the VM memory size.
	envHypemanInstanceSize = "KI_E2E_HYPEMAN_SIZE"
)

// hypemanBackend starts the image as a remote VM on a running Hypeman dev server
// using the github.com/kernel/hypeman-go client library.
//
// The instance is reachable on its assigned network IP. All endpoint accessors
// target that IP on the fixed guest ports. Command execution is performed
// against the instance's own API server (/process/exec) so that callers get the
// same (exitCode, combinedOutput, error) shape as the Docker backend.
type hypemanBackend struct {
	client hypeman.Client
	image  string

	instanceID string
	ip         string

	exitCh chan error
}

// newHypemanBackend validates configuration and constructs a hypeman-backed
// Backend. The hypeman SDK reads HYPEMAN_BASE_URL / HYPEMAN_API_KEY from the
// environment; this constructor additionally wires the kernel-images-specific
// override vars (KI_E2E_HYPEMAN_BASE_URL, HYPEMAN_AUTH_TOKEN).
func newHypemanBackend(image string) (Backend, error) {
	var opts []option.RequestOption
	if base := strings.TrimSpace(os.Getenv(envHypemanBaseURL)); base != "" {
		opts = append(opts, option.WithBaseURL(base))
	}
	if token := strings.TrimSpace(os.Getenv(envHypemanToken)); token != "" {
		opts = append(opts, option.WithAPIKey(token))
	}

	// Fail fast with an actionable message if neither this var nor the SDK's
	// native vars provide connection details.
	hasBase := strings.TrimSpace(os.Getenv(envHypemanBaseURL)) != "" || strings.TrimSpace(os.Getenv("HYPEMAN_BASE_URL")) != ""
	hasToken := strings.TrimSpace(os.Getenv(envHypemanToken)) != "" || strings.TrimSpace(os.Getenv("HYPEMAN_API_KEY")) != ""
	if !hasBase || !hasToken {
		return nil, fmt.Errorf(
			"hypeman backend requires a base URL (%s or HYPEMAN_BASE_URL) and a token (%s or HYPEMAN_API_KEY)",
			envHypemanBaseURL, envHypemanToken,
		)
	}

	return &hypemanBackend{
		client: hypeman.NewClient(opts...),
		image:  image,
		exitCh: make(chan error, 1),
	}, nil
}

// Start creates and boots a hypeman instance for the image, waits for it to
// reach the Running state, and resolves its network IP.
func (c *hypemanBackend) Start(ctx context.Context, cfg ContainerConfig) error {
	env := make(map[string]string, len(cfg.Env)+1)
	for k, v := range cfg.Env {
		env[k] = v
	}
	// Mirror the Docker backend: ensure --no-sandbox is present for CI.
	if flags, ok := env["CHROMIUM_FLAGS"]; !ok || flags == "" {
		env["CHROMIUM_FLAGS"] = "--no-sandbox"
	} else {
		env["CHROMIUM_FLAGS"] = flags + " --no-sandbox"
	}

	params := hypeman.InstanceNewParams{
		Image: c.image,
		Name:  hypemanInstanceName(),
		Env:   env,
	}
	if size := strings.TrimSpace(os.Getenv(envHypemanInstanceSize)); size != "" {
		params.Size = hypeman.String(size)
	}
	if devs := parseCommaList(os.Getenv(envHypemanGPUDevices)); len(devs) > 0 {
		params.Devices = devs
	}

	inst, err := c.client.Instances.New(ctx, params)
	if err != nil {
		return fmt.Errorf("hypeman: create instance: %w", err)
	}
	c.instanceID = inst.ID

	// Wait for the guest program to start. The SDK caps the server-side wait at
	// a few minutes; loop until our context deadline if needed.
	if err := c.waitForRunning(ctx); err != nil {
		return err
	}

	ip, err := c.resolveIP(ctx)
	if err != nil {
		return err
	}
	c.ip = ip
	return nil
}

// waitForRunning polls the instance wait endpoint until the instance is Running
// or the context is done.
func (c *hypemanBackend) waitForRunning(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("hypeman: waiting for Running: %w", err)
		}
		resp, err := c.client.Instances.Wait(ctx, c.instanceID, hypeman.InstanceWaitParams{
			State:   hypeman.InstanceWaitParamsStateRunning,
			Timeout: hypeman.String("60s"),
		})
		if err == nil && resp != nil && string(resp.State) == string(hypeman.InstanceStateRunning) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("hypeman: timed out waiting for instance %s to reach Running", c.instanceID)
		case <-time.After(time.Second):
		}
	}
}

// resolveIP fetches the instance until a network IP is assigned.
func (c *hypemanBackend) resolveIP(ctx context.Context) (string, error) {
	for {
		inst, err := c.client.Instances.Get(ctx, c.instanceID)
		if err == nil && inst != nil && strings.TrimSpace(inst.Network.IP) != "" {
			return inst.Network.IP, nil
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("hypeman: timed out resolving IP for instance %s", c.instanceID)
		case <-time.After(time.Second):
		}
	}
}

// Stop deletes the hypeman instance.
func (c *hypemanBackend) Stop(ctx context.Context) error {
	if c.instanceID == "" {
		return nil
	}
	if err := c.client.Instances.Delete(ctx, c.instanceID); err != nil {
		return fmt.Errorf("hypeman: delete instance %s: %w", c.instanceID, err)
	}
	select {
	case c.exitCh <- nil:
	default:
	}
	return nil
}

func (c *hypemanBackend) APIBaseURL() string {
	return fmt.Sprintf("http://%s:%d", c.ip, hypemanAPIPort)
}

func (c *hypemanBackend) CDPURL() string {
	return fmt.Sprintf("ws://%s:%d/", c.ip, hypemanCDPPort)
}

func (c *hypemanBackend) CDPAddr() string {
	return fmt.Sprintf("%s:%d", c.ip, hypemanCDPPort)
}

func (c *hypemanBackend) ChromeDriverURL() string {
	return fmt.Sprintf("http://%s:%d", c.ip, hypemanChromeDriverPort)
}

func (c *hypemanBackend) APIClient() (*instanceoapi.ClientWithResponses, error) {
	return instanceoapi.NewClientWithResponses(c.APIBaseURL())
}

func (c *hypemanBackend) APIClientNoKeepAlive() (*instanceoapi.ClientWithResponses, error) {
	transport := &http.Transport{DisableKeepAlives: true}
	httpClient := &http.Client{Transport: transport}
	return instanceoapi.NewClientWithResponses(c.APIBaseURL(), instanceoapi.WithHTTPClient(httpClient))
}

// WaitReady polls the instance API server's /spec.yaml until it serves 200.
func (c *hypemanBackend) WaitReady(ctx context.Context) error {
	return pollHTTP200(ctx, c.APIBaseURL()+"/spec.yaml", 200*time.Millisecond)
}

// WaitDevTools polls the CDP TCP port until it accepts connections.
func (c *hypemanBackend) WaitDevTools(ctx context.Context) error {
	addr := c.CDPAddr()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		conn, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "tcp", addr)
		if err == nil {
			conn.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// WaitChromeDriver polls the ChromeDriver proxy /status until it serves 200.
func (c *hypemanBackend) WaitChromeDriver(ctx context.Context) error {
	return pollHTTP200(ctx, c.ChromeDriverURL()+"/status", 500*time.Millisecond)
}

// Exec runs a command inside the guest via the instance API server's
// /process/exec endpoint, returning the exit code and combined stdout+stderr.
//
// The Docker backend invokes commands as an argv slice; we map the first
// element to Command and the remainder to Args so callers like
// Exec(ctx, []string{"sh", "-lc", "..."}) behave identically.
func (c *hypemanBackend) Exec(ctx context.Context, cmd []string) (int, string, error) {
	if len(cmd) == 0 {
		return -1, "", fmt.Errorf("hypeman: empty command")
	}
	client, err := c.APIClient()
	if err != nil {
		return -1, "", err
	}

	body := instanceoapi.ProcessExecRequest{Command: cmd[0]}
	if len(cmd) > 1 {
		args := cmd[1:]
		body.Args = &args
	}

	resp, err := client.ProcessExecWithResponse(ctx, body)
	if err != nil {
		return -1, "", fmt.Errorf("hypeman: exec: %w", err)
	}
	if resp.JSON200 == nil {
		return -1, "", fmt.Errorf("hypeman: exec returned status %d: %s", resp.StatusCode(), string(resp.Body))
	}

	out := decodeB64(resp.JSON200.StdoutB64) + decodeB64(resp.JSON200.StderrB64)
	exitCode := 0
	if resp.JSON200.ExitCode != nil {
		exitCode = *resp.JSON200.ExitCode
	}
	return exitCode, out, nil
}

// ExitCh returns a channel that fires when the instance is stopped.
func (c *hypemanBackend) ExitCh() <-chan error {
	return c.exitCh
}

// Container returns nil: the hypeman backend is not Docker-based.
func (c *hypemanBackend) Container() testcontainers.Container {
	return nil
}

// hypemanInstanceName builds a DNS-safe, unique instance name. Hypeman requires
// lowercase letters, digits, and dashes only, not starting/ending with a dash.
func hypemanInstanceName() string {
	return "ki-e2e-" + strings.ToLower(cuid2.Generate())
}

func parseCommaList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func decodeB64(s *string) string {
	if s == nil || *s == "" {
		return ""
	}
	b, err := base64.StdEncoding.DecodeString(*s)
	if err != nil {
		return ""
	}
	return string(b)
}

// pollHTTP200 polls url until it returns HTTP 200 or ctx is done.
func pollHTTP200(ctx context.Context, url string, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	client := &http.Client{Timeout: 2 * time.Second}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				return err
			}
			resp, err := client.Do(req)
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return nil
				}
			}
		}
	}
}
