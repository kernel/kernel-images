package agentproxy

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	valid := `{"acpremote":"/opt/acp/bin/acpremote","maxConnections":8,"harnesses":{"pi":{"command":"/opt/pi-acp","cwd":"/workspace","args":[],"env":{},"inheritEnv":[]}}}`
	for _, test := range []struct {
		name string
		data string
		ok   bool
	}{
		{"valid", valid, true},
		{"unknown field", strings.Replace(valid, `"maxConnections":8`, `"typo":8`, 1), false},
		{"extra JSON", valid + `{}`, false},
		{"oversized", valid + strings.Repeat(" ", 64<<10), false},
		{"relative executable", strings.Replace(valid, "/opt/pi-acp", "pi-acp", 1), false},
		{"relative cwd", strings.Replace(valid, "/workspace", "workspace", 1), false},
		{"unknown harness", strings.Replace(valid, `"pi":`, `"other":`, 1), false},
		{"no connection bound", strings.Replace(valid, `"maxConnections":8`, `"maxConnections":0`, 1), false},
		{"reserved environment", strings.Replace(valid, `"env":{}`, `"env":{"KERNEL_ACP_BRIDGE_TOKEN":"override"}`, 1), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(test.data), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			if (err == nil) != test.ok {
				t.Fatalf("load: %v", err)
			}
		})
	}
}

func TestEnvironmentIsExplicit(t *testing.T) {
	t.Setenv("AGENT_PROXY_TEST_SECRET", "do-not-inherit")
	t.Setenv("AGENT_PROXY_TEST_ALLOWED", "inherited")
	h := Harness{InheritEnv: []string{"AGENT_PROXY_TEST_ALLOWED"}, Env: map[string]string{"HOME": "/shared/pi"}}
	env, err := h.environment("internal-token")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "do-not-inherit") || !strings.Contains(joined, "AGENT_PROXY_TEST_ALLOWED=inherited") || !strings.Contains(joined, "HOME=/shared/pi") {
		t.Fatal("environment inheritance or override is incorrect")
	}
	t.Setenv("AGENT_PROXY_TEST_ALLOWED", "")
	if _, err := h.environment("internal-token"); err == nil {
		t.Fatal("empty required environment accepted")
	}
}

func TestBridgeAddress(t *testing.T) {
	for _, address := range []string{"ws://127.0.0.1:1234/acp/ws", "ws://example.com:1234/acp/ws", "ws://127.0.0.1:0/acp/ws", "ws://127.0.0.1:70000/acp/ws", "ws://127.0.0.1:1234/acp/ws?secret=x", "ws://user@127.0.0.1:1234/acp/ws", "ws://127.0.0.1:1234/other"} {
		got := bridgeAddress("Serving ACP WebSocket at " + address)
		if (got != "") != (address == "ws://127.0.0.1:1234/acp/ws") {
			t.Fatalf("unexpected address accepted: %q", got)
		}
	}
}

func TestHTTPAdmissionAndDiscovery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	config := Config{ACPRemote: "/nonexistent/acpremote", MaxConnections: 1, Harnesses: map[string]Harness{
		"pi": {Command: "/nonexistent/pi", Cwd: t.TempDir(), Env: map[string]string{"PRIVATE_KEY": "secret-value"}},
	}}
	h, err := New(ctx, config, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	h.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/agent/v1/harnesses", nil))
	var discovery struct {
		Configured []string `json:"configured"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &discovery); err != nil || len(discovery.Configured) != 1 || discovery.Configured[0] != "pi" {
		t.Fatal("invalid configured harness list")
	}
	if strings.Contains(response.Body.String(), "secret-value") {
		t.Fatal("discovery leaked launch environment")
	}
	for _, test := range []struct {
		path string
		code int
	}{
		{"/agent/v1/acp?harness=unknown", 404},
		{"/agent/v1/acp?harness=pi", 400},
		{"/agent/v1/commands", 404},
	} {
		response := httptest.NewRecorder()
		h.ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.path, nil))
		if response.Code != test.code {
			t.Fatalf("%s: %d", test.path, response.Code)
		}
	}
	request := httptest.NewRequest(http.MethodGet, "/agent/v1/acp?harness=pi", nil)
	request.Header.Set("Upgrade", "websocket")
	response = httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != 503 || len(h.slots) != 0 {
		t.Fatal("failed startup did not release admission slot")
	}
	cancel()
	response = httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != 503 {
		t.Fatal("shutdown accepted a new connection")
	}
}
