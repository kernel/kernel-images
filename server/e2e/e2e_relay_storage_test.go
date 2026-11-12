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

	instanceoapi "github.com/kernel/kernel-images/server/lib/oapi"
)

const relayStoragePath = "/btel-relay/v1/records"

// relayMock is a minimal metro-api storage relay served on the test host. It
// records the Authorization header and records of every request.
type relayMock struct {
	srv *http.Server
	ln  net.Listener

	mu      sync.Mutex
	auths   []string
	records []relayMockRecord
}

type relayMockRecord struct {
	Body json.RawMessage `json:"body"`
}

func startRelayMock(t *testing.T) *relayMock {
	t.Helper()
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	require.NoError(t, err, "listen for mock relay")
	m := &relayMock{ln: ln}
	mux := http.NewServeMux()
	mux.HandleFunc(relayStoragePath, m.handle)
	m.srv = &http.Server{Handler: mux}
	go func() { _ = m.srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = m.srv.Shutdown(ctx)
	})
	return m
}

func (m *relayMock) handle(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var req struct {
		Records []relayMockRecord `json:"records"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	m.mu.Lock()
	m.auths = append(m.auths, r.Header.Get("Authorization"))
	m.records = append(m.records, req.Records...)
	m.mu.Unlock()
	_, _ = fmt.Fprintf(w, `{"start_seq_num":0,"end_seq_num":%d}`, len(req.Records))
}

func (m *relayMock) hostEndpoint() string {
	return fmt.Sprintf("host.docker.internal:%d", m.ln.Addr().(*net.TCPAddr).Port)
}

func (m *relayMock) snapshot() ([]string, []relayMockRecord) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.auths...), append([]relayMockRecord(nil), m.records...)
}

// enableControlStorage turns on the control category with storage, then makes
// a few browser-control calls to generate stored events.
func enableControlStorage(t *testing.T, ctx context.Context, client *instanceoapi.ClientWithResponses) {
	t.Helper()
	tr := true
	resp, err := client.PutTelemetryWithResponse(ctx, instanceoapi.PutTelemetryJSONRequestBody{
		Browser: &instanceoapi.BrowserTelemetryCategoriesConfig{
			Control: &instanceoapi.BrowserTelemetryControlConfig{Enabled: &tr},
		},
		Storage: &instanceoapi.BrowserTelemetryStorageConfig{Enabled: &tr},
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode(), "put telemetry: %s", string(resp.Body))
	for range 3 {
		_, _ = client.TakeScreenshotWithResponse(ctx, instanceoapi.TakeScreenshotJSONRequestBody{})
		time.Sleep(50 * time.Millisecond)
	}
}

// requireEnvelopes checks each stored record is a {seq,event} envelope.
func requireEnvelopes(t *testing.T, records []relayMockRecord) {
	t.Helper()
	for _, rec := range records {
		var env struct {
			Seq   uint64 `json:"seq"`
			Event struct {
				Type string `json:"type"`
			} `json:"event"`
		}
		require.NoError(t, json.Unmarshal(rec.Body, &env))
		require.NotEmpty(t, env.Event.Type)
	}
}

// TestRelayStorage runs a headless container with a relay path and verifies
// stored events reach the relay with the instance JWT as a bearer token. Direct
// S2 credentials are also set, to show the relay wins. Skips when docker is
// unavailable.
func TestRelayStorage(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker not available: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	relay := startRelayMock(t)

	c := NewTestContainer(t, headlessImage)
	require.NoError(t, c.Start(ctx, ContainerConfig{
		HostAccess: true,
		Env: map[string]string{
			"BTEL_OTLP_ENDPOINT":  relay.hostEndpoint(),
			"BTEL_OTLP_INSECURE":  "true",
			"BTEL_STORAGE_PATH":   relayStoragePath,
			"S2_BASIN":            "unused-basin",
			"S2_ACCESS_TOKEN":     "unused-token",
			"S2_STREAM":           "browser-e2e-1",
			"INST_NAME":           "browser-e2e-1",
			"KERNEL_INSTANCE_JWT": "e2e-jwt-token",
		},
	}), "failed to start container")
	defer c.Stop(ctx)
	require.NoError(t, c.WaitReady(ctx), "api not ready")

	client, err := c.APIClient()
	require.NoError(t, err)
	enableControlStorage(t, ctx, client)

	require.Eventually(t, func() bool {
		_, records := relay.snapshot()
		return len(records) > 0
	}, 20*time.Second, 250*time.Millisecond, "relay received no records")

	auths, records := relay.snapshot()
	assert.Equal(t, "Bearer e2e-jwt-token", auths[0])
	requireEnvelopes(t, records)
}

// TestRelayStorageForkIdentity boots a fork waiting for its identity, with a
// dead relay endpoint and no credential, then applies an identity carrying a
// fresh JWT, stream, and metro-api URL. Storage must reach the relay at the
// handed metro-api with the fresh JWT.
func TestRelayStorageForkIdentity(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker not available: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	relay := startRelayMock(t)

	c := NewTestContainer(t, headlessImage)
	require.NoError(t, c.Start(ctx, ContainerConfig{
		HostAccess: true,
		Env: map[string]string{
			"KERNEL_FORK_IDENTITY_WAIT": "true",
			"BTEL_OTLP_ENDPOINT":        "127.0.0.1:1",
			"BTEL_OTLP_INSECURE":        "true",
			"BTEL_STORAGE_PATH":         relayStoragePath,
			"S2_STREAM":                 "seed-stream",
			"KERNEL_INSTANCE_JWT":       "",
		},
	}), "failed to start container")
	defer c.Stop(ctx)
	require.NoError(t, c.WaitReady(ctx), "api not ready")

	payload := map[string]string{
		"instance_name":             "browser-fork-e2e",
		"kernel_instance_jwt":       "fresh-fork-jwt",
		"s2_stream":                 "browser-fork-e2e",
		"kernel_metro_api_base_url": "http://" + relay.hostEndpoint(),
	}
	buf, err := json.Marshal(payload)
	require.NoError(t, err)
	// See TestOTLPExportForkIdentityRefresh: retry until the apply sticks.
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
	enableControlStorage(t, ctx, client)

	require.Eventually(t, func() bool {
		_, records := relay.snapshot()
		return len(records) > 0
	}, 30*time.Second, 250*time.Millisecond, "relay at the handed metro-api received no records")

	auths, records := relay.snapshot()
	for _, a := range auths {
		assert.Equal(t, "Bearer fresh-fork-jwt", a)
	}
	requireEnvelopes(t, records)
}
