package e2e

import (
	"context"
	"strings"
	"testing"
)

// TestBackendKindFromEnv verifies the KI_E2E_BACKEND selection logic. These are
// cheap, infra-free unit tests safe to run in CI.
func TestBackendKindFromEnv(t *testing.T) {
	cases := []struct {
		name string
		set  bool
		val  string
		want BackendKind
	}{
		{name: "unset defaults to docker", set: false, want: BackendDocker},
		{name: "empty defaults to docker", set: true, val: "", want: BackendDocker},
		{name: "docker", set: true, val: "docker", want: BackendDocker},
		{name: "hypeman", set: true, val: "hypeman", want: BackendHypeman},
		{name: "case-insensitive + trimmed", set: true, val: "  HYPEMAN ", want: BackendHypeman},
		{name: "unknown passes through", set: true, val: "bogus", want: BackendKind("bogus")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv(envBackendKind, tc.val)
			} else {
				// t.Setenv requires a value; ensure the var is empty for the
				// "unset" case by setting it to empty, which the function
				// treats as the default.
				t.Setenv(envBackendKind, "")
			}
			if got := backendKindFromEnv(); got != tc.want {
				t.Fatalf("backendKindFromEnv() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestNewHypemanBackendRequiresConfig ensures the hypeman backend fails fast and
// with an actionable message when connection details are missing.
func TestNewHypemanBackendRequiresConfig(t *testing.T) {
	if _, err := newHypemanBackend("some/image:tag", hypemanConfig{}); err == nil {
		t.Fatal("expected error when base URL/token are empty, got nil")
	}
	if _, err := newHypemanBackend("some/image:tag", hypemanConfig{BaseURL: "http://x"}); err == nil {
		t.Fatal("expected error when token is empty, got nil")
	}
}

// TestNewHypemanBackendWithConfig ensures a valid config constructs a backend
// without error — and without reading the environment.
func TestNewHypemanBackendWithConfig(t *testing.T) {
	b, err := newHypemanBackend("some/image:tag", hypemanConfig{
		BaseURL: "http://hypeman.example.invalid:8080",
		Token:   "test-token-not-a-real-secret",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if b == nil {
		t.Fatal("expected non-nil backend")
	}
}

// TestHypemanConfigFromEnv verifies env resolution happens in one place: the
// SDK-native fallbacks, the TLS default, and the comma-split GPU devices.
func TestHypemanConfigFromEnv(t *testing.T) {
	t.Setenv(envHypemanBaseURL, "")
	t.Setenv("HYPEMAN_BASE_URL", "https://hypeman.dev-x.kernel.sh")
	t.Setenv(envHypemanToken, "")
	t.Setenv("HYPEMAN_API_KEY", "tok")
	t.Setenv(envHypemanIngressTLS, "")
	t.Setenv(envHypemanGPUDevices, "a, b ,c")
	t.Setenv(envHypemanGPUProfile, "NVIDIA L40S-2Q")

	cfg := hypemanConfigFromEnv()
	if cfg.BaseURL != "https://hypeman.dev-x.kernel.sh" {
		t.Errorf("BaseURL = %q (expected HYPEMAN_BASE_URL fallback)", cfg.BaseURL)
	}
	if cfg.Token != "tok" {
		t.Errorf("Token = %q (expected HYPEMAN_API_KEY fallback)", cfg.Token)
	}
	if !cfg.IngressTLS {
		t.Errorf("IngressTLS = false, want default true")
	}
	if len(cfg.GPUDevices) != 3 || cfg.GPUDevices[0] != "a" || cfg.GPUDevices[2] != "c" {
		t.Errorf("GPUDevices = %v, want [a b c]", cfg.GPUDevices)
	}
	if cfg.GPUProfile != "NVIDIA L40S-2Q" {
		t.Errorf("GPUProfile = %q", cfg.GPUProfile)
	}
}

// TestHypemanRawIPMode verifies endpoint derivation in the default raw-IP mode
// (no ingress domain): the private IP on the fixed guest ports.
func TestHypemanRawIPMode(t *testing.T) {
	b := &hypemanBackend{ip: "10.1.2.3"}
	for _, tc := range []struct{ name, got, want string }{
		{"api", b.APIBaseURL(), "http://10.1.2.3:10001"},
		{"cdp", b.CDPURL(), "ws://10.1.2.3:9222/"},
		{"cdpAddr", b.CDPAddr(), "10.1.2.3:9222"},
		{"cd", b.ChromeDriverURL(), "http://10.1.2.3:9224"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

// TestHypemanIngressRouting verifies hostname-routed endpoints (single wildcard
// hostname, roles differentiated by listen port, TLS) and the per-role ingress
// params. The instance name contains dashes, which must stay inside the single
// {instance} hostname label.
func TestHypemanIngressRouting(t *testing.T) {
	const domain = "dev-yul-hypeman-1.kernel.sh"
	b := &hypemanBackend{name: "ki-e2e-abc123", useIngress: true, ingressDomain: domain, ingressTLS: true}
	for _, tc := range []struct{ name, got, want string }{
		{"api", b.APIBaseURL(), "https://ki-e2e-abc123." + domain + ":444"},
		{"cdp", b.CDPURL(), "wss://ki-e2e-abc123." + domain + ":9222/"},
		{"cdpAddr", b.CDPAddr(), "ki-e2e-abc123." + domain + ":9222"},
		{"cd", b.ChromeDriverURL(), "https://ki-e2e-abc123." + domain + ":9224"},
		{"wildcard", b.wildcardHost(), "{instance}." + domain},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}

	// The "api" role reuses the host's :444 -> :10001 browser ingress shape.
	p := b.roleIngressParams(ingressRoles[0])
	if p.Name != "ki-e2e-api" {
		t.Errorf("ingress name = %q, want ki-e2e-api", p.Name)
	}
	if len(p.Rules) != 1 {
		t.Fatalf("got %d rules, want 1", len(p.Rules))
	}
	r := p.Rules[0]
	if r.Match.Hostname != "{instance}."+domain {
		t.Errorf("match hostname = %q", r.Match.Hostname)
	}
	if got := r.Match.Port.Or(0); got != 444 {
		t.Errorf("match port = %d, want 444", got)
	}
	if r.Target.Instance != "{instance}" || r.Target.Port != hypemanAPIPort {
		t.Errorf("target = %q:%d, want {instance}:%d", r.Target.Instance, r.Target.Port, hypemanAPIPort)
	}
}

// TestHypemanIngressPlaintext verifies http/ws when TLS is disabled.
func TestHypemanIngressPlaintext(t *testing.T) {
	b := &hypemanBackend{name: "x", useIngress: true, ingressDomain: "d", ingressTLS: false}
	if got, want := b.APIBaseURL(), "http://x.d:444"; got != want {
		t.Errorf("APIBaseURL = %q, want %q", got, want)
	}
	if got, want := b.CDPURL(), "ws://x.d:9222/"; got != want {
		t.Errorf("CDPURL = %q, want %q", got, want)
	}
}

// TestHypemanRejectsHostAccess verifies the hypeman backend refuses HostAccess
// (no host-loopback bridge for remote VMs) before doing any network I/O.
func TestHypemanRejectsHostAccess(t *testing.T) {
	b := &hypemanBackend{}
	err := b.Start(context.Background(), ContainerConfig{HostAccess: true})
	if err == nil || !strings.Contains(err.Error(), "HostAccess") {
		t.Fatalf("expected HostAccess rejection, got %v", err)
	}
}

// TestDeriveIngressDomain strips a leading "hypeman." from the control API host.
func TestDeriveIngressDomain(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://hypeman.dev-yul-hypeman-1.kernel.sh", "dev-yul-hypeman-1.kernel.sh"},
		{"https://dev-yul-hypeman-1.kernel.sh", "dev-yul-hypeman-1.kernel.sh"},
		{"", ""},
	} {
		if got := deriveIngressDomain(tc.in); got != tc.want {
			t.Errorf("deriveIngressDomain(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
