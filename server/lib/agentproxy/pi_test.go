package agentproxy

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPiConfigurationValidation(t *testing.T) {
	t.Setenv("PI_TEST_KEY", "not-a-real-key")
	p := PiOptions{Credentials: map[string]string{"provider": "PI_TEST_KEY"}}
	valid := PiConfiguration{Launch: PiLaunch{Provider: "openrouter", Model: "z-ai/glm-5.3", Thinking: "low", Credential: "provider"}}
	if err := p.validateDesired(valid); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"npm:extension", "npm:extension@latest", "npm:extension@^1.2.3", "/tmp/extension", "npm:extension@1.0.0 --evil"} {
		c := valid
		c.Shared.Extensions = []string{source}
		if err := p.validateDesired(c); err == nil {
			t.Fatalf("accepted %s", source)
		}
	}
	valid.Shared.Extensions = []string{"npm:@example/extension@1.2.3"}
	if err := p.validateDesired(valid); err != nil {
		t.Fatal(err)
	}
	valid.Launch.Credential = "unknown"
	if err := p.validateDesired(valid); err == nil {
		t.Fatal("unknown credential accepted")
	}
}

func extensionArchive(t *testing.T, broken bool) []byte {
	t.Helper()
	var output bytes.Buffer
	gz := gzip.NewWriter(&output)
	archive := tar.NewWriter(gz)
	source := `export default function(pi) { pi.registerCommand("fixture-command", { description: "fixture", handler: async () => {} }); }`
	manifest := `{"name":"test-pi-extension","version":"1.0.0","type":"module","pi":{"extensions":["index.js"]}}`
	if broken {
		source = `export default function(pi) { throw new Error("fixture extension is broken"); }`
		manifest = strings.Replace(manifest, "1.0.0", "1.0.1", 1)
	}
	for name, data := range map[string]string{"package/package.json": manifest, "package/index.js": source} {
		if err := archive.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(archive, data); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestPiPreparesPackagesAndRetainsLastReady(t *testing.T) {
	runtime := os.Getenv("AGENT_PI_TEST_RUNTIME")
	if runtime == "" {
		t.Skip("set AGENT_PI_TEST_RUNTIME to the installed pinned pi runtime")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal(err)
	}
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Fatal(err)
	}
	// Preparation must use the configured executables, not PATH lookup.
	t.Setenv("PATH", "")
	good, bad := extensionArchive(t, false), extensionArchive(t, true)
	var registry *httptest.Server
	registry = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/test-pi-extension":
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "test-pi-extension", "versions": map[string]any{
				"1.0.0": map[string]any{"name": "test-pi-extension", "version": "1.0.0", "dist": map[string]string{"tarball": registry.URL + "/good.tgz"}},
				"1.0.1": map[string]any{"name": "test-pi-extension", "version": "1.0.1", "dist": map[string]string{"tarball": registry.URL + "/bad.tgz"}},
			}})
		case "/good.tgz":
			_, _ = w.Write(good)
		case "/bad.tgz":
			_, _ = w.Write(bad)
		default:
			http.NotFound(w, r)
		}
	}))
	defer registry.Close()
	t.Setenv("PI_TEST_KEY", "not-a-real-key")
	p := PiOptions{StateDir: t.TempDir(), RuntimeDir: runtime, Node: node, Bun: bun, Registry: registry.URL, Credentials: map[string]string{"provider": "PI_TEST_KEY"}}
	m, err := newConfigurationManager(p.StateDir, p)
	if err != nil {
		t.Fatal(err)
	}
	c := PiConfiguration{Launch: PiLaunch{Provider: "openrouter", Model: "z-ai/glm-5.3", Thinking: "low", Credential: "provider"}, Shared: PiShared{Extensions: []string{"npm:test-pi-extension@1.0.0"}, MCPServers: make([]ManagedMCPServer, 0)}}
	data, _ := json.Marshal(c)
	if err = m.apply(context.Background(), "0", data); err != nil {
		t.Fatal(err)
	}
	ready := m.snapshot()
	contents, err := os.ReadFile(filepath.Join(p.StateDir, "current", "node_modules", "test-pi-extension", "index.js"))
	if err != nil || !strings.Contains(string(contents), "fixture-command") {
		t.Fatal("extension package was not materialized", err)
	}
	c.Shared.Extensions = []string{"npm:test-pi-extension@1.0.1"}
	data, _ = json.Marshal(c)
	if err = m.apply(context.Background(), ready.Revision, data); err == nil {
		t.Fatal("broken extension activated")
	}
	if state := m.snapshot(); state.Status != "failed" || state.EffectiveRevision != ready.Revision {
		t.Fatal(state)
	}
}
