package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"google.golang.org/protobuf/proto"

	instanceoapi "github.com/kernel/kernel-images/server/lib/oapi"
)

// otlpMockCollector is a minimal OTLP/HTTP logs receiver served on the test host
// (0.0.0.0 so a container can reach it via host.docker.internal). It records the
// Authorization header, resource attributes, and event names of every export.
type otlpMockCollector struct {
	srv *http.Server
	ln  net.Listener

	mu           sync.Mutex
	auths        []string
	instanceName []string
	metro        []string
	serviceName  []string
	eventNames   []string
}

func startOTLPMockCollector(t *testing.T) *otlpMockCollector {
	t.Helper()
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	require.NoError(t, err, "listen for mock collector")
	c := &otlpMockCollector{ln: ln}
	mux := http.NewServeMux()
	mux.HandleFunc("/otlp-relay/v1/logs", c.handle)
	c.srv = &http.Server{Handler: mux}
	go func() { _ = c.srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = c.srv.Shutdown(ctx)
	})
	return c
}

func (c *otlpMockCollector) handle(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var req collogspb.ExportLogsServiceRequest
	if err := proto.Unmarshal(body, &req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	c.auths = append(c.auths, r.Header.Get("Authorization"))
	for _, rl := range req.ResourceLogs {
		for _, attr := range rl.Resource.GetAttributes() {
			switch attr.Key {
			case "kernel.instance_name":
				c.instanceName = append(c.instanceName, attr.Value.GetStringValue())
			case "kernel.metro":
				c.metro = append(c.metro, attr.Value.GetStringValue())
			case "service.name":
				c.serviceName = append(c.serviceName, attr.Value.GetStringValue())
			}
		}
		for _, sl := range rl.ScopeLogs {
			for _, lr := range sl.LogRecords {
				c.eventNames = append(c.eventNames, lr.EventName)
			}
		}
	}
	c.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

// hostEndpoint returns host:port the container uses to reach the collector.
func (c *otlpMockCollector) hostEndpoint() string {
	return fmt.Sprintf("host.docker.internal:%d", c.ln.Addr().(*net.TCPAddr).Port)
}

func (c *otlpMockCollector) snapshot() (auths, instanceName, eventNames []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.auths...), append([]string(nil), c.instanceName...), append([]string(nil), c.eventNames...)
}

// enableControlExport turns on the control category (so api_call events flow)
// and OTLP export, then makes a few browser-control calls to generate
// exportable events.
func enableControlExport(t *testing.T, ctx context.Context, client *instanceoapi.ClientWithResponses) {
	t.Helper()
	tr := true
	resp, err := client.PutTelemetryWithResponse(ctx, instanceoapi.PutTelemetryJSONRequestBody{
		Browser: &instanceoapi.BrowserTelemetryCategoriesConfig{
			Control: &instanceoapi.BrowserTelemetryControlConfig{Enabled: &tr},
		},
		Export: &instanceoapi.BrowserTelemetryExportConfig{
			Otlp: &instanceoapi.BrowserTelemetryOTLPExportConfig{Enabled: &tr},
		},
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode(), "put telemetry: %s", string(resp.Body))
	// Browser-control calls emit api_call (control) events, which OTLP exports.
	for i := 0; i < 3; i++ {
		_, _ = client.TakeScreenshotWithResponse(ctx, instanceoapi.TakeScreenshotJSONRequestBody{})
		time.Sleep(50 * time.Millisecond)
	}
}

// TestOTLPExport (Tier 2) runs a headless container pointed at a mock OTLP
// collector, enables export via the telemetry API, and verifies exported records
// land carrying the instance JWT as a bearer token plus the expected resource
// attributes. Skips when docker is unavailable.
func TestOTLPExport(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker not available: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	collector := startOTLPMockCollector(t)

	c := NewTestContainer(t, headlessImage)
	require.NoError(t, c.Start(ctx, ContainerConfig{
		HostAccess: true,
		Env: map[string]string{
			"BTEL_OTLP_ENDPOINT":  collector.hostEndpoint(),
			"BTEL_OTLP_PATH":      "/otlp-relay/v1/logs",
			"BTEL_OTLP_INSECURE":  "true",
			"INST_NAME":           "browser-e2e-1",
			"METRO_NAME":          "dev-iad",
			"KERNEL_INSTANCE_JWT": "e2e-jwt-token",
		},
	}), "failed to start container")
	defer c.Stop(ctx)
	require.NoError(t, c.WaitReady(ctx), "api not ready")

	client, err := c.APIClient()
	require.NoError(t, err)
	enableControlExport(t, ctx, client)

	require.Eventually(t, func() bool {
		_, _, events := collector.snapshot()
		return len(events) > 0
	}, 20*time.Second, 250*time.Millisecond, "collector received no exported records")

	auths, instanceName, eventNames := collector.snapshot()
	t.Logf("collector saw %d record(s); auths=%v instance=%v events=%v", len(eventNames), auths, instanceName, eventNames)
	require.NotEmpty(t, auths)
	assert.Equal(t, "Bearer e2e-jwt-token", auths[0], "exporter must send the instance JWT as a bearer token")
	assert.Contains(t, instanceName, "browser-e2e-1", "resource should carry the instance name")
	assert.Contains(t, eventNames, "api_call", "control-category api_call events should be exported")
}

// TestOTLPExportForkIdentityRefresh (Tier 3) starts a container in
// fork-identity-wait mode with an empty boot JWT, applies a fork identity
// carrying a fresh KERNEL_INSTANCE_JWT via the real /internal/fork-identity
// endpoint, and verifies exports pick up that fresh JWT without a restart.
func TestOTLPExportForkIdentityRefresh(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker not available: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	collector := startOTLPMockCollector(t)

	c := NewTestContainer(t, headlessImage)
	require.NoError(t, c.Start(ctx, ContainerConfig{
		HostAccess: true,
		Env: map[string]string{
			"KERNEL_FORK_IDENTITY_WAIT": "true",
			"BTEL_OTLP_ENDPOINT":        collector.hostEndpoint(),
			"BTEL_OTLP_PATH":            "/otlp-relay/v1/logs",
			"BTEL_OTLP_INSECURE":        "true",
			"KERNEL_INSTANCE_JWT":       "", // unbound fork: no boot credential
			"METRO_NAME":                "dev-iad",
		},
	}), "failed to start container")
	defer c.Stop(ctx)
	require.NoError(t, c.WaitReady(ctx), "api not ready")

	// Apply the fork identity with a fresh JWT via the real internal endpoint.
	// The wrapper clears any pre-existing payload once when it first enters its
	// wait loop, so a POST that lands before that point gets wiped. Retry with a
	// bounded per-attempt timeout until the apply sticks; 409 means an earlier
	// attempt already applied the (identical) payload.
	payload := map[string]string{
		"instance_name":       "browser-fork-e2e",
		"session_intel_url":   "https://intel.example",
		"kernel_instance_jwt": "fresh-fork-jwt",
	}
	buf, err := json.Marshal(payload)
	require.NoError(t, err)
	applied := false
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); {
		attemptCtx, acancel := context.WithTimeout(ctx, 5*time.Second)
		req, reqErr := http.NewRequestWithContext(attemptCtx, http.MethodPost, c.APIBaseURL()+"/internal/fork-identity", bytes.NewReader(buf))
		require.NoError(t, reqErr)
		req.Header.Set("Content-Type", "application/json")
		resp, doErr := http.DefaultClient.Do(req)
		if doErr != nil {
			acancel()
			continue
		}
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		acancel()
		if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusConflict {
			applied = true
			break
		}
		t.Logf("fork-identity apply not ready yet: %d %s", resp.StatusCode, string(respBody))
		time.Sleep(500 * time.Millisecond)
	}
	require.True(t, applied, "fork identity never applied")

	client, err := c.APIClient()
	require.NoError(t, err)
	enableControlExport(t, ctx, client)

	require.Eventually(t, func() bool {
		auths, _, _ := collector.snapshot()
		for _, a := range auths {
			if a == "Bearer fresh-fork-jwt" {
				return true
			}
		}
		return false
	}, 30*time.Second, 250*time.Millisecond, "collector never saw the refreshed fork JWT")

	auths, instanceName, _ := collector.snapshot()
	t.Logf("collector auths=%v instance=%v", auths, instanceName)
	for _, a := range auths {
		assert.Contains(t, []string{"", "Bearer fresh-fork-jwt"}, a, "only empty or the fresh fork JWT expected")
	}
	// The resource identity is also refreshed from the applied payload, not left
	// at the empty boot INST_NAME.
	assert.Contains(t, instanceName, "browser-fork-e2e", "resource instance_name should come from the applied fork payload")
}
