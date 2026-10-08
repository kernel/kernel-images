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
	if len(policy) != 3 {
		t.Fatalf("pin sets %d policies, want ProxySettings, WebRtcIPHandling and WebRtcIPHandlingUrl: %s", len(policy), data)
	}
	if string(policy["WebRtcIPHandlingUrl"]) != "[]" {
		t.Fatalf("pin leaves per-URL WebRTC rules in place: %s", data)
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
// pin has to reproduce the egress proxy from the base flags and the bypass list
// Chromium starts with.
func TestPinSyncFollowsChromiumFlags(t *testing.T) {
	const proxy = "--proxy-server=http://192.0.2.1:3129"
	tests := []struct {
		name  string
		base  []string
		final []string
		want  proxySettings
	}{
		{
			name:  "image default bypass",
			base:  []string{"--kiosk", proxy},
			final: []string{"--kiosk", proxy, "--proxy-bypass-list=10.0.0.0/8;172.16.0.0/12;192.168.0.0/16;100.64.0.0/10;fc00::/7"},
			want:  proxySettings{ProxyMode: "fixed_servers", ProxyServer: "http://192.0.2.1:3129", ProxyBypassList: "10.0.0.0/8;172.16.0.0/12;192.168.0.0/16;100.64.0.0/10;fc00::/7"},
		},
		{
			name:  "runtime bypass list after base flags wins",
			base:  []string{proxy, "--proxy-bypass-list=10.0.0.0/8"},
			final: []string{proxy, "--proxy-bypass-list=10.0.0.0/8", "--proxy-bypass-list=preview.internal"},
			want:  proxySettings{ProxyMode: "fixed_servers", ProxyServer: "http://192.0.2.1:3129", ProxyBypassList: "preview.internal"},
		},
		{
			name:  "runtime proxy server is not pinned",
			base:  []string{"--proxy-server=http://old:1", proxy},
			final: []string{"--proxy-server=http://old:1", proxy, "--proxy-server=direct://"},
			want:  proxySettings{ProxyMode: "fixed_servers", ProxyServer: "http://192.0.2.1:3129"},
		},
		{
			name:  "explicit empty bypass list",
			base:  []string{proxy},
			final: []string{proxy, "--proxy-bypass-list=10.0.0.0/8", "--proxy-bypass-list="},
			want:  proxySettings{ProxyMode: "fixed_servers", ProxyServer: "http://192.0.2.1:3129"},
		},
		{
			name:  "bare empty bypass list",
			base:  []string{proxy},
			final: []string{proxy, "--proxy-bypass-list"},
			want:  proxySettings{ProxyMode: "fixed_servers", ProxyServer: "http://192.0.2.1:3129"},
		},
		{
			name:  "no bypass list",
			base:  []string{proxy},
			final: []string{proxy},
			want:  proxySettings{ProxyMode: "fixed_servers", ProxyServer: "http://192.0.2.1:3129"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := testPin(t)
			if err := p.Sync(true, tt.base, tt.final); err != nil {
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
	flags := []string{"--proxy-server=http://192.0.2.1:3129"}
	if err := p.Sync(true, flags, flags); err == nil {
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
// A --proxy-server that only the runtime flags carry does not count: anyone
// holding the session's token can write those.
func TestPinSyncRefusesFilteredSessionWithoutProxy(t *testing.T) {
	runtimeProxy := []string{"--proxy-server=http://192.0.2.1:3129"}
	for _, base := range [][]string{nil, {"--proxy-server="}, {"--proxy-server"}} {
		p := testPin(t)
		if err := p.Sync(true, base, runtimeProxy); err == nil {
			t.Fatalf("Sync(true, %q) succeeded", base)
		}
		if present, _ := p.Present(); present {
			t.Fatalf("Sync(true, %q) wrote a pin", base)
		}
	}
}

// Removing an allowlist hands the proxy back to the command line, so an
// extension the customer chose can set it again.
func TestPinSyncRemovesPinWhenUnfiltered(t *testing.T) {
	p := testPin(t)
	flags := []string{"--proxy-server=http://192.0.2.1:3129"}
	if err := p.Sync(true, flags, flags); err != nil {
		t.Fatalf("Sync(true): %v", err)
	}
	for range 2 {
		if err := p.Sync(false, nil, nil); err != nil {
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
