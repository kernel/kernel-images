package egresspolicy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func testPin(t *testing.T) Pin {
	t.Helper()
	return Pin{Path: filepath.Join(t.TempDir(), "kernel-egress.json")}
}

func readPin(t *testing.T, p Pin) proxySettings {
	t.Helper()
	data, err := os.ReadFile(p.Path)
	if err != nil {
		t.Fatalf("read pin: %v", err)
	}
	var policy map[string]proxySettings
	if err := json.Unmarshal(data, &policy); err != nil {
		t.Fatalf("decode pin: %v", err)
	}
	if len(policy) != 1 {
		t.Fatalf("pin sets %d policies, want only ProxySettings: %s", len(policy), data)
	}
	settings, ok := policy["ProxySettings"]
	if !ok {
		t.Fatalf("pin does not set ProxySettings: %s", data)
	}
	return settings
}

// Once the pin is in place Chromium ignores the proxy flags entirely, so the
// pin has to reproduce what the last occurrence of each flag says.
func TestPinSyncFollowsChromiumFlags(t *testing.T) {
	tests := []struct {
		name  string
		flags []string
		want  proxySettings
	}{
		{
			name:  "image default bypass",
			flags: []string{"--kiosk", "--proxy-server=http://192.0.2.1:3129", "--proxy-bypass-list=10.0.0.0/8;172.16.0.0/12;192.168.0.0/16;100.64.0.0/10;fc00::/7"},
			want:  proxySettings{ProxyMode: "fixed_servers", ProxyServer: "http://192.0.2.1:3129", ProxyBypassList: "10.0.0.0/8;172.16.0.0/12;192.168.0.0/16;100.64.0.0/10;fc00::/7"},
		},
		{
			name:  "runtime flags after base flags win",
			flags: []string{"--proxy-server=http://old:1", "--proxy-bypass-list=10.0.0.0/8", "--proxy-server=http://192.0.2.1:3129", "--proxy-bypass-list=preview.internal"},
			want:  proxySettings{ProxyMode: "fixed_servers", ProxyServer: "http://192.0.2.1:3129", ProxyBypassList: "preview.internal"},
		},
		{
			name:  "explicit empty bypass list",
			flags: []string{"--proxy-server=http://192.0.2.1:3129", "--proxy-bypass-list=10.0.0.0/8", "--proxy-bypass-list="},
			want:  proxySettings{ProxyMode: "fixed_servers", ProxyServer: "http://192.0.2.1:3129"},
		},
		{
			name:  "bare empty bypass list",
			flags: []string{"--proxy-server=http://192.0.2.1:3129", "--proxy-bypass-list"},
			want:  proxySettings{ProxyMode: "fixed_servers", ProxyServer: "http://192.0.2.1:3129"},
		},
		{
			name:  "no bypass list",
			flags: []string{"--proxy-server=http://192.0.2.1:3129"},
			want:  proxySettings{ProxyMode: "fixed_servers", ProxyServer: "http://192.0.2.1:3129"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := testPin(t)
			if err := p.Sync(true, tt.flags); err != nil {
				t.Fatalf("Sync: %v", err)
			}
			if got := readPin(t, p); got != tt.want {
				t.Fatalf("pin mismatch:\n got: %+v\nwant: %+v", got, tt.want)
			}
		})
	}
}

// A filtered session without a proxy to pin would come up with nothing
// stopping an extension from going direct, so the launcher must not start it.
func TestPinSyncRefusesFilteredSessionWithoutProxy(t *testing.T) {
	for _, flags := range [][]string{nil, {"--proxy-server="}, {"--proxy-server"}} {
		p := testPin(t)
		if err := p.Sync(true, flags); err == nil {
			t.Fatalf("Sync(true, %q) succeeded", flags)
		}
		if present, _ := p.Present(); present {
			t.Fatalf("Sync(true, %q) wrote a pin", flags)
		}
	}
}

// Removing an allowlist hands the proxy back to the command line, so an
// extension the customer chose can set it again.
func TestPinSyncRemovesPinWhenUnfiltered(t *testing.T) {
	p := testPin(t)
	if err := p.Sync(true, []string{"--proxy-server=http://192.0.2.1:3129"}); err != nil {
		t.Fatalf("Sync(true): %v", err)
	}
	for range 2 {
		if err := p.Sync(false, nil); err != nil {
			t.Fatalf("Sync(false): %v", err)
		}
		present, err := p.Present()
		if err != nil {
			t.Fatalf("Present: %v", err)
		}
		if present {
			t.Fatal("pin still present after the session became unfiltered")
		}
	}
}
