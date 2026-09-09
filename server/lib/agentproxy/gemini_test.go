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

func geminiTestOptions(t *testing.T) GeminiOptions {
	t.Helper()
	t.Setenv("GEMINI_TEST_PROVIDER", "private-provider-value")
	t.Setenv("GEMINI_TEST_MCP", "private-mcp-value")
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node required for Gemini runtime preparation")
	}
	runtime := t.TempDir()
	path := filepath.Join(runtime, "node_modules/@google/gemini-cli/bundle")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	// Preparation must not inherit either provider or MCP credentials.
	source := `if (Object.values(process.env).some(v => v.includes("private-"))) process.exit(1); console.log("0.58.0");`
	if err := os.WriteFile(filepath.Join(path, "gemini.js"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	return GeminiOptions{StateDir: t.TempDir(), RuntimeDir: runtime, Node: node, Credentials: map[string]string{"google": "GEMINI_TEST_PROVIDER", "docs": "GEMINI_TEST_MCP"}}
}
func geminiTestConfiguration() GeminiConfiguration {
	return GeminiConfiguration{Launch: GeminiLaunch{Model: "gemini-2.5-flash", Credential: "google", TrustWorkspace: true}, Shared: GeminiShared{Settings: GeminiSettings{MaxSessionTurns: 8}, MCPServers: []ManagedMCPServer{
		{Name: "docs", Command: "/bin/true", EnvBindings: map[string]string{"DOCS_TOKEN": "docs"}},
		{Name: "remote", URL: "https://example.com/mcp", Transport: "http", HeaderBindings: map[string]CredentialHeader{"Authorization": {Credential: "docs", Prefix: "Bearer "}}},
	}}}
}

func TestGeminiValidation(t *testing.T) {
	p := geminiTestOptions(t)
	cases := map[string]func(*GeminiConfiguration){
		"missing model":       func(c *GeminiConfiguration) { c.Launch.Model = "" },
		"model flag":          func(c *GeminiConfiguration) { c.Launch.Model = "--yolo" },
		"model interpolation": func(c *GeminiConfiguration) { c.Launch.Model = "$HOME" },
		"unknown credential":  func(c *GeminiConfiguration) { c.Launch.Credential = "absent" },
		"unbounded turns":     func(c *GeminiConfiguration) { c.Shared.Settings.MaxSessionTurns = -1 },
		"too many turns":      func(c *GeminiConfiguration) { c.Shared.Settings.MaxSessionTurns = 101 },
		"untrusted stdio":     func(c *GeminiConfiguration) { c.Launch.TrustWorkspace = false },
		"untrusted HTTP": func(c *GeminiConfiguration) {
			c.Launch.TrustWorkspace = false
			c.Shared.MCPServers = c.Shared.MCPServers[1:]
		},
		"unknown MCP credential": func(c *GeminiConfiguration) { c.Shared.MCPServers[0].EnvBindings["DOCS_TOKEN"] = "absent" },
		"duplicate MCP":          func(c *GeminiConfiguration) { c.Shared.MCPServers[1].Name = "docs" },
		"prototype MCP":          func(c *GeminiConfiguration) { c.Shared.MCPServers[0].Name = "__proto__" },
		"inline credential":      func(c *GeminiConfiguration) { c.Shared.MCPServers[1].URL = "https://user:pass@example.com/mcp" },
		"query credential":       func(c *GeminiConfiguration) { c.Shared.MCPServers[1].URL = "https://example.com/mcp?key=secret" },
		"interpolated arg":       func(c *GeminiConfiguration) { c.Shared.MCPServers[0].Args = []string{"${GEMINI_API_KEY}"} },
		"interpolated header": func(c *GeminiConfiguration) {
			c.Shared.MCPServers[1].HeaderBindings["Authorization"] = CredentialHeader{Credential: "docs", Prefix: "$HOME"}
		},
	}
	if err := p.validateDesired(geminiTestConfiguration()); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := geminiTestConfiguration()
			mutate(&c)
			if p.validateDesired(c) == nil {
				t.Fatal("accepted invalid configuration")
			}
		})
	}
	t.Setenv("GEMINI_TEST_PROVIDER", "")
	if p.validateDesired(geminiTestConfiguration()) == nil {
		t.Fatal("accepted empty binding")
	}
}

func TestGeminiPreparationAndRecovery(t *testing.T) {
	p := geminiTestOptions(t)
	m, err := newConfigurationManager(p.StateDir, p)
	if err != nil {
		t.Fatal(err)
	}
	desired, _ := json.Marshal(geminiTestConfiguration())
	if err = m.apply(context.Background(), "0", desired); err != nil {
		t.Fatal(err)
	}
	ready := m.snapshot()
	launch, _ := m.preparedLaunch()
	settings, err := os.ReadFile(filepath.Join(p.StateDir, "current/settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(settings), "${KERNEL_GEMINI_SECRET_0}") || !strings.Contains(string(settings), `"selectedType":"gemini-api-key"`) {
		t.Fatal(string(settings))
	}
	for _, source := range []string{"GEMINI_TEST_PROVIDER", "GEMINI_TEST_MCP"} {
		if strings.Contains(string(settings), source) {
			t.Fatal("source names leaked to settings")
		}
	}
	// Native mutable state must not be under the replaceable revision.
	marker := filepath.Join(launch.Env["HOME"], "native-marker")
	if err = os.WriteFile(marker, []byte("retain"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = m.apply(context.Background(), ready.Revision, desired); err != nil {
		t.Fatal(err)
	}
	latest := m.snapshot()
	if _, err = os.Stat(marker); err != nil {
		t.Fatal("native state was replaced", err)
	}
	if _, err = os.Stat(launch.Env["GEMINI_CLI_SYSTEM_SETTINGS_PATH"]); err != nil {
		t.Fatal("old launch revision removed", err)
	}
	broken := p
	broken.Node = "/missing/node"
	m.preparer = broken
	if err = m.apply(context.Background(), latest.Revision, desired); err == nil {
		t.Fatal("broken preparation activated")
	}
	failed := m.snapshot()
	if failed.Status != "failed" || failed.EffectiveRevision != latest.Revision || failed.Revision == latest.Revision {
		t.Fatal(failed)
	}
	if _, err = os.Stat(filepath.Join(p.StateDir, "revisions", failed.Revision)); !os.IsNotExist(err) {
		t.Fatal("failed directory retained", err)
	}
	recovered, err := newConfigurationManager(p.StateDir, p)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.snapshot().EffectiveRevision != latest.Revision || recovered.snapshot().Status != "failed" {
		t.Fatal(recovered.snapshot())
	}
	if err = recovered.apply(context.Background(), ready.Revision, desired); !errors.Is(err, errConfigurationConflict) {
		t.Fatal("stale revision accepted", err)
	}
	// All persisted files and public responses are free of credential values.
	err = filepath.Walk(p.StateDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(data), "private-provider-value") || strings.Contains(string(data), "private-mcp-value") {
			t.Errorf("credential in %s", path)
		}
		if info.Mode().Perm()&0077 != 0 {
			t.Errorf("nonprivate file %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestGeminiConfigurationHTTP(t *testing.T) {
	p := geminiTestOptions(t)
	h, err := New(context.Background(), Config{ACPRemote: "/bin/acpremote", MaxConnections: 2, Gemini: &p}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path, body, etag string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if etag != "" {
			r.Header.Set("If-Match", etag)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	path := "/agent/v1/harnesses/gemini/config"
	if w := request("GET", "/agent/v1/harnesses", "", ""); !strings.Contains(w.Body.String(), `["gemini"]`) {
		t.Fatal(w.Body.String())
	}
	if w := request("GET", "/agent/v1/acp?harness=gemini", "", ""); w.Code != 409 {
		t.Fatal(w.Code)
	}
	body := `{"launch":{"model":"gemini-2.5-flash","credential":"google"},"shared":{}}`
	if w := request("PUT", path, body, ""); w.Code != 428 {
		t.Fatal(w.Code)
	}
	for _, bad := range []string{`{"launch":{"provider":"google"}}`, `{"shared":{"extensions":[]}}`, body + body} {
		if w := request("PUT", path, bad, `"0"`); w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	w := request("PUT", path, body, `"0"`)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"mcpServers":[]`) || !strings.Contains(w.Body.String(), `"maxSessionTurns":20`) {
		t.Fatal(w.Body.String())
	}
	if strings.Contains(w.Body.String(), "private-") || strings.Contains(w.Body.String(), "GEMINI_TEST_PROVIDER") {
		t.Fatal("public credential leak")
	}
	if stale := request("PUT", path, body, `"0"`); stale.Code != 409 {
		t.Fatal(stale.Code)
	}
	if w := request("DELETE", path, "", ""); w.Code != 405 {
		t.Fatal(w.Code)
	}
	// GET stays available while one PUT prepares; a competing write returns 409.
	started, release := make(chan struct{}), make(chan struct{})
	h.gemini.preparer = prepareFunc(func(ctx context.Context, dir string, desired json.RawMessage) (Harness, error) {
		close(started)
		<-release
		return Harness{}, errors.New("private-provider-value")
	})
	done := make(chan *httptest.ResponseRecorder, 1)
	etag := w.Header().Get("ETag")
	go func() { done <- request("PUT", path, body, etag) }()
	<-started
	state := request("GET", path, "", "")
	if !strings.Contains(state.Body.String(), `"status":"preparing"`) {
		t.Fatal(state.Body.String())
	}
	conflict := request("PUT", path, body, state.Header().Get("ETag"))
	close(release)
	if conflict.Code != http.StatusConflict {
		t.Fatal(conflict.Code)
	}
	if result := <-done; result.Code != 422 || strings.Contains(result.Body.String(), "private-") {
		t.Fatal(result.Code, result.Body.String())
	}
	if result := request("GET", path, "", ""); !strings.Contains(result.Body.String(), `"status":"failed"`) || strings.Contains(result.Body.String(), "private-") {
		t.Fatal(result.Body.String())
	}
}

func TestGeminiPinnedRuntimePreparation(t *testing.T) {
	runtime := os.Getenv("AGENT_GEMINI_TEST_RUNTIME")
	if runtime == "" {
		t.Skip("set AGENT_GEMINI_TEST_RUNTIME to the installed pinned Gemini runtime")
	}
	p := geminiTestOptions(t)
	p.RuntimeDir = runtime
	m, err := newConfigurationManager(p.StateDir, p)
	if err != nil {
		t.Fatal(err)
	}
	desired, _ := json.Marshal(geminiTestConfiguration())
	if err = m.apply(context.Background(), "0", desired); err != nil {
		t.Fatal(err)
	}
}
