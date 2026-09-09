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

type ClaudeOptions struct {
	StateDir    string            `json:"stateDir"`
	RuntimeDir  string            `json:"runtimeDir"`
	Node        string            `json:"node"`
	Credentials map[string]string `json:"credentials"`
}

type ClaudeConfiguration struct {
	Launch ClaudeLaunch `json:"launch"`
	Shared ClaudeShared `json:"shared"`
}

type ClaudeLaunch struct {
	Model      string `json:"model"`
	Credential string `json:"credential"`
}

type ClaudeShared struct {
	Settings   ClaudeSettings     `json:"settings"`
	MCPServers []ManagedMCPServer `json:"mcpServers"`
}

type ClaudeSettings struct {
	Language              string `json:"language,omitempty"`
	AlwaysThinkingEnabled *bool  `json:"alwaysThinkingEnabled,omitempty"`
}

var claudeEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func (p ClaudeOptions) validate() error {
	if !filepath.IsAbs(p.StateDir) || !filepath.IsAbs(p.RuntimeDir) || !filepath.IsAbs(p.Node) {
		return errors.New("claude paths must be absolute")
	}
	for name, source := range p.Credentials {
		if name == "" || !claudeEnvName.MatchString(source) || !validEnvName(source) {
			return errors.New("invalid claude credential binding")
		}
	}
	return nil
}

func (p ClaudeOptions) validateDesired(c ClaudeConfiguration) error {
	if !strings.HasPrefix(c.Launch.Model, "claude-") || len(c.Launch.Model) > 128 || strings.ContainsAny(c.Launch.Model, "\x00\r\n ") {
		return errors.New("a native Anthropic claude model ID is required")
	}
	if len(c.Shared.Settings.Language) > 128 || strings.ContainsAny(c.Shared.Settings.Language, "\x00\r\n") {
		return errors.New("invalid claude language setting")
	}
	bindings, err := validateMCPServers(c.Shared.MCPServers)
	if err != nil {
		return err
	}
	for _, s := range c.Shared.MCPServers {
		if s.Command == "" {
			return errors.New("claude shared MCP currently supports stdio only")
		}
		for name := range s.EnvBindings {
			if !claudeEnvName.MatchString(name) {
				return errors.New("invalid MCP environment name")
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

func (p ClaudeOptions) Prepare(ctx context.Context, dir string, desired json.RawMessage) (Harness, error) {
	var c ClaudeConfiguration
	if err := json.Unmarshal(desired, &c); err != nil {
		return Harness{}, err
	}
	if err := p.validateDesired(c); err != nil {
		return Harness{}, err
	}
	if err := atomicWrite(filepath.Join(dir, "config.json"), desired); err != nil {
		return Harness{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, p.Node, filepath.Join(p.RuntimeDir, "prepare.mjs"), dir)
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "CLAUDE_CONFIG_DIR=" + dir}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
	command.WaitDelay = time.Second
	err := command.Run()
	if command.Process != nil {
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
	if err != nil {
		return Harness{}, errors.New("claude native preparation or validation failed")
	}
	refs := map[string]string{c.Launch.Credential: p.Credentials[c.Launch.Credential]}
	for _, s := range c.Shared.MCPServers {
		for _, binding := range s.EnvBindings {
			refs[binding] = p.Credentials[binding]
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
			"HOME":                   filepath.Join(p.StateDir, "home"),
			"CLAUDE_CONFIG_DIR":      filepath.Join(p.StateDir, "native"),
			"KERNEL_CLAUDE_BINDINGS": string(bindings),
		},
		InheritEnv: inherited,
	}, nil
}
