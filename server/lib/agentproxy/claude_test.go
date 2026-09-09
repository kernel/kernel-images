package agentproxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestClaudeConfigurationHTTP(t *testing.T) {
	t.Setenv("CLAUDE_TEST_KEY", "private-provider-key")
	p := &ClaudeOptions{StateDir: t.TempDir(), RuntimeDir: "/runtime/claude", Node: "/bin/node", Credentials: map[string]string{"provider": "CLAUDE_TEST_KEY"}}
	h, err := New(context.Background(), Config{ACPRemote: "/bin/acpremote", MaxConnections: 1, Claude: p}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	h.claude.preparer = prepareFunc(func(_ context.Context, dir string, _ json.RawMessage) (Harness, error) {
		return Harness{Command: "/bin/true", Cwd: dir}, nil
	})
	request := func(method, path, body, etag string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if etag != "" {
			r.Header.Set("If-Match", etag)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	path := "/agent/v1/harnesses/claude/config"
	if w := request("GET", "/agent/v1/harnesses", "", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"claude"`) {
		t.Fatal(w)
	}
	if w := request("GET", "/agent/v1/acp?harness=claude", "", ""); w.Code != 409 {
		t.Fatal(w)
	}
	if w := request("GET", path, "", ""); w.Header().Get("ETag") != `"0"` {
		t.Fatal(w)
	}
	body := `{"launch":{"model":"claude-haiku-4-5-20251001","credential":"provider"},"shared":{}}`
	for _, tc := range []struct {
		body, etag string
		code       int
	}{
		{body, "", 428}, {strings.Replace(body, `"shared":{}`, `"shared":{"extensions":[]}`, 1), `"0"`, 400},
		{strings.Replace(body, "provider", "missing", 1), `"0"`, 422}, {body + " {}", `"0"`, 400},
	} {
		if w := request("PUT", path, tc.body, tc.etag); w.Code != tc.code {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	ready := request("PUT", path, body, `"0"`)
	if ready.Code != 200 || !strings.Contains(ready.Body.String(), `"mcpServers":[]`) {
		t.Fatal(ready)
	}
	if w := request("PUT", path, body, `"0"`); w.Code != 409 {
		t.Fatal(w)
	}
	h.claude.preparer = prepareFunc(func(context.Context, string, json.RawMessage) (Harness, error) {
		return Harness{}, errors.New("private-provider-key")
	})
	if w := request("PUT", path, body, ready.Header().Get("ETag")); w.Code != 422 || strings.Contains(w.Body.String(), "private-provider-key") {
		t.Fatal(w)
	}
	state := request("GET", path, "", "")
	if !strings.Contains(state.Body.String(), `"status":"failed"`) || strings.Contains(state.Body.String(), "private-provider-key") || strings.Contains(state.Body.String(), "CLAUDE_TEST_KEY") {
		t.Fatal(state)
	}
	if _, ok := h.claude.preparedLaunch(); !ok {
		t.Fatal("failed preparation removed last ready launch")
	}
	if w := request(http.MethodDelete, path, "", ""); w.Code != 405 {
		t.Fatal(w)
	}
}

func TestClaudePreparesNativeSettings(t *testing.T) {
	runtime := os.Getenv("AGENT_CLAUDE_TEST_RUNTIME")
	if runtime == "" {
		t.Skip("set AGENT_CLAUDE_TEST_RUNTIME to installed Claude runtime")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_TEST_KEY", "not-a-real-key")
	p := ClaudeOptions{StateDir: t.TempDir(), RuntimeDir: runtime, Node: node, Credentials: map[string]string{"provider": "CLAUDE_TEST_KEY"}}
	m, err := newConfigurationManager(p.StateDir, p)
	if err != nil {
		t.Fatal(err)
	}
	data := json.RawMessage(`{"launch":{"model":"claude-haiku-4-5-20251001","credential":"provider"},"shared":{"settings":{"language":"English","alwaysThinkingEnabled":false},"mcpServers":[]}}`)
	if err = m.apply(context.Background(), "0", data); err != nil {
		t.Fatal(err)
	}
	ready := m.snapshot()
	launch, _ := m.preparedLaunch()
	if launch.Env["CLAUDE_CONFIG_DIR"] != filepath.Join(p.StateDir, "native") {
		t.Fatal(launch)
	}
	if _, err = os.Stat(filepath.Join(p.StateDir, "native")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("preparation wrote native state")
	}
	for _, name := range []string{"settings.json", "config.json", "prepared.json"} {
		path := filepath.Join(p.StateDir, "current", name)
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal(name, err)
		}
		contents, _ := os.ReadFile(path)
		if strings.Contains(string(contents), "not-a-real-key") {
			t.Fatal("persisted secret")
		}
	}
	p.Node = "/bin/false"
	m.preparer = p
	if err = m.apply(context.Background(), ready.Revision, data); err == nil {
		t.Fatal("broken runtime accepted")
	}
	recovered, err := newConfigurationManager(p.StateDir, p)
	if err != nil {
		t.Fatal(err)
	}
	if state := recovered.snapshot(); state.Status != "failed" || state.EffectiveRevision != ready.Revision || state.Revision == ready.Revision {
		t.Fatal(state)
	}
	entries, _ := os.ReadDir(filepath.Join(p.StateDir, "revisions"))
	if len(entries) != 1 {
		t.Fatal("failed preparation directory retained")
	}
}

func TestClaudeNativeInitializeAndList(t *testing.T) {
	runtime, remote := os.Getenv("AGENT_CLAUDE_TEST_RUNTIME"), os.Getenv("AGENT_PROXY_TEST_ACPREMOTE")
	if runtime == "" || remote == "" {
		t.Skip("set AGENT_CLAUDE_TEST_RUNTIME and AGENT_PROXY_TEST_ACPREMOTE for native ACP verification")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_TEST_KEY", "fixture-not-a-provider-key")
	p := &ClaudeOptions{StateDir: t.TempDir(), RuntimeDir: runtime, Node: node, Credentials: map[string]string{"provider": "CLAUDE_TEST_KEY"}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h, err := New(ctx, Config{ACPRemote: remote, MaxConnections: 2, Claude: p}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	data := json.RawMessage(`{"launch":{"model":"claude-haiku-4-5-20251001","credential":"provider"},"shared":{"settings":{},"mcpServers":[]}}`)
	if err := h.claude.apply(ctx, "0", data); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	t.Cleanup(func() { cancel(); server.Close() })
	for range 2 {
		conn := connect(t, "ws"+strings.TrimPrefix(server.URL, "http")+"/agent/v1/acp?harness=claude")
		response, _ := call(t, conn, 1, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}})
		var initialized struct {
			AgentInfo struct {
				Version string `json:"version"`
			} `json:"agentInfo"`
		}
		if err := json.Unmarshal(response, &initialized); err != nil || initialized.AgentInfo.Version != "0.75.1" {
			t.Fatalf("unexpected initialize: %s", response)
		}
		response, _ = call(t, conn, 2, "session/list", map[string]any{})
		if !strings.Contains(string(response), `"sessions":[]`) {
			t.Fatalf("unexpected session list: %s", response)
		}
		conn.CloseNow()
		eventually(t, func() bool { return len(h.slots) == 0 })
	}
}

func TestClaudeRejectsUnsupportedConfiguration(t *testing.T) {
	t.Setenv("CLAUDE_TEST_KEY", "fixture")
	p := ClaudeOptions{Credentials: map[string]string{"provider": "CLAUDE_TEST_KEY"}}
	c := ClaudeConfiguration{Launch: ClaudeLaunch{Model: "claude-haiku-4-5-20251001", Credential: "provider"}}
	if err := p.validateDesired(c); err != nil {
		t.Fatal(err)
	}
	c.Shared.MCPServers = []ManagedMCPServer{{Name: "http", URL: "https://example.com/mcp", Transport: "http"}}
	if err := p.validateDesired(c); err == nil {
		t.Fatal("unvalidated HTTP MCP accepted")
	}
	c.Shared.MCPServers = nil
	c.Launch.Model = "bedrock-model"
	if err := p.validateDesired(c); err == nil {
		t.Fatal("unsupported provider accepted")
	}
}
