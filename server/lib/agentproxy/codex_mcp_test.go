package agentproxy

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// Only initialize/authenticate/new: native MCP discovery, no model prompt or paid provider call.
func TestCodexNativeHTTPMCP(t *testing.T) {
	runtime, remote := os.Getenv("AGENT_CODEX_TEST_RUNTIME"), os.Getenv("AGENT_PROXY_TEST_ACPREMOTE")
	if runtime == "" || remote == "" {
		t.Skip("set AGENT_CODEX_TEST_RUNTIME and AGENT_PROXY_TEST_ACPREMOTE to installed pinned runtimes")
	}
	var initialized, listed atomic.Bool
	mcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer mcp-fixture" {
			http.Error(w, "missing bound header", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				ProtocolVersion string `json:"protocolVersion"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if len(request.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch request.Method {
		case "initialize":
			initialized.Store(true)
			result = map[string]any{"protocolVersion": request.Params.ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "fixture", "version": "1"}}
		case "tools/list":
			listed.Store(true)
			result = map[string]any{"tools": []any{}}
		default:
			result = map[string]any{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
	}))
	defer mcp.Close()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_TEST_KEY", "provider-fixture")
	t.Setenv("CODEX_TEST_MCP_KEY", "mcp-fixture")
	p := &CodexOptions{StateDir: t.TempDir(), RuntimeDir: runtime, Node: node, Credentials: map[string]string{"provider": "CODEX_TEST_KEY", "mcp": "CODEX_TEST_MCP_KEY"}}
	ctx, cancel := context.WithCancel(context.Background())
	h, err := New(ctx, Config{ACPRemote: remote, MaxConnections: 1, Codex: p}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	defer func() { cancel(); server.Close(); eventually(t, func() bool { return len(h.slots) == 0 }) }()
	c := codexDesired()
	c.Shared.MCPServers = []ManagedMCPServer{{Name: "docs", URL: mcp.URL, Transport: "http", HeaderBindings: map[string]CredentialHeader{"Authorization": {Credential: "mcp", Prefix: "Bearer "}}}}
	data, _ := json.Marshal(c)
	if err := h.codex.apply(ctx, "0", data); err != nil {
		t.Fatal(err)
	}
	conn := connect(t, "ws"+strings.TrimPrefix(server.URL, "http")+"/agent/v1/acp?harness=codex")
	defer conn.CloseNow()
	request := func(id int, method string, params any) {
		send(t, conn, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		for {
			message := read(t, conn)
			if string(message.ID) == strconv.Itoa(id) {
				return
			}
		}
	}
	request(1, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}})
	request(2, "authenticate", map[string]string{"methodId": "api-key"})
	workspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, ".codex"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".codex", "config.toml"), []byte("[features]\nshell_snapshot = true\nshell_snapshot_v2 = true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	request(3, "session/new", map[string]any{"cwd": workspace, "mcpServers": []any{}})
	eventually(t, func() bool { return initialized.Load() && listed.Load() })
	conn.CloseNow()
	eventually(t, func() bool { return len(h.slots) == 0 })
	if err := filepath.WalkDir(p.StateDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(data), "provider-fixture") || strings.Contains(string(data), "mcp-fixture") {
			t.Errorf("native state persisted a credential in %s", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
