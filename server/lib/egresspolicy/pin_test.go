package egresspolicy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// testPin lays out a policy directory the way the image does, so the pin's
// temporary file lands where it would on a real VM.
func testPin(t *testing.T) Pin {
	t.Helper()
	managed := filepath.Join(t.TempDir(), "managed")
	if err := os.Mkdir(managed, 0o755); err != nil {
		t.Fatalf("create policy dir: %v", err)
	}
	return Pin{Path: filepath.Join(managed, "zz-kernel-egress.json")}
}

func readPin(t *testing.T, p Pin) proxySettings {
	t.Helper()
	data, err := os.ReadFile(p.Path)
	if err != nil {
		t.Fatalf("read pin: %v", err)
	}
	var policy map[string]json.RawMessage
	if err := json.Unmarshal(data, &policy); err != nil {
		t.Fatalf("decode pin: %v", err)
	}
	if len(policy) != 2 {
		t.Fatalf("pin sets %d policies, want ProxySettings and WebRtcIPHandling: %s", len(policy), data)
	}
	var pinned pinPolicy
	if err := json.Unmarshal(data, &pinned); err != nil {
		t.Fatalf("decode pin: %v", err)
	}
	if pinned.WebRtcIPHandling != "disable_non_proxied_udp" {
		t.Fatalf("pin lets WebRTC send UDP around the proxy: %s", data)
	}
	return pinned.ProxySettings
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

// Chromium applies every file in the policy directory, whatever its name, so a
// write that fails before the rename must not leave its temporary file there.
func TestPinSyncFailureLeavesNothingInThePolicyDirectory(t *testing.T) {
	p := testPin(t)
	// A directory in the pin's place makes the rename fail after the write.
	if err := os.MkdirAll(filepath.Join(p.Path, "blocker"), 0o755); err != nil {
		t.Fatalf("block pin path: %v", err)
	}
	if err := p.Sync(true, []string{"--proxy-server=http://192.0.2.1:3129"}); err == nil {
		t.Fatal("Sync succeeded over a directory")
	}
	entries, err := os.ReadDir(filepath.Dir(p.Path))
	if err != nil {
		t.Fatalf("read policy dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(p.Path) {
		t.Fatalf("policy dir holds %v, want only the blocked pin path", entries)
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
