package e2e

import (
	"context"
	"strings"
	"testing"

	instanceoapi "github.com/kernel/kernel-images/server/lib/oapi"
)

// TestContainer is the handle every e2e test uses to drive a browser instance.
//
// Historically this struct wrapped testcontainers-go directly. It is now a thin
// facade over a pluggable Backend (see backend.go), selected at construction
// time via the KI_E2E_BACKEND env var. The public method set is unchanged, so
// the ~24 e2e_*_test.go files that hold a *TestContainer continue to work
// without modification regardless of whether the instance runs as a local
// Docker container or a remote Hypeman VM.
type TestContainer struct {
	// Image is the OCI image reference under test.
	Image string

	tb      testing.TB
	backend Backend
}

// NewTestContainer creates a new test container handle backed by the configured
// backend. The actual instance is provisioned when Start() is called.
// Works with both *testing.T and *testing.B (any testing.TB).
func NewTestContainer(tb testing.TB, image string) *TestContainer {
	tb.Helper()
	return &TestContainer{
		Image:   image,
		tb:      tb,
		backend: newBackend(tb, image),
	}
}

// Start starts the instance with the given configuration.
//
// If the test requests HostAccess but the selected backend can't bridge the
// instance to the test host (e.g. the hypeman backend, which runs a remote VM),
// the test is skipped rather than failed: such tests rely on a fixture served
// from the runner's loopback that a remote instance cannot reach. This keeps the
// hypeman CI job green while preserving coverage on the Docker backend.
func (c *TestContainer) Start(ctx context.Context, cfg ContainerConfig) error {
	if cfg.HostAccess && !c.backend.SupportsHostAccess() {
		c.tb.Skipf("skipping host-access test: %s backend has no host-loopback bridge for the instance", backendKindFromEnv())
	}
	return c.backend.Start(ctx, cfg)
}

// Stop stops and removes the instance.
func (c *TestContainer) Stop(ctx context.Context) error {
	return c.backend.Stop(ctx)
}

// APIBaseURL returns the URL for the instance's API server.
func (c *TestContainer) APIBaseURL() string {
	return c.backend.APIBaseURL()
}

// CDPURL returns the WebSocket URL for the instance's DevTools proxy.
func (c *TestContainer) CDPURL() string {
	return c.backend.CDPURL()
}

// CDPAddr returns the TCP address for the instance's DevTools proxy.
func (c *TestContainer) CDPAddr() string {
	return c.backend.CDPAddr()
}

// ChromeDriverURL returns the base HTTP URL for the instance's ChromeDriver proxy.
func (c *TestContainer) ChromeDriverURL() string {
	return c.backend.ChromeDriverURL()
}

// ChromeDriverAddr returns the host:port for the instance's ChromeDriver proxy,
// derived from ChromeDriverURL (scheme stripped). Useful for substring
// assertions on proxy-rewritten URLs. Handles both http:// (docker) and
// https:// (hypeman ingress) ChromeDriver URLs.
func (c *TestContainer) ChromeDriverAddr() string {
	u := c.backend.ChromeDriverURL()
	if i := strings.Index(u, "://"); i >= 0 {
		return u[i+3:]
	}
	return u
}

// ChromeDriverWSURL returns the WebSocket URL for the instance's ChromeDriver
// proxy. The scheme matches the ChromeDriver endpoint's transport: wss:// when
// it's served over TLS (the hypeman ingress on :9224), ws:// otherwise (docker).
// path should include a leading slash.
func (c *TestContainer) ChromeDriverWSURL(path string) string {
	scheme := "ws"
	if strings.HasPrefix(c.backend.ChromeDriverURL(), "https://") {
		scheme = "wss"
	}
	return scheme + "://" + c.ChromeDriverAddr() + path
}

// APIClient creates an OpenAPI client for this instance's API.
func (c *TestContainer) APIClient() (*instanceoapi.ClientWithResponses, error) {
	return c.backend.APIClient()
}

// APIClientNoKeepAlive creates an API client that doesn't reuse connections.
// This is useful after server restarts where existing connections may be stale.
func (c *TestContainer) APIClientNoKeepAlive() (*instanceoapi.ClientWithResponses, error) {
	return c.backend.APIClientNoKeepAlive()
}

// WaitReady waits for the instance's API to become ready.
func (c *TestContainer) WaitReady(ctx context.Context) error {
	return c.backend.WaitReady(ctx)
}

// WaitDevTools waits for the CDP WebSocket endpoint to be ready.
func (c *TestContainer) WaitDevTools(ctx context.Context) error {
	return c.backend.WaitDevTools(ctx)
}

// WaitChromeDriver waits for the ChromeDriver proxy (and upstream ChromeDriver)
// to be ready.
func (c *TestContainer) WaitChromeDriver(ctx context.Context) error {
	return c.backend.WaitChromeDriver(ctx)
}

// Exec executes a command inside the instance and returns the exit code and
// combined output.
func (c *TestContainer) Exec(ctx context.Context, cmd []string) (int, string, error) {
	return c.backend.Exec(ctx, cmd)
}

// ExitCh returns a channel that receives when the instance exits.
func (c *TestContainer) ExitCh() <-chan error {
	return c.backend.ExitCh()
}
