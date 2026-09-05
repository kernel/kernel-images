// Package agentproxy exposes ACP WebSockets without interpreting ACP messages.
package agentproxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Config is trusted, operator-provisioned launch configuration. It is not a
// public command-execution API. Harness-specific installers will produce it.
type Config struct {
	ACPRemote      string             `json:"acpremote"`
	MaxConnections int                `json:"maxConnections"`
	Harnesses      map[string]Harness `json:"harnesses"`
}

type Harness struct {
	Command    string            `json:"command"`
	Args       []string          `json:"args"`
	Cwd        string            `json:"cwd"`
	Env        map[string]string `json:"env"`
	InheritEnv []string          `json:"inheritEnv"`
}

func Load(path string) (Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return Config{}, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, (64<<10)+1))
	if err != nil {
		return Config{}, err
	}
	if len(data) > 64<<10 {
		return Config{}, errors.New("agent configuration exceeds 64 KiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var config Config
	if err := decoder.Decode(&config); err != nil {
		return Config{}, errors.New("invalid agent configuration")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return Config{}, errors.New("expected one agent configuration")
	}
	if err := config.validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

func (c Config) validate() error {
	if !filepath.IsAbs(c.ACPRemote) {
		return errors.New("acpremote must be an absolute executable path")
	}
	if c.MaxConnections < 1 || c.MaxConnections > 64 {
		return errors.New("maxConnections must be between 1 and 64")
	}
	if len(c.Harnesses) == 0 {
		return errors.New("at least one harness is required")
	}
	for name, harness := range c.Harnesses {
		switch name {
		case "pi", "codex", "claude", "gemini":
		default:
			return fmt.Errorf("unknown harness %q", name)
		}
		if !filepath.IsAbs(harness.Command) || !filepath.IsAbs(harness.Cwd) {
			return fmt.Errorf("%s requires absolute command and cwd paths", name)
		}
		for name, value := range harness.Env {
			if !validEnvName(name) || strings.ContainsRune(value, '\x00') {
				return errors.New("invalid harness environment")
			}
		}
		for _, name := range harness.InheritEnv {
			if !validEnvName(name) {
				return errors.New("invalid inherited environment name")
			}
		}
	}
	return nil
}

func validEnvName(name string) bool {
	return name != "" && !strings.ContainsAny(name, "=\x00") && name != bridgeTokenEnv
}

func (h Harness) environment(token string) ([]string, error) {
	values := make(map[string]string)
	for _, name := range []string{"PATH", "HOME", "USER", "LANG", "TMPDIR", "TERM", "SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if value, ok := os.LookupEnv(name); ok {
			values[name] = value
		}
	}
	for _, name := range h.InheritEnv {
		value, ok := os.LookupEnv(name)
		if !ok || value == "" {
			return nil, errors.New("required harness environment is unavailable")
		}
		values[name] = value
	}
	for name, value := range h.Env {
		values[name] = value
	}
	values[bridgeTokenEnv] = token
	env := make([]string, 0, len(values))
	for name, value := range values {
		env = append(env, name+"="+value)
	}
	return env, nil
}
