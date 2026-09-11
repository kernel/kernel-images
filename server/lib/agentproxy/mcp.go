package agentproxy

import (
	"errors"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
)

type CredentialHeader struct {
	Credential string `json:"credential"`
	Prefix     string `json:"prefix,omitempty"`
}
type ManagedMCPServer struct {
	Name           string                      `json:"name"`
	Command        string                      `json:"command,omitempty"`
	Args           []string                    `json:"args,omitempty"`
	EnvBindings    map[string]string           `json:"envBindings,omitempty"`
	URL            string                      `json:"url,omitempty"`
	Transport      string                      `json:"transport,omitempty"`
	HeaderBindings map[string]CredentialHeader `json:"headerBindings,omitempty"`
}

var headerName = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")

func validateMCPServers(servers []ManagedMCPServer) ([]string, error) {
	if len(servers) > 32 {
		return nil, errors.New("at most 32 MCP servers are supported")
	}
	bindings := make([]string, 0)
	names := make(map[string]bool)
	for _, server := range servers {
		if server.Name == "" || names[server.Name] {
			return nil, errors.New("MCP server names must be nonempty and unique")
		}
		names[server.Name] = true
		if err := validateMCPTransport(server); err != nil {
			return nil, err
		}
		for _, arg := range server.Args {
			if strings.ContainsRune(arg, '\x00') {
				return nil, errors.New("invalid MCP argument")
			}
		}
		for name, binding := range server.EnvBindings {
			if !validEnvName(name) {
				return nil, errors.New("invalid MCP environment name")
			}
			bindings = append(bindings, binding)
		}
		for name, header := range server.HeaderBindings {
			if !headerName.MatchString(name) || strings.ContainsAny(header.Prefix, "\r\n\x00") {
				return nil, errors.New("invalid MCP header")
			}
			bindings = append(bindings, header.Credential)
		}
	}
	return bindings, nil
}

func validateMCPTransport(server ManagedMCPServer) error {
	if server.Command != "" {
		if !filepath.IsAbs(server.Command) || server.URL != "" || server.Transport != "" || len(server.HeaderBindings) != 0 {
			return errors.New("invalid stdio MCP server")
		}
		return nil
	}
	u, err := url.Parse(server.URL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || len(server.Args) != 0 || len(server.EnvBindings) != 0 {
		return errors.New("MCP URL must be HTTP(S), without embedded credentials or query parameters")
	}
	if server.Transport != "http" && server.Transport != "sse" {
		return errors.New("MCP transport must be http or sse")
	}
	return nil
}
