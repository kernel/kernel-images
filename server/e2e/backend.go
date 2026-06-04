package e2e

import (
	"context"
	"os"
	"strings"
	"testing"

	instanceoapi "github.com/kernel/kernel-images/server/lib/oapi"
)

// ContainerConfig holds optional configuration for instance startup.
//
// It is shared by every backend so that the ~24 e2e_*_test.go files can keep
// calling Start with the same shape regardless of where the browser instance
// actually runs (a local Docker container or a remote Hypeman VM).
type ContainerConfig struct {
	Env map[string]string
	// HostAccess requests that the browser instance be able to reach a service
	// the test stands up on its own host (loopback) — used by tests with a local
	// fixture server (capmonster, persisted-login). How it's provided is a
	// backend detail (the Docker backend maps host.docker.internal); backends
	// that cannot bridge a remote instance to the test host reject it.
	HostAccess bool
}

// Backend is the abstraction every e2e browser-instance provider implements.
//
// It captures the public surface that the test files consume via *TestContainer.
// Two implementations exist:
//
//   - dockerBackend: runs the image as a local Docker container via
//     testcontainers-go (the historical behavior, still the default).
//   - hypemanBackend: starts the image as a remote VM on a running Hypeman dev
//     server using the github.com/kernel/hypeman-go client library.
//
// Keeping the surface identical means selecting a backend is a pure factory
// concern and requires no changes in individual tests.
type Backend interface {
	// Start provisions and boots the browser instance.
	Start(ctx context.Context, cfg ContainerConfig) error
	// Stop tears the instance down and releases its resources.
	Stop(ctx context.Context) error

	// APIBaseURL returns the base URL for the instance's control-plane API
	// server (container port 10001).
	APIBaseURL() string
	// CDPURL returns the WebSocket URL for the DevTools proxy (port 9222).
	CDPURL() string
	// CDPAddr returns the TCP host:port for the DevTools proxy (port 9222).
	CDPAddr() string
	// ChromeDriverURL returns the base HTTP URL for the ChromeDriver proxy
	// (port 9224).
	ChromeDriverURL() string

	// APIClient returns an OpenAPI client bound to APIBaseURL.
	APIClient() (*instanceoapi.ClientWithResponses, error)
	// APIClientNoKeepAlive returns an OpenAPI client that disables HTTP
	// connection reuse (useful after server restarts).
	APIClientNoKeepAlive() (*instanceoapi.ClientWithResponses, error)

	// WaitReady blocks until the instance's API server is serving.
	WaitReady(ctx context.Context) error
	// WaitDevTools blocks until the CDP endpoint accepts connections.
	WaitDevTools(ctx context.Context) error
	// WaitChromeDriver blocks until the ChromeDriver proxy reports ready.
	WaitChromeDriver(ctx context.Context) error

	// Exec runs a command inside the instance and returns the exit code and
	// combined stdout+stderr output.
	Exec(ctx context.Context, cmd []string) (int, string, error)

	// ExitCh returns a channel that fires when the instance exits.
	ExitCh() <-chan error
}

// BackendKind enumerates the supported e2e backends.
type BackendKind string

const (
	BackendDocker  BackendKind = "docker"
	BackendHypeman BackendKind = "hypeman"
)

// envBackendKind is the env var that selects the backend. It defaults to
// "docker" so existing CI (which sets nothing) is unchanged.
const envBackendKind = "KI_E2E_BACKEND"

// backendKindFromEnv reads and normalizes KI_E2E_BACKEND, defaulting to docker.
func backendKindFromEnv() BackendKind {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(envBackendKind)))
	if v == "" {
		return BackendDocker
	}
	return BackendKind(v)
}

// newBackend constructs the backend selected by the KI_E2E_BACKEND env var.
//
// Selection is resolved here (and not per test) so that adding a backend never
// requires touching the test files. Unknown values fail the test loudly rather
// than silently falling back, to avoid masking misconfiguration in CI.
func newBackend(tb testing.TB, image string) Backend {
	tb.Helper()
	kind := backendKindFromEnv()
	switch kind {
	case BackendDocker:
		return newDockerBackend(image)
	case BackendHypeman:
		b, err := newHypemanBackend(image, hypemanConfigFromEnv())
		if err != nil {
			tb.Fatalf("e2e: failed to configure hypeman backend: %v", err)
		}
		return b
	default:
		tb.Fatalf("e2e: unsupported %s=%q (want %q or %q)", envBackendKind, kind, BackendDocker, BackendHypeman)
		return nil
	}
}
