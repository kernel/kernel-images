package harness_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	transport "github.com/kernel/kernel-images/server/experiments/agenttransport"
	"github.com/kernel/kernel-images/server/experiments/agenttransport/acceptance"
	"github.com/kernel/kernel-images/server/experiments/agenttransport/acp"
	"github.com/kernel/kernel-images/server/experiments/agenttransport/probe"
)

const prompt = "Use the acceptance-probe MCP server's checkpoint tool exactly once. Do not use shell commands or inspect files. Wait for the tool result, then reply ACCEPTANCE_COMPLETE."

func TestFixtureProcess(t *testing.T) {
	dir := os.Getenv("AGENT_FIXTURE_PROBE")
	if dir == "" {
		return
	}
	serve := probe.ServeACP
	if os.Getenv("AGENT_FIXTURE_PERMISSION") == "1" {
		serve = probe.ServePermissionACP
	}
	if err := serve(context.Background(), os.Stdin, os.Stdout, probe.Probe{Dir: dir}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}
func TestDeterministicACP(t *testing.T) {
	factory := func(permission bool) acceptance.Factory {
		return func(t *testing.T) acceptance.Fixture {
			dir := t.TempDir()
			config := acp.Config{Command: os.Args[0], Args: []string{"-test.run=^TestFixtureProcess$"}, Cwd: dir, Env: []string{"AGENT_FIXTURE_PROBE=" + dir}}
			if permission {
				config.Env = append(config.Env, "AGENT_FIXTURE_PERMISSION=1")
				t.Cleanup(func() {
					if t.Failed() {
						return
					}
					data, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
					if err != nil {
						t.Fatal(err)
					}
					if bytes.Contains(data, []byte(`"kind":"cancelled"`)) && !bytes.Contains(data, []byte("cancelled checkpoint")) {
						t.Fatal("ACP update emitted during cancellation was lost")
					}
				})
			}
			return fixture(t, dir, config, 30*time.Second)
		}
	}
	acceptance.Run(t, factory(false))
	acceptance.RunCrash(t, factory(false))
	acceptance.RunPermissions(t, factory(true))
}

type harnessConfig struct {
	Harness        string      `json:"harness"`
	HarnessVersion string      `json:"harnessVersion"`
	AdapterVersion string      `json:"adapterVersion"`
	ACP            acp.Config  `json:"acp"`
	PermissionACP  *acp.Config `json:"permissionACP,omitempty"`
}

// TestRealHarness is opt-in: a skipped test is NOT a compatibility pass. Each
// invocation uses a supplied, pinned agent and actual MCP checkpoint tool.
func TestRealHarness(t *testing.T) {
	path := os.Getenv("AGENT_ACCEPTANCE_CONFIG")
	if path == "" {
		t.Skip("NOT VALIDATED: set AGENT_ACCEPTANCE_CONFIG to a pinned harness configuration")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var config harnessConfig
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		t.Fatal(err)
	}
	if config.Harness == "" || config.HarnessVersion == "" || config.AdapterVersion == "" {
		t.Fatal("harness, harnessVersion, and adapterVersion are required evidence")
	}
	executable := filepath.Join(t.TempDir(), "agent-probe")
	build := exec.Command("go", "build", "-o", executable, "../cmd/agent-probe")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build probe: %v\n%s", err, output)
	}
	t.Logf("harness=%s declared_version=%s adapter_version=%s", config.Harness, config.HarnessVersion, config.AdapterVersion)
	factory := func(base acp.Config) acceptance.Factory {
		return func(t *testing.T) acceptance.Fixture {
			dir := t.TempDir()
			if root := os.Getenv("AGENT_ACCEPTANCE_EVIDENCE_DIR"); root != "" {
				if err := os.MkdirAll(root, 0700); err != nil {
					t.Fatal(err)
				}
				dir, err = os.MkdirTemp(root, "acceptance-")
				if err != nil {
					t.Fatal(err)
				}
				t.Logf("private evidence directory: %s", dir)
			}
			// Round trip so each subtest gets independent slices and maps.
			copyData, _ := json.Marshal(base)
			var agent acp.Config
			_ = json.Unmarshal(copyData, &agent)
			agent.Cwd = filepath.Join(dir, "workspace")
			if err := os.Mkdir(agent.Cwd, 0700); err != nil {
				t.Fatal(err)
			}
			replace := strings.NewReplacer("{workspace}", agent.Cwd, "{probeDir}", dir, "{probeCommand}", executable)
			agent.Command = replace.Replace(agent.Command)
			for i := range agent.Args {
				agent.Args[i] = replace.Replace(agent.Args[i])
			}
			for i := range agent.Env {
				agent.Env[i] = replace.Replace(agent.Env[i])
			}
			server, _ := json.Marshal(map[string]any{"name": "acceptance-probe", "command": executable, "args": []string{"--mode", "mcp", "--dir", dir}, "env": []any{}})
			agent.MCPServers = append(agent.MCPServers, server)
			return fixture(t, dir, agent, 2*time.Minute)
		}
	}
	acceptance.Run(t, factory(config.ACP))
	acceptance.RunCrash(t, factory(config.ACP))
	if config.PermissionACP == nil {
		t.Run("permissions", func(t *testing.T) { t.Skip("NOT VALIDATED: permissionACP configuration not provided") })
	} else {
		acceptance.RunPermissions(t, factory(*config.PermissionACP))
	}
}
func fixture(t *testing.T, dir string, config acp.Config, timeout time.Duration) acceptance.Fixture {
	t.Helper()
	stderr, err := os.OpenFile(filepath.Join(dir, "agent.stderr"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stderr.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	client, err := acp.Start(ctx, config, stderr)
	if err != nil {
		t.Fatalf("ACP startup failed: %v (inspect private agent.stderr)", err)
	}
	t.Cleanup(client.Close)
	if err := os.WriteFile(filepath.Join(dir, "capabilities.json"), client.Capabilities, 0600); err != nil {
		t.Fatal(err)
	}
	store, err := transport.OpenFileStore(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := transport.NewRuntime(client, store)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	server := httptest.NewServer(runtime)
	t.Cleanup(func() { runtime.Close(); server.Close() })
	return acceptance.Fixture{URL: server.URL, Probe: probe.Probe{Dir: dir}, Prompt: prompt, Dispatches: client.Dispatches, ForcedStops: client.ForcedStops, ProcessID: client.PID, Kill: client.Close, Timeout: timeout}
}
