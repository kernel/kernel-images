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
	if b.Container() != nil {
		t.Fatal("hypeman backend Container() must be nil")
	}
}
