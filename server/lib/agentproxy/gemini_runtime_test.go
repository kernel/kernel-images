package agentproxy

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

func TestGeminiNativeInitialize(t *testing.T) {
	runtime := os.Getenv("AGENT_GEMINI_TEST_RUNTIME")
	if runtime == "" {
		t.Skip("set AGENT_GEMINI_TEST_RUNTIME for native ACP initialization")
	}
	p := geminiTestOptions(t)
	p.RuntimeDir = runtime
	c := geminiTestConfiguration()
	c.Shared.MCPServers = make([]ManagedMCPServer, 0)
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
	if _, err = stdin.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1,"clientCapabilities":{}}}` + "\n")); err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		var response struct {
			ID     int `json:"id"`
			Result struct {
				AgentInfo struct {
					Version string `json:"version"`
				} `json:"agentInfo"`
				AuthMethods []struct {
					ID string `json:"id"`
				} `json:"authMethods"`
			} `json:"result"`
		}
		if json.Unmarshal(scanner.Bytes(), &response) != nil || response.ID != 1 {
			continue
		}
		if response.Result.AgentInfo.Version != geminiVersion {
			t.Fatal("incorrect native agent version")
		}
		for _, method := range response.Result.AuthMethods {
			if method.ID == "gemini-api-key" {
				return
			}
		}
		t.Fatal("native API key authentication not advertised")
	}
	t.Fatal("native initialize did not respond", scanner.Err())
}
