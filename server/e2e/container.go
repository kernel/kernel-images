package e2e

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/go-connections/nat"
	instanceoapi "github.com/onkernel/kernel-images/server/lib/oapi"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// TestContainer wraps testcontainers-go to manage a Docker container for e2e tests.
// This enables parallel test execution by giving each test its own dynamically allocated ports.
type TestContainer struct {
	Name    string
	Image   string
	APIPort int // dynamically allocated host port -> container 10001
	CDPPort int // dynamically allocated host port -> container 9222
	ctr     testcontainers.Container
}

// ContainerConfig holds optional configuration for container startup.
type ContainerConfig struct {
	Env        map[string]string
	HostAccess bool // Add host.docker.internal mapping
}

// NewTestContainer creates a new test container placeholder.
// The actual container is started when Start() is called.
// Works with both *testing.T and *testing.B (any testing.TB).
func NewTestContainer(tb testing.TB, image string) *TestContainer {
	tb.Helper()
	return &TestContainer{
		Image: image,
	}
}

// Start starts the container with the given configuration using testcontainers-go.
func (c *TestContainer) Start(ctx context.Context, cfg ContainerConfig) error {
	// Build environment variables
	env := make(map[string]string)
	for k, v := range cfg.Env {
		env[k] = v
	}
	// Ensure CHROMIUM_FLAGS includes --no-sandbox for CI
	if flags, ok := env["CHROMIUM_FLAGS"]; !ok {
		env["CHROMIUM_FLAGS"] = "--no-sandbox"
	} else if flags != "" {
		env["CHROMIUM_FLAGS"] = flags + " --no-sandbox"
	} else {
		env["CHROMIUM_FLAGS"] = "--no-sandbox"
	}

	// Build container request options
	opts := []testcontainers.ContainerCustomizer{
		testcontainers.WithImage(c.Image),
		testcontainers.WithExposedPorts("10001/tcp", "9222/tcp"),
		testcontainers.WithEnv(env),
		testcontainers.WithTmpfs(map[string]string{"/dev/shm": "size=2g,mode=1777"}),
		// Set privileged mode for Chrome
		testcontainers.WithHostConfigModifier(func(hc *container.HostConfig) {
			hc.Privileged = true
		}),
		// Wait for the API to be ready
		testcontainers.WithWaitStrategy(
			wait.ForHTTP("/spec.yaml").
				WithPort("10001/tcp").
				WithStartupTimeout(2 * time.Minute),
		),
	}

	// Add host access if requested
	if cfg.HostAccess {
		opts = append(opts, testcontainers.WithHostConfigModifier(func(hc *container.HostConfig) {
			hc.ExtraHosts = append(hc.ExtraHosts, "host.docker.internal:host-gateway")
		}))
	}

	// Start container
	ctr, err := testcontainers.Run(ctx, c.Image, opts...)
	if err != nil {
		return fmt.Errorf("failed to start container: %w", err)
	}
	c.ctr = ctr

	// Get container name
	inspect, err := ctr.Inspect(ctx)
	if err == nil {
		c.Name = inspect.Name
	}

	// Get mapped ports
	apiPort, err := ctr.MappedPort(ctx, "10001/tcp")
	if err != nil {
		return fmt.Errorf("failed to get API port: %w", err)
	}
	c.APIPort = apiPort.Int()

	cdpPort, err := ctr.MappedPort(ctx, "9222/tcp")
	if err != nil {
		return fmt.Errorf("failed to get CDP port: %w", err)
	}
	c.CDPPort = cdpPort.Int()

	return nil
}

// Stop stops and removes the container.
func (c *TestContainer) Stop(ctx context.Context) error {
	if c.ctr == nil {
		return nil
	}
	return testcontainers.TerminateContainer(c.ctr)
}

// APIBaseURL returns the URL for the container's API server.
func (c *TestContainer) APIBaseURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d", c.APIPort)
}

// CDPURL returns the WebSocket URL for the container's DevTools proxy.
func (c *TestContainer) CDPURL() string {
	return fmt.Sprintf("ws://127.0.0.1:%d/", c.CDPPort)
}

// APIClient creates an OpenAPI client for this container's API.
func (c *TestContainer) APIClient() (*instanceoapi.ClientWithResponses, error) {
	return instanceoapi.NewClientWithResponses(c.APIBaseURL())
}

// WaitReady waits for the container's API to become ready.
// Note: With testcontainers-go, this is usually handled by the wait strategy in Start().
// This method is kept for compatibility and performs an additional health check.
func (c *TestContainer) WaitReady(ctx context.Context) error {
	url := c.APIBaseURL() + "/spec.yaml"
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	client := &http.Client{Timeout: 2 * time.Second}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			resp, err := client.Get(url)
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return nil
				}
			}
		}
	}
}

// ExitCh returns a channel that receives when the container exits.
// Note: testcontainers-go handles this internally; this is kept for API compatibility.
func (c *TestContainer) ExitCh() <-chan error {
	ch := make(chan error, 1)
	// testcontainers-go doesn't expose an exit channel directly
	// Return a channel that never fires - container lifecycle is managed by testcontainers
	return ch
}

// WaitDevTools waits for the CDP WebSocket endpoint to be ready.
func (c *TestContainer) WaitDevTools(ctx context.Context) error {
	return wait.ForListeningPort(nat.Port("9222/tcp")).
		WithStartupTimeout(2 * time.Minute).
		WaitUntilReady(ctx, c.ctr)
}

// APIClientNoKeepAlive creates an API client that doesn't reuse connections.
// This is useful after server restarts where existing connections may be stale.
func (c *TestContainer) APIClientNoKeepAlive() (*instanceoapi.ClientWithResponses, error) {
	transport := &http.Transport{
		DisableKeepAlives: true,
	}
	httpClient := &http.Client{Transport: transport}
	return instanceoapi.NewClientWithResponses(c.APIBaseURL(), instanceoapi.WithHTTPClient(httpClient))
}

// CDPAddr returns the TCP address for the container's DevTools proxy.
func (c *TestContainer) CDPAddr() string {
	return fmt.Sprintf("127.0.0.1:%d", c.CDPPort)
}

// Exec executes a command inside the container and returns the combined output.
func (c *TestContainer) Exec(ctx context.Context, cmd []string) (int, string, error) {
	exitCode, reader, err := c.ctr.Exec(ctx, cmd)
	if err != nil {
		return exitCode, "", err
	}

	// Read all output
	buf := make([]byte, 0)
	tmp := make([]byte, 1024)
	for {
		n, err := reader.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			break
		}
	}

	return exitCode, string(buf), nil
}

// Container returns the underlying testcontainers.Container for advanced usage.
func (c *TestContainer) Container() testcontainers.Container {
	return c.ctr
}
