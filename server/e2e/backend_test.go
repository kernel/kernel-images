package e2e

import "testing"

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
	// Clear every env var the backend (and the SDK) consult.
	for _, k := range []string{envHypemanBaseURL, "HYPEMAN_BASE_URL", envHypemanToken, "HYPEMAN_API_KEY"} {
		t.Setenv(k, "")
	}
	if _, err := newHypemanBackend("some/image:tag"); err == nil {
		t.Fatal("expected error when hypeman base URL/token are unset, got nil")
	}
}

// TestNewHypemanBackendWithConfig ensures a valid configuration constructs a
// backend without error.
func TestNewHypemanBackendWithConfig(t *testing.T) {
	t.Setenv(envHypemanBaseURL, "http://hypeman.example.invalid:8080")
	t.Setenv(envHypemanToken, "test-token-not-a-real-secret")
	b, err := newHypemanBackend("some/image:tag")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if b == nil {
		t.Fatal("expected non-nil backend")
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

// TestHypemanIngressRouting verifies hostname-routed endpoints and that the
// shared ingress params describe one wildcard rule per role on the proxy's
// plaintext listen port. The instance name contains dashes, which must end up
// inside the {instance} capture, not split the role suffix.
func TestHypemanIngressRouting(t *testing.T) {
	b := &hypemanBackend{name: "ki-e2e-abc123", ingressDomain: "e2e.hypeman.dev"}
	for _, tc := range []struct{ name, got, want string }{
		{"api", b.APIBaseURL(), "http://ki-e2e-abc123-api.e2e.hypeman.dev:80"},
		{"cdp", b.CDPURL(), "ws://ki-e2e-abc123-cdp.e2e.hypeman.dev:80/"},
		{"cdpAddr", b.CDPAddr(), "ki-e2e-abc123-cdp.e2e.hypeman.dev:80"},
		{"cd", b.ChromeDriverURL(), "http://ki-e2e-abc123-cd.e2e.hypeman.dev:80"},
		{"pattern", b.ingressPatternHost("api"), "{instance}-api.e2e.hypeman.dev"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}

	p := b.desiredIngressParams()
	if p.Name != ingressName {
		t.Errorf("ingress name = %q, want %q", p.Name, ingressName)
	}
	if len(p.Rules) != len(ingressRoles) {
		t.Fatalf("got %d rules, want %d", len(p.Rules), len(ingressRoles))
	}
	if got := p.Rules[0].Target.Instance; got != "{instance}" {
		t.Errorf("rule[0] target instance = %q, want {instance}", got)
	}
	if got := p.Rules[0].Target.Port; got != hypemanAPIPort {
		t.Errorf("rule[0] target port = %d, want %d", got, hypemanAPIPort)
	}
}

// TestHypemanIngressTLS verifies https/wss + :443 when TLS is enabled.
func TestHypemanIngressTLS(t *testing.T) {
	b := &hypemanBackend{name: "x", ingressDomain: "d", ingressTLS: true}
	if got, want := b.APIBaseURL(), "https://x-api.d:443"; got != want {
		t.Errorf("APIBaseURL = %q, want %q", got, want)
	}
	if got, want := b.CDPURL(), "wss://x-cdp.d:443/"; got != want {
		t.Errorf("CDPURL = %q, want %q", got, want)
	}
}
