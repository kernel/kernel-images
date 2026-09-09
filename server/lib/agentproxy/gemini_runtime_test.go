package agentproxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestGeminiLaunchCredentialIsolation(t *testing.T) {
	p := geminiTestOptions(t)
	source, err := os.ReadFile("../../runtime/acp/gemini/launch.mjs")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(p.RuntimeDir, "launch.mjs"), source, 0600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	desired, _ := json.Marshal(geminiTestConfiguration())
	launch, err := p.Prepare(context.Background(), dir, desired)
	if err != nil {
		t.Fatal(err)
	}
	// Verify values inside the fixture without emitting them on stdout/stderr.
	fixture := `const e=process.env;
const ok=e.GEMINI_API_KEY==="private-provider-value" && e.KERNEL_GEMINI_SECRET_0==="private-mcp-value"
 && !e.GEMINI_TEST_PROVIDER && !e.GEMINI_TEST_MCP && !e.KERNEL_GEMINI_BINDINGS
 && !e.KERNEL_GEMINI_PROVIDER_SOURCE && !Object.values(e).includes("bridge-private-token");
console.log(JSON.stringify({ok,args:process.argv.slice(2)}));`
	if err = os.WriteFile(filepath.Join(p.RuntimeDir, "node_modules/@google/gemini-cli/bundle/gemini.js"), []byte(fixture), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, launch.Command, launch.Args...)
	cmd.Dir = launch.Cwd
	cmd.Env, err = launch.environment("bridge-private-token")
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(output), `"ok":true`) || !strings.Contains(string(output), `"--experimental-acp","--model","gemini-2.5-flash","--extensions","none"`) {
		t.Fatal(string(output))
	}
}

func TestGeminiNativeFreshSessionControls(t *testing.T) {
	runtime := os.Getenv("AGENT_GEMINI_TEST_RUNTIME")
	if runtime == "" {
		t.Skip("set AGENT_GEMINI_TEST_RUNTIME for native ACP initialization")
	}
	for _, trust := range []bool{false, true} {
		t.Run(fmt.Sprintf("trust=%t", trust), func(t *testing.T) {
			testGeminiNativeFreshSessionControls(t, runtime, trust)
		})
	}
}

func testGeminiNativeFreshSessionControls(t *testing.T, runtime string, trust bool) {
	p := geminiTestOptions(t)
	p.RuntimeDir = runtime
	var httpMCPInitialized atomic.Bool
	mcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if r.Header.Get("Authorization") != "Bearer private-mcp-value" {
			t.Error("native HTTP MCP credential binding was not resolved")
			w.WriteHeader(http.StatusUnauthorized)
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
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if len(request.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		result := map[string]any{"tools": []any{}}
		if request.Method == "initialize" {
			httpMCPInitialized.Store(true)
			result = map[string]any{"protocolVersion": request.Params.ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "fixture", "version": "1"}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
	}))
	defer mcp.Close()
	c := geminiTestConfiguration()
	c.Shared.MCPServers = []ManagedMCPServer{{Name: "http-fixture", URL: mcp.URL, Transport: "http", HeaderBindings: map[string]CredentialHeader{"Authorization": {Credential: "docs", Prefix: "Bearer "}}}}
	c.Launch.TrustWorkspace = trust
	if !trust {
		c.Shared.MCPServers = make([]ManagedMCPServer, 0)
	}
	desired, _ := json.Marshal(c)
	launch, err := p.Prepare(context.Background(), t.TempDir(), desired)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, launch.Command, launch.Args...)
	cmd.Dir = launch.Cwd
	cmd.Env, err = launch.environment("bridge-private-token")
	if err != nil {
		t.Fatal(err)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); _ = cmd.Wait() }()
	scanner := bufio.NewScanner(stdout)
	seq := 0
	call := func(method string, params any) json.RawMessage {
		t.Helper()
		seq++
		data, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": seq, "method": method, "params": params})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = stdin.Write(append(data, '\n')); err != nil {
			t.Fatal(err)
		}
		for scanner.Scan() {
			var response struct {
				ID     int             `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
			}
			if json.Unmarshal(scanner.Bytes(), &response) != nil || response.ID != seq {
				continue
			}
			if len(response.Error) != 0 {
				t.Fatalf("native %s failed (error output intentionally omitted)", method)
			}
			return response.Result
		}
		t.Fatalf("native %s did not respond: %v", method, scanner.Err())
		return nil
	}
	initialized := call("initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}})
	var info struct {
		AgentInfo struct {
			Version string `json:"version"`
		} `json:"agentInfo"`
		AuthMethods []struct {
			ID string `json:"id"`
		} `json:"authMethods"`
	}
	if err = json.Unmarshal(initialized, &info); err != nil {
		t.Fatal(err)
	}
	if info.AgentInfo.Version != geminiVersion {
		t.Fatal("incorrect native agent version")
	}
	found := false
	for _, method := range info.AuthMethods {
		if method.ID == "gemini-api-key" {
			found = true
		}
	}
	if !found {
		t.Fatal("native API key authentication not advertised")
	}
	// These controls configure a session with a dummy key; none sends a prompt.
	call("authenticate", map[string]string{"methodId": "gemini-api-key"})
	created := call("session/new", map[string]any{"cwd": p.StateDir, "mcpServers": []any{}})
	var session struct {
		SessionID string `json:"sessionId"`
	}
	if err = json.Unmarshal(created, &session); err != nil || session.SessionID == "" {
		t.Fatal("missing native session ID")
	}
	if trust && !httpMCPInitialized.Load() {
		t.Fatal("native HTTP MCP did not initialize")
	}
	call("session/set_model", map[string]string{"sessionId": session.SessionID, "modelId": "gemini-2.5-flash"})
	call("session/set_mode", map[string]string{"sessionId": session.SessionID, "modeId": "default"})
	// ACP cancellation is a notification, not a request with a response.
	cancelMessage, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "session/cancel", "params": map[string]string{"sessionId": session.SessionID}})
	if _, err = stdin.Write(append(cancelMessage, '\n')); err != nil {
		t.Fatal(err)
	}
	call("session/set_mode", map[string]string{"sessionId": session.SessionID, "modeId": "default"})
}
