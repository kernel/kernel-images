package agentproxy

import "testing"

func TestManagedMCPValidation(t *testing.T) {
	for _, test := range []struct {
		name   string
		server ManagedMCPServer
		valid  bool
	}{
		{"stdio", ManagedMCPServer{Name: "docs", Command: "/bin/server", EnvBindings: map[string]string{"TOKEN": "docs"}}, true},
		{"http", ManagedMCPServer{Name: "docs", URL: "https://example.com/mcp", Transport: "http", HeaderBindings: map[string]CredentialHeader{"Authorization": {Credential: "docs", Prefix: "Bearer "}}}, true},
		{"sse", ManagedMCPServer{Name: "docs", URL: "https://example.com/sse", Transport: "sse"}, true},
		{"relative command", ManagedMCPServer{Name: "docs", Command: "server"}, false},
		{"mixed transports", ManagedMCPServer{Name: "docs", Command: "/bin/server", URL: "https://example.com"}, false},
		{"URL credential", ManagedMCPServer{Name: "docs", URL: "https://user:password@example.com", Transport: "http"}, false},
		{"URL query", ManagedMCPServer{Name: "docs", URL: "https://example.com?token=x", Transport: "http"}, false},
		{"header injection", ManagedMCPServer{Name: "docs", URL: "https://example.com", Transport: "http", HeaderBindings: map[string]CredentialHeader{"Bad Header": {Credential: "docs"}}}, false},
		{"NUL argument", ManagedMCPServer{Name: "docs", Command: "/bin/server", Args: []string{"\x00"}}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := validateMCPServers([]ManagedMCPServer{test.server})
			if (err == nil) != test.valid {
				t.Fatal(err)
			}
		})
	}
	server := ManagedMCPServer{Name: "docs", Command: "/bin/server"}
	if _, err := validateMCPServers([]ManagedMCPServer{server, server}); err == nil {
		t.Fatal("duplicate name accepted")
	}
}
