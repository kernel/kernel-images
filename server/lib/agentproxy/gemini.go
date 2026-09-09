package agentproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

const geminiVersion = "0.58.0"

type GeminiOptions struct {
	StateDir    string            `json:"stateDir"`
	RuntimeDir  string            `json:"runtimeDir"`
	Node        string            `json:"node"`
	Credentials map[string]string `json:"credentials"`
}

type GeminiConfiguration struct {
	Launch GeminiLaunch `json:"launch"`
	Shared GeminiShared `json:"shared"`
}
type GeminiLaunch struct {
	Model          string `json:"model"`
	Credential     string `json:"credential"`
	TrustWorkspace bool   `json:"trustWorkspace"`
}
type GeminiShared struct {
	Settings   GeminiSettings     `json:"settings"`
	MCPServers []ManagedMCPServer `json:"mcpServers"`
}
type GeminiSettings struct {
	MaxSessionTurns int `json:"maxSessionTurns"`
}

var geminiEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var geminiModel = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

func (p GeminiOptions) validate() error {
	if !filepath.IsAbs(p.StateDir) || !filepath.IsAbs(p.RuntimeDir) || !filepath.IsAbs(p.Node) {
		return errors.New("gemini paths must be absolute")
	}
	for name, source := range p.Credentials {
		if name == "" || !geminiEnvName.MatchString(source) || !validEnvName(source) {
			return errors.New("invalid gemini credential binding")
		}
	}
	return nil
}

func (p GeminiOptions) validateDesired(c GeminiConfiguration) error {
	if !geminiModel.MatchString(c.Launch.Model) {
		return errors.New("a native Gemini model ID is required")
	}
	if c.Shared.Settings.MaxSessionTurns < 1 || c.Shared.Settings.MaxSessionTurns > 100 {
		return errors.New("maxSessionTurns must be between 1 and 100")
	}
	bindings, err := validateMCPServers(c.Shared.MCPServers)
	if err != nil {
		return err
	}
	for _, s := range c.Shared.MCPServers {
		if s.Name == "__proto__" || s.Name == "constructor" || s.Name == "prototype" {
			return errors.New("reserved MCP server name")
		}
		if s.Command != "" && !c.Launch.TrustWorkspace {
			return errors.New("Gemini stdio MCP requires explicit trustWorkspace")
		}
		for name := range s.EnvBindings {
			if !geminiEnvName.MatchString(name) {
				return errors.New("invalid MCP environment name")
			}
		}
		// Gemini expands settings strings natively; only managed bindings may interpolate.
		values := append([]string{s.Command, s.URL}, s.Args...)
		for _, header := range s.HeaderBindings {
			values = append(values, header.Prefix)
		}
		for _, value := range values {
			if strings.ContainsAny(value, "$\x00") {
				return errors.New("MCP settings must not contain environment interpolation or NUL")
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

func (p GeminiOptions) Prepare(ctx context.Context, dir string, desired json.RawMessage) (Harness, error) {
	var c GeminiConfiguration
	if err := json.Unmarshal(desired, &c); err != nil {
		return Harness{}, err
	}
	if err := p.validateDesired(c); err != nil {
		return Harness{}, err
	}
	if err := atomicWrite(filepath.Join(dir, "config.json"), desired); err != nil {
		return Harness{}, err
	}

	// Generated settings contain environment references, never credential values.
	refs := make(map[string]string)
	aliases := make(map[string]string)
	alias := func(binding string) string {
		source := p.Credentials[binding]
		name, ok := aliases[source]
		if !ok {
			name = fmt.Sprintf("KERNEL_GEMINI_SECRET_%d", len(refs))
			refs[name] = source
			aliases[source] = name
		}
		return "${" + name + "}"
	}
	servers := make(map[string]any)
	for _, s := range c.Shared.MCPServers {
		server := map[string]any{"trust": false, "timeout": 15000}
		if s.Command != "" {
			env := make(map[string]string)
			for name, binding := range s.EnvBindings {
				env[name] = alias(binding)
			}
			server["command"], server["env"] = s.Command, env
			if len(s.Args) != 0 {
				server["args"] = s.Args
			}
		} else {
			headers := make(map[string]string)
			for name, header := range s.HeaderBindings {
				headers[name] = header.Prefix + alias(header.Credential)
			}
			server["url"], server["type"], server["headers"] = s.URL, s.Transport, headers
			server["oauth"] = map[string]bool{"enabled": false}
		}
		servers[s.Name] = server
	}
	blocked := []string{"GEMINI_API_KEY"}
	for name := range refs {
		blocked = append(blocked, name)
	}
	sort.Strings(blocked)
	settings := map[string]any{
		"model":       map[string]any{"name": c.Launch.Model, "maxSessionTurns": c.Shared.Settings.MaxSessionTurns},
		"mcpServers":  servers,
		"general":     map[string]bool{"enableAutoUpdate": false, "enableAutoUpdateNotification": false},
		"privacy":     map[string]bool{"usageStatisticsEnabled": false},
		"telemetry":   map[string]bool{"enabled": false},
		"hooksConfig": map[string]bool{"enabled": false},
		"advanced":    map[string]bool{"ignoreLocalEnv": true},
		"security": map[string]any{
			"auth":                         map[string]string{"selectedType": "gemini-api-key"},
			"environmentVariableRedaction": map[string]any{"enabled": true, "blocked": blocked},
		},
	}
	data, err := json.Marshal(settings)
	if err != nil {
		return Harness{}, err
	}
	if err = atomicWrite(filepath.Join(dir, "settings.json"), data); err != nil {
		return Harness{}, err
	}
	if err = atomicWrite(filepath.Join(dir, "system-defaults.json"), []byte("{}")); err != nil {
		return Harness{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, p.Node, filepath.Join(p.RuntimeDir, "node_modules/@google/gemini-cli/bundle/gemini.js"), "--version")
	command.Dir = dir
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "GEMINI_CLI_HOME=" + dir, "GEMINI_CLI_SYSTEM_SETTINGS_PATH=" + filepath.Join(dir, "settings.json"), "GEMINI_CLI_SYSTEM_DEFAULTS_PATH=" + filepath.Join(dir, "system-defaults.json")}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
	command.WaitDelay = time.Second
	output, err := command.Output()
	if command.Process != nil {
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
	if err != nil || strings.TrimSpace(string(output)) != geminiVersion {
		return Harness{}, errors.New("pinned Gemini runtime validation failed")
	}
	if err = os.MkdirAll(filepath.Join(p.StateDir, "home"), 0700); err != nil {
		return Harness{}, err
	}
	bindings, _ := json.Marshal(refs)
	sources := map[string]bool{p.Credentials[c.Launch.Credential]: true}
	for _, source := range refs {
		sources[source] = true
	}
	inherited := make([]string, 0, len(sources))
	for source := range sources {
		inherited = append(inherited, source)
	}
	sort.Strings(inherited)
	return Harness{
		Command: p.Node, Args: []string{filepath.Join(p.RuntimeDir, "launch.mjs"), filepath.Join(dir, "config.json")}, Cwd: p.StateDir,
		Env: map[string]string{
			"HOME":                            filepath.Join(p.StateDir, "home"),
			"GEMINI_CLI_HOME":                 filepath.Join(p.StateDir, "home"),
			"GEMINI_CLI_SYSTEM_SETTINGS_PATH": filepath.Join(dir, "settings.json"),
			"GEMINI_CLI_SYSTEM_DEFAULTS_PATH": filepath.Join(dir, "system-defaults.json"),
			"GEMINI_CLI_TRUST_WORKSPACE":      fmt.Sprint(c.Launch.TrustWorkspace),
			"KERNEL_GEMINI_BINDINGS":          string(bindings),
			"KERNEL_GEMINI_PROVIDER_SOURCE":   p.Credentials[c.Launch.Credential],
		}, InheritEnv: inherited,
	}, nil
}
