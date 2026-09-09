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

type PiOptions struct {
	StateDir    string            `json:"stateDir"`
	RuntimeDir  string            `json:"runtimeDir"`
	Node        string            `json:"node"`
	Credentials map[string]string `json:"credentials"`
	Registry    string            `json:"registry,omitempty"`
}

type PiConfiguration struct {
	Launch PiLaunch `json:"launch"`
	Shared PiShared `json:"shared"`
}
type PiLaunch struct {
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	Thinking   string `json:"thinking"`
	Credential string `json:"credential"`
}
type PiShared struct {
	Extensions []string           `json:"extensions"`
	MCPServers []ManagedMCPServer `json:"mcpServers"`
}

var pinnedPackage = regexp.MustCompile(`^npm:(@[-a-z0-9_.]+/)?[a-z0-9][-a-z0-9_.]*@[0-9]+\.[0-9]+\.[0-9]+(-[a-zA-Z0-9.-]+)?$`)
var providerEnv = map[string]string{"openrouter": "OPENROUTER_API_KEY", "openai": "OPENAI_API_KEY", "anthropic": "ANTHROPIC_API_KEY", "google": "GEMINI_API_KEY"}

func (p PiOptions) validate() error {
	if !filepath.IsAbs(p.StateDir) || !filepath.IsAbs(p.RuntimeDir) || !filepath.IsAbs(p.Node) {
		return errors.New("pi paths must be absolute")
	}
	for name, source := range p.Credentials {
		if name == "" || !validEnvName(source) {
			return errors.New("invalid credential binding")
		}
	}
	return nil
}
func (p PiOptions) validateDesired(c PiConfiguration) error {
	if providerEnv[c.Launch.Provider] == "" || strings.TrimSpace(c.Launch.Model) == "" || len(c.Launch.Model) > 512 || strings.HasPrefix(c.Launch.Model, "-") || strings.ContainsAny(c.Launch.Model, "\x00\r\n") {
		return errors.New("supported provider and model are required")
	}
	switch c.Launch.Thinking {
	case "off", "minimal", "low", "medium", "high", "xhigh":
	default:
		return errors.New("invalid thinking level")
	}
	if len(c.Shared.Extensions) > 32 {
		return errors.New("at most 32 extensions are supported")
	}
	for _, source := range c.Shared.Extensions {
		if !pinnedPackage.MatchString(source) {
			return errors.New("extensions must be exact-version npm package references")
		}
	}
	bindings, err := validateMCPServers(c.Shared.MCPServers)
	if err != nil {
		return err
	}
	for _, binding := range append(bindings, c.Launch.Credential) {
		source, ok := p.Credentials[binding]
		if !ok || os.Getenv(source) == "" {
			return errors.New("credential binding is unavailable")
		}
	}
	return nil
}

func (p PiOptions) Prepare(ctx context.Context, dir string, desired json.RawMessage) (Harness, error) {
	var c PiConfiguration
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
	command := exec.CommandContext(ctx, p.Node, filepath.Join(p.RuntimeDir, "prepare.mjs"), dir, p.StateDir)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
	command.WaitDelay = time.Second
	registry := p.Registry
	if registry == "" {
		registry = "https://registry.npmjs.org"
	}
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "PI_SKIP_VERSION_CHECK=1", "KERNEL_PI_REGISTRY=" + registry}
	err := command.Run()
	if command.Process != nil {
		// Extension validation must not leave preparation-time descendants running.
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
	if err != nil {
		return Harness{}, errors.New("pi package preparation or validation failed")
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
	return Harness{Command: p.Node, Args: []string{filepath.Join(p.RuntimeDir, "launch.mjs"), filepath.Join(dir, "config.json")}, Cwd: p.StateDir, Env: map[string]string{"HOME": filepath.Join(p.StateDir, "home"), "PI_CODING_AGENT_DIR": filepath.Join(p.StateDir, "current", "agent"), "KERNEL_PI_STATE": p.StateDir, "KERNEL_PI_BINDINGS": string(bindings), "KERNEL_PI_PROVIDER_ENV": providerEnv[c.Launch.Provider], "KERNEL_PI_PROVIDER_SOURCE": p.Credentials[c.Launch.Credential]}, InheritEnv: inherited}, nil
}
