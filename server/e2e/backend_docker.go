package e2e

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/go-connections/nat"
	instanceoapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// dockerBackend runs the image as a local Docker container via testcontainers-go.
//
// This is the historical e2e behavior, preserved verbatim and moved behind the
// Backend interface. It enables parallel test execution by giving each test its
// own dynamically allocated host ports.
type dockerBackend struct {
	Name             string
	Image            string
	APIPort          int // dynamically allocated host port -> container 10001
	CDPPort          int // dynamically allocated host port -> container 9222
	ChromeDriverPort int // dynamically allocated host port -> container 9224
	ctr              testcontainers.Container
}

// newDockerBackend returns a Docker-backed Backend for the given image.
func newDockerBackend(image string) Backend {
	return &dockerBackend{Image: image}
}

// Start starts the container with the given configuration using testcontainers-go.
func (c *dockerBackend) Start(ctx context.Context, cfg ContainerConfig) error {
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
		testcontainers.WithExposedPorts("10001/tcp", "9222/tcp", "9224/tcp"),
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

	chromeDriverPort, err := ctr.MappedPort(ctx, "9224/tcp")
	if err != nil {
		return fmt.Errorf("failed to get ChromeDriver port: %w", err)
	}
	c.ChromeDriverPort = chromeDriverPort.Int()

	return nil
}

// Stop stops and removes the container.
func (c *dockerBackend) Stop(ctx context.Context) error {
	if c.ctr == nil {
		return nil
	}
	return testcontainers.TerminateContainer(c.ctr)
}

// APIBaseURL returns the URL for the container's API server.
func (c *dockerBackend) APIBaseURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d", c.APIPort)
}

// CDPURL returns the WebSocket URL for the container's DevTools proxy.
func (c *dockerBackend) CDPURL() string {
	return fmt.Sprintf("ws://127.0.0.1:%d/", c.CDPPort)
}

// CDPAddr returns the TCP address for the container's DevTools proxy.
func (c *dockerBackend) CDPAddr() string {
	return fmt.Sprintf("127.0.0.1:%d", c.CDPPort)
}

// ChromeDriverURL returns the base HTTP URL for the container's ChromeDriver proxy.
func (c *dockerBackend) ChromeDriverURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d", c.ChromeDriverPort)
}

// APIClient creates an OpenAPI client for this container's API.
func (c *dockerBackend) APIClient() (*instanceoapi.ClientWithResponses, error) {
	return instanceoapi.NewClientWithResponses(c.APIBaseURL())
}

// APIClientNoKeepAlive creates an API client that doesn't reuse connections.
func (c *dockerBackend) APIClientNoKeepAlive() (*instanceoapi.ClientWithResponses, error) {
	transport := &http.Transport{
		DisableKeepAlives: true,
	}
	httpClient := &http.Client{Transport: transport}
	return instanceoapi.NewClientWithResponses(c.APIBaseURL(), instanceoapi.WithHTTPClient(httpClient))
}

// WaitReady waits for the container's API to become ready.
// Note: With testcontainers-go, this is usually handled by the wait strategy in
// Start(). This method performs an additional health check.
func (c *dockerBackend) WaitReady(ctx context.Context) error {
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

// WaitDevTools waits for the CDP WebSocket endpoint to be ready.
func (c *dockerBackend) WaitDevTools(ctx context.Context) error {
	return wait.ForListeningPort(nat.Port("9222/tcp")).
		WithStartupTimeout(2*time.Minute).
		WaitUntilReady(ctx, c.ctr)
}

// WaitChromeDriver waits for the ChromeDriver proxy (and upstream ChromeDriver)
// to be ready by polling the /status endpoint.
func (c *dockerBackend) WaitChromeDriver(ctx context.Context) error {
	statusURL := c.ChromeDriverURL() + "/status"
	client := &http.Client{Timeout: 2 * time.Second}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			resp, err := client.Get(statusURL)
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return nil
				}
			}
		}
	}
}

// Exec executes a command inside the container and returns the combined output.
func (c *dockerBackend) Exec(ctx context.Context, cmd []string) (int, string, error) {
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

// ExitCh returns a channel that receives when the container exits.
// Note: testcontainers-go handles this internally; this is kept for API
// compatibility and returns a channel that never fires.
func (c *dockerBackend) ExitCh() <-chan error {
	ch := make(chan error, 1)
	return ch
}
