package agentproxy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

type CodexOptions struct {
	StateDir    string            `json:"stateDir"`
	RuntimeDir  string            `json:"runtimeDir"`
	Node        string            `json:"node"`
	Credentials map[string]string `json:"credentials"`
}

type CodexConfiguration struct {
	Launch CodexLaunch `json:"launch"`
	Shared CodexShared `json:"shared"`
}

type CodexLaunch struct {
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	Thinking   string `json:"thinking"`
	Mode       string `json:"mode"`
	Credential string `json:"credential"`
}

type CodexShared struct {
	Instructions string             `json:"instructions,omitempty"`
	WebSearch    string             `json:"webSearch"`
	MCPServers   []ManagedMCPServer `json:"mcpServers"`
}

var codexMCPName = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
var codexEnvName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

func (p CodexOptions) validate() error {
	if !filepath.IsAbs(p.StateDir) || !filepath.IsAbs(p.RuntimeDir) || !filepath.IsAbs(p.Node) {
		return errors.New("codex paths must be absolute")
	}
	for name, source := range p.Credentials {
		if name == "" || !codexEnvName.MatchString(source) || !validEnvName(source) {
			return errors.New("invalid credential binding")
		}
	}
	return nil
}

func (p CodexOptions) validateDesired(c CodexConfiguration) error {
	if c.Launch.Provider != "openai" || strings.TrimSpace(c.Launch.Model) == "" || len(c.Launch.Model) > 512 || strings.HasPrefix(c.Launch.Model, "-") || strings.ContainsAny(c.Launch.Model, "\x00\r\n") {
		return errors.New("codex requires the openai provider and a model")
	}
	switch c.Launch.Thinking {
	case "minimal", "low", "medium", "high", "xhigh":
	default:
		return errors.New("invalid codex thinking level")
	}
	switch c.Launch.Mode {
	case "read-only", "agent", "agent-full-access":
	default:
		return errors.New("invalid codex mode")
	}
	switch c.Shared.WebSearch {
	case "disabled", "cached", "live":
	default:
		return errors.New("invalid codex webSearch setting")
	}
	if len(c.Shared.Instructions) > 64<<10 || strings.ContainsRune(c.Shared.Instructions, '\x00') {
		return errors.New("invalid codex instructions")
	}
	bindings, err := validateMCPServers(c.Shared.MCPServers)
	if err != nil {
		return err
	}
	for _, s := range c.Shared.MCPServers {
		if !codexMCPName.MatchString(s.Name) || len(s.Name) > 64 {
			return errors.New("codex MCP names must use letters, digits, underscores or hyphens (at most 64)")
		}
		if s.Transport == "sse" {
			return errors.New("codex does not support SSE MCP")
		}
		if strings.ContainsRune(s.Command, '\x00') {
			return errors.New("invalid MCP command")
		}
		for name := range s.EnvBindings {
			if !codexEnvName.MatchString(name) {
				return errors.New("invalid MCP environment name")
			}
		}
		for _, header := range s.HeaderBindings {
			value := os.Getenv(p.Credentials[header.Credential])
			if strings.ContainsAny(value, "\r\n\x00") {
				return errors.New("invalid MCP header credential")
			}
		}
	}
	for _, binding := range append(bindings, c.Launch.Credential) {
		source, ok := p.Credentials[binding]
		if !ok || os.Getenv(source) == "" {
			return errors.New("credential binding is unavailable")
		}
	}
	return nil
}

func (p CodexOptions) Prepare(ctx context.Context, dir string, desired json.RawMessage) (Harness, error) {
	var c CodexConfiguration
	if err := json.Unmarshal(desired, &c); err != nil {
		return Harness{}, err
	}
	if err := p.validateDesired(c); err != nil {
		return Harness{}, err
	}
	if err := atomicWrite(filepath.Join(dir, "config.json"), desired); err != nil {
		return Harness{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, p.Node, filepath.Join(p.RuntimeDir, "prepare.mjs"), dir)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
	command.WaitDelay = time.Second
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir}
	err := command.Run()
	if command.Process != nil {
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
	if err != nil {
		return Harness{}, errors.New("codex runtime or configuration validation failed")
	}
	refs := map[string]string{c.Launch.Credential: p.Credentials[c.Launch.Credential]}
	for _, s := range c.Shared.MCPServers {
		for _, b := range s.EnvBindings {
			refs[b] = p.Credentials[b]
		}
		for _, h := range s.HeaderBindings {
			refs[h.Credential] = p.Credentials[h.Credential]
		}
	}
	bindings, _ := json.Marshal(refs)
	inherited := make([]string, 0, len(refs))
	for _, source := range refs {
		inherited = append(inherited, source)
	}
	return Harness{
		Command: p.Node,
		Args:    []string{filepath.Join(p.RuntimeDir, "launch.mjs"), filepath.Join(dir, "config.json")},
		Cwd:     p.StateDir,
		Env: map[string]string{
			"HOME":                  filepath.Join(p.StateDir, "home"),
			"CODEX_HOME":            filepath.Join(p.StateDir, "native"),
			"KERNEL_CODEX_STATE":    p.StateDir,
			"KERNEL_CODEX_BINDINGS": string(bindings),
		},
		InheritEnv: inherited,
	}, nil
}
