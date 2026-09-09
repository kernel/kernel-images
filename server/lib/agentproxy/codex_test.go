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
	"strings"
	"testing"
)

func codexDesired() CodexConfiguration {
	return CodexConfiguration{
		Launch: CodexLaunch{Provider: "openai", Model: "gpt-5.4-mini", Thinking: "low", Mode: "read-only", Credential: "provider"},
		Shared: CodexShared{WebSearch: "disabled", MCPServers: make([]ManagedMCPServer, 0)},
	}
}

func TestCodexValidation(t *testing.T) {
	t.Setenv("CODEX_TEST_KEY", "private-key")
	p := CodexOptions{Credentials: map[string]string{"provider": "CODEX_TEST_KEY"}}
	if err := p.validateDesired(codexDesired()); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*CodexConfiguration){
		"provider":     func(c *CodexConfiguration) { c.Launch.Provider = "bedrock" },
		"model":        func(c *CodexConfiguration) { c.Launch.Model = "\nsecret" },
		"thinking":     func(c *CodexConfiguration) { c.Launch.Thinking = "off" },
		"mode":         func(c *CodexConfiguration) { c.Launch.Mode = "bypass" },
		"credential":   func(c *CodexConfiguration) { c.Launch.Credential = "unknown" },
		"webSearch":    func(c *CodexConfiguration) { c.Shared.WebSearch = "yes" },
		"instructions": func(c *CodexConfiguration) { c.Shared.Instructions = "\x00" },
		"sse": func(c *CodexConfiguration) {
			c.Shared.MCPServers = []ManagedMCPServer{{Name: "docs", URL: "https://example.com", Transport: "sse"}}
		},
		"name": func(c *CodexConfiguration) {
			c.Shared.MCPServers = []ManagedMCPServer{{Name: "docs.dot", Command: "/bin/true"}}
		},
		"env": func(c *CodexConfiguration) {
			c.Shared.MCPServers = []ManagedMCPServer{{Name: "docs", Command: "/bin/true", EnvBindings: map[string]string{"bad-name": "provider"}}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := codexDesired()
			change(&c)
			if err := p.validateDesired(c); err == nil {
				t.Fatal("accepted invalid configuration")
			}
		})
	}
}

func TestCodexHTTPConcurrencyAndFailure(t *testing.T) {
	t.Setenv("CODEX_TEST_KEY", "private-key")
	p := &CodexOptions{StateDir: t.TempDir(), RuntimeDir: "/runtime", Node: "/bin/node", Credentials: map[string]string{"provider": "CODEX_TEST_KEY"}}
	h, err := New(context.Background(), Config{ACPRemote: "/bin/acpremote", MaxConnections: 1, Codex: p, Harnesses: map[string]Harness{"codex": {Command: "/bin/true", Cwd: "/tmp"}}}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	h.codex.preparer = prepareFunc(func(ctx context.Context, dir string, _ json.RawMessage) (Harness, error) {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return Harness{}, ctx.Err()
		}
		return Harness{Command: "/bin/true", Cwd: dir}, nil
	})
	path := "/agent/v1/harnesses/codex/config"
	request := func(method, url, body, etag string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, url, strings.NewReader(body))
		r.Header.Set("If-Match", etag)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := request("GET", "/agent/v1/harnesses", "", ""); w.Body.String() != "{\"configured\":[\"codex\"]}\n" {
		t.Fatal(w.Body.String())
	}
	if w := request("GET", "/agent/v1/acp?harness=codex", "", ""); w.Code != 409 {
		t.Fatal(w.Code)
	}
	initial := request("GET", path, "", "")
	if initial.Header().Get("ETag") != `"0"` {
		t.Fatal(initial.Header())
	}
	data, _ := json.Marshal(codexDesired())
	body := string(data)
	if w := request("PUT", path, body, ""); w.Code != 428 {
		t.Fatal(w.Code)
	}
	if w := request("PUT", path, strings.Replace(body, `"webSearch"`, `"extensions"`, 1), `"0"`); w.Code != 400 {
		t.Fatal(w.Code)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- request("PUT", path, body, `"0"`) }()
	<-started
	if w := request("GET", path, "", ""); !strings.Contains(w.Body.String(), `"status":"preparing"`) {
		t.Fatal(w.Body.String())
	}
	if w := request("PUT", path, body, `"0"`); w.Code != 409 {
		t.Fatal(w.Code)
	}
	close(release)
	ready := <-done
	if ready.Code != 200 {
		t.Fatal(ready.Body.String())
	}
	if strings.Contains(ready.Body.String(), "private-key") || strings.Contains(ready.Body.String(), "CODEX_TEST_KEY") || !strings.Contains(ready.Body.String(), `"mcpServers":[]`) {
		t.Fatal(ready.Body.String())
	}
	if w := request("PUT", path, body, `"0"`); w.Code != 409 {
		t.Fatal(w.Code)
	}
	h.codex.preparer = prepareFunc(func(context.Context, string, json.RawMessage) (Harness, error) { return Harness{}, os.ErrPermission })
	if w := request("PUT", path, body, ready.Header().Get("ETag")); w.Code != 422 {
		t.Fatal(w.Code)
	}
	failed := h.codex.snapshot()
	if failed.Status != "failed" || failed.EffectiveRevision == failed.Revision || failed.EffectiveRevision == "" {
		t.Fatal(failed)
	}
	restored, err := newConfigurationManager(p.StateDir, *p)
	if err != nil || restored.snapshot().EffectiveRevision != failed.EffectiveRevision {
		t.Fatal(err)
	}
	if _, ok := restored.preparedLaunch(); !ok {
		t.Fatal("lost launch")
	}
	if w := request(http.MethodDelete, path, "", ""); w.Code != 405 {
		t.Fatal(w.Code)
	}
}

func TestCodexNativePreparation(t *testing.T) {
	runtime := os.Getenv("AGENT_CODEX_TEST_RUNTIME")
	if runtime == "" {
		t.Skip("set AGENT_CODEX_TEST_RUNTIME to the installed pinned Codex runtime")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_TEST_KEY", "private-test-credential")
	p := CodexOptions{StateDir: t.TempDir(), RuntimeDir: runtime, Node: node, Credentials: map[string]string{"provider": "CODEX_TEST_KEY"}}
	m, err := newConfigurationManager(p.StateDir, p)
	if err != nil {
		t.Fatal(err)
	}
	c := codexDesired()
	c.Shared.MCPServers = []ManagedMCPServer{{Name: "docs", Command: "/bin/true", EnvBindings: map[string]string{"TOKEN": "provider"}}, {Name: "remote", URL: "https://example.com/mcp", Transport: "http", HeaderBindings: map[string]CredentialHeader{"Authorization": {Credential: "provider", Prefix: "Bearer "}}}}
	data, _ := json.Marshal(c)
	if err = m.apply(context.Background(), "0", data); err != nil {
		t.Fatal(err)
	}
	ready := m.snapshot()
	if err = filepath.WalkDir(p.StateDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(content), "private-test-credential") {
			t.Errorf("credential persisted in %s", path)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode().Perm()&0077 != 0 {
			t.Errorf("non-private file %s", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(p.StateDir, "native")); !os.IsNotExist(err) {
		t.Fatal("preparation touched native state")
	}
	c.Shared.MCPServers[0].Command = "/nonexistent/codex-mcp-test"
	data, _ = json.Marshal(c)
	if err = m.apply(context.Background(), ready.Revision, data); err == nil {
		t.Fatal("missing MCP command activated")
	}
	failed := m.snapshot()
	if failed.Status != "failed" || failed.EffectiveRevision != ready.Revision {
		t.Fatal(failed)
	}
	if _, err := os.Stat(filepath.Join(p.StateDir, "revisions", failed.Revision)); !os.IsNotExist(err) {
		t.Fatal("failed directory retained")
	}
}
