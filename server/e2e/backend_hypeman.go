package e2e

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	hypeman "github.com/kernel/hypeman-go"
	"github.com/kernel/hypeman-go/option"
	instanceoapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/nrednav/cuid2"
)

// Guest ports exposed by the kernel-images browser image. They are fixed inside
// the guest; the Docker backend remaps them to random host ports, while the
// Hypeman backend reaches them either through an ingress (by listen port) or
// directly on the instance's private network IP.
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
	// names to attach for GPU/PCI passthrough.
	envHypemanGPUDevices = "KI_E2E_HYPEMAN_GPU_DEVICES"
	// envHypemanGPUProfile requests a vGPU profile (e.g. "NVIDIA L40S-2Q") for
	// the instance; the host assigns the backing mdev. Required to boot the vGPU
	// browser image (chromium-headful-vgpu).
	envHypemanGPUProfile = "KI_E2E_HYPEMAN_GPU_PROFILE"
	// envHypemanInstanceSize optionally overrides the VM memory size.
	envHypemanInstanceSize = "KI_E2E_HYPEMAN_SIZE"
	// envHypemanIngressDomain overrides the wildcard ingress base domain. If
	// unset it is derived from the base URL host by stripping a leading
	// "hypeman." prefix (e.g. hypeman.dev-yul-hypeman-1.kernel.sh ->
	// dev-yul-hypeman-1.kernel.sh), matching the host's "{instance}.<domain>"
	// wildcard ingresses.
	envHypemanIngressDomain = "KI_E2E_HYPEMAN_INGRESS_DOMAIN"
	// envHypemanIngressTLS toggles TLS on ingress endpoints. Defaults to true
	// (the host terminates TLS with a wildcard cert); set 0/false for plaintext.
	envHypemanIngressTLS = "KI_E2E_HYPEMAN_INGRESS_TLS"
	// envHypemanRawIP forces reaching the instance on its private network IP
	// instead of via ingress. Only works from a network with L3 reachability to
	// the hypeman instance subnet (e.g. the API's own tailnet-tagged hosts).
	envHypemanRawIP = "KI_E2E_HYPEMAN_RAW_IP"
)

// ingressRole maps a logical endpoint to the ingress listen port and the guest
// target port. Hostname routing uses a single wildcard hostname
// "{instance}.<domain>" and differentiates roles by listen port, matching the
// host's existing convention (the browser API is exposed on :444 -> guest
// :10001). cdp/cd reuse the guest port as the listen port.
type ingressRole struct {
	role       string
	listenPort int64
	targetPort int64
}

var ingressRoles = []ingressRole{
	{role: "api", listenPort: 444, targetPort: hypemanAPIPort},
	{role: "cdp", listenPort: hypemanCDPPort, targetPort: hypemanCDPPort},
	{role: "cd", listenPort: hypemanChromeDriverPort, targetPort: hypemanChromeDriverPort},
}

// Tag applied to ingresses this backend creates, so they are recognizable as
// e2e-managed. We still reuse any pre-existing ingress (e.g. the API's own
// browser ingress) regardless of tag — matching is by rule shape.
const (
	ingressTagKey = "managed-by"
	ingressTagVal = "ki-e2e"
)

// hypemanBackend starts the image as a remote VM on a running Hypeman dev server
// using the github.com/kernel/hypeman-go client library.
//
// Endpoints are reached one of two ways:
//
//   - Ingress (default): a wildcard ingress per role routes
//     "<instance>.<domain>:<listenPort>" through the host's reverse proxy to the
//     instance's guest port. Each rule uses the "{instance}" hostname capture so
//     a single host-level ingress serves every instance; rules are found-or-
//     created (reusing pre-existing ones, e.g. the browser API :444 -> :10001).
//     Works from anywhere that can resolve <domain> and reach the host.
//   - Raw network IP (opt-in via KI_E2E_HYPEMAN_RAW_IP): the instance's private
//     IP on the fixed guest ports. Needs L3 reachability to the instance subnet.
//
// Command execution runs against the instance's own API server (/process/exec)
// so callers get the same (exitCode, combinedOutput, error) shape as Docker.
type hypemanBackend struct {
	client hypeman.Client
	image  string

	instanceID string
	name       string
	ip         string

	useIngress    bool
	ingressDomain string
	ingressTLS    bool

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

	baseURL := strings.TrimSpace(os.Getenv(envHypemanBaseURL))
	if baseURL == "" {
		baseURL = strings.TrimSpace(os.Getenv("HYPEMAN_BASE_URL"))
	}

	// Fail fast with an actionable message if neither this var nor the SDK's
	// native vars provide connection details.
	hasBase := baseURL != ""
	hasToken := strings.TrimSpace(os.Getenv(envHypemanToken)) != "" || strings.TrimSpace(os.Getenv("HYPEMAN_API_KEY")) != ""
	if !hasBase || !hasToken {
		return nil, fmt.Errorf(
			"hypeman backend requires a base URL (%s or HYPEMAN_BASE_URL) and a token (%s or HYPEMAN_API_KEY)",
			envHypemanBaseURL, envHypemanToken,
		)
	}

	domain := strings.TrimSpace(os.Getenv(envHypemanIngressDomain))
	if domain == "" {
		domain = deriveIngressDomain(baseURL)
	}
	rawIP := isTruthy(os.Getenv(envHypemanRawIP))

	return &hypemanBackend{
		client:        hypeman.NewClient(opts...),
		image:         image,
		useIngress:    !rawIP && domain != "",
		ingressDomain: domain,
		ingressTLS:    envBoolDefault(envHypemanIngressTLS, true),
		exitCh:        make(chan error, 1),
	}, nil
}

// deriveIngressDomain extracts the wildcard ingress base domain from the control
// API base URL by stripping a leading "hypeman." label.
func deriveIngressDomain(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	return strings.TrimPrefix(u.Hostname(), "hypeman.")
}

// Start creates and boots a hypeman instance for the image, waits for it to
// reach the Running state, then prepares the chosen routing mode.
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

	c.name = hypemanInstanceName()
	params := hypeman.InstanceNewParams{
		Image: c.image,
		Name:  c.name,
		Env:   env,
	}
	if size := strings.TrimSpace(os.Getenv(envHypemanInstanceSize)); size != "" {
		params.Size = hypeman.String(size)
	}
	if devs := parseCommaList(os.Getenv(envHypemanGPUDevices)); len(devs) > 0 {
		params.Devices = devs
	}
	if profile := strings.TrimSpace(os.Getenv(envHypemanGPUProfile)); profile != "" {
		params.GPU = hypeman.InstanceNewParamsGPU{Profile: hypeman.String(profile)}
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

	if c.useIngress {
		// Ensure the wildcard ingress rules exist; endpoints derive from the
		// instance name + domain, so no instance IP is needed.
		return c.ensureIngress(ctx)
	}

	// Raw-IP fallback: reach the instance directly on its private network IP.
	ip, err := c.resolveIP(ctx)
	if err != nil {
		return err
	}
	c.ip = ip
	return nil
}

// ensureIngress finds or creates a wildcard ingress for each role. Ingresses are
// host-level constructs keyed by rule shape (wildcard hostname + listen port ->
// target port), so we reuse any pre-existing rule (e.g. the API's browser
// ingress) and only create what's missing — never one ingress per instance.
func (c *hypemanBackend) ensureIngress(ctx context.Context) error {
	have := c.existingRuleSet(ctx)
	for _, r := range ingressRoles {
		key := ruleKey(c.wildcardHost(), r.listenPort, r.targetPort)
		if have[key] {
			continue
		}
		if _, err := c.client.Ingresses.New(ctx, c.roleIngressParams(r)); err != nil {
			// Another runner may have created it concurrently; re-check.
			if c.existingRuleSet(ctx)[key] {
				continue
			}
			return fmt.Errorf("hypeman: ensure ingress for role %q (:%d->:%d): %w", r.role, r.listenPort, r.targetPort, err)
		}
	}
	return nil
}

// existingRuleSet lists all ingresses and indexes their rules by shape so we can
// reuse any rule (regardless of ingress name/tag) that already provides routing.
func (c *hypemanBackend) existingRuleSet(ctx context.Context) map[string]bool {
	set := map[string]bool{}
	list, err := c.client.Ingresses.List(ctx, hypeman.IngressListParams{})
	if err != nil || list == nil {
		return set
	}
	for _, ing := range *list {
		for _, rule := range ing.Rules {
			set[ruleKey(rule.Match.Hostname, rule.Match.Port, rule.Target.Port)] = true
		}
	}
	return set
}

func (c *hypemanBackend) roleIngressParams(r ingressRole) hypeman.IngressNewParams {
	return hypeman.IngressNewParams{
		Name: "ki-e2e-" + r.role,
		Rules: []hypeman.IngressRuleParam{{
			Match: hypeman.IngressMatchParam{
				Hostname: c.wildcardHost(),
				Port:     hypeman.Int(r.listenPort),
			},
			Target: hypeman.IngressTargetParam{
				Instance: "{instance}",
				Port:     r.targetPort,
			},
			Tls: hypeman.Bool(c.ingressTLS),
		}},
		Tags: map[string]string{ingressTagKey: ingressTagVal},
	}
}

func ruleKey(host string, listen, target int64) string {
	return fmt.Sprintf("%s|%d|%d", host, listen, target)
}

// wildcardHost is the pattern hostname ("{instance}.<domain>") used in ingress
// rules; ingressHost is the concrete hostname for this instance.
func (c *hypemanBackend) wildcardHost() string { return "{instance}." + c.ingressDomain }
func (c *hypemanBackend) ingressHost() string  { return c.name + "." + c.ingressDomain }

func (c *hypemanBackend) listenPortFor(role string) int64 {
	for _, r := range ingressRoles {
		if r.role == role {
			return r.listenPort
		}
	}
	return 0
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

// Stop deletes the hypeman instance. The shared wildcard ingresses are
// host-level and intentionally left in place for reuse by other instances/runs.
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
	return c.httpScheme() + "://" + c.endpoint("api", hypemanAPIPort)
}

func (c *hypemanBackend) CDPURL() string {
	return c.wsScheme() + "://" + c.endpoint("cdp", hypemanCDPPort) + "/"
}

func (c *hypemanBackend) CDPAddr() string {
	return c.endpoint("cdp", hypemanCDPPort)
}

func (c *hypemanBackend) ChromeDriverURL() string {
	return c.httpScheme() + "://" + c.endpoint("cd", hypemanChromeDriverPort)
}

// endpoint returns the host:port a caller should dial for a role: the ingress
// hostname on the role's listen port when hostname routing is enabled, otherwise
// the instance's private IP on the fixed guest port.
func (c *hypemanBackend) endpoint(role string, guestPort int64) string {
	if c.useIngress {
		return fmt.Sprintf("%s:%d", c.ingressHost(), c.listenPortFor(role))
	}
	return fmt.Sprintf("%s:%d", c.ip, guestPort)
}

func (c *hypemanBackend) httpScheme() string {
	if c.useIngress && c.ingressTLS {
		return "https"
	}
	return "http"
}

func (c *hypemanBackend) wsScheme() string {
	if c.useIngress && c.ingressTLS {
		return "wss"
	}
	return "ws"
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

// hypemanInstanceName builds a DNS-safe, unique instance name. Hypeman requires
// lowercase letters, digits, and dashes only, not starting/ending with a dash.
func hypemanInstanceName() string {
	return "ki-e2e-" + strings.ToLower(cuid2.Generate())
}

// isTruthy reports whether an env value means "on" (1/true/yes, case-insensitive).
func isTruthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}

// envBoolDefault parses a boolean env var, returning def when unset/empty.
func envBoolDefault(name string, def bool) bool {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	return isTruthy(v)
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
