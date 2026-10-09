package egresspolicy

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kernel/kernel-images/server/lib/chromiumflags"
	"github.com/kernel/kernel-images/server/lib/policy"
)

// testPin lays out a policy directory the way the image does, with the pin
// staged in its parent.
func testPin(t *testing.T) Pin {
	t.Helper()
	policies := t.TempDir()
	managed := filepath.Join(policies, "managed")
	if err := os.Mkdir(managed, 0o755); err != nil {
		t.Fatalf("create policy dir: %v", err)
	}
	return Pin{Path: filepath.Join(managed, "zz-kernel-egress.json"), StageDir: policies}
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
	if len(policy) != 5 {
		t.Fatalf("pin sets %d policies, want ProxySettings, WebRtcIPHandling, WebRtcIPHandlingUrl, DnsOverHttpsMode and PolicyListMultipleSourceMergeList: %s", len(policy), data)
	}
	if string(policy["WebRtcIPHandlingUrl"]) != "[]" {
		t.Fatalf("pin leaves per-URL WebRTC rules in place: %s", data)
	}
	if string(policy["PolicyListMultipleSourceMergeList"]) != "[]" {
		t.Fatalf("pin lets chrome policy merge per-URL WebRTC rules into its own: %s", data)
	}
	var pinned pinPolicy
	if err := json.Unmarshal(data, &pinned); err != nil {
		t.Fatalf("decode pin: %v", err)
	}
	if pinned.WebRtcIPHandling != "disable_non_proxied_udp" {
		t.Fatalf("pin lets WebRTC send UDP around the proxy: %s", data)
	}
	if pinned.DnsOverHttpsMode != "off" {
		t.Fatalf("pin lets Chromium connect to a DNS-over-HTTPS server around the proxy: %s", data)
	}
	return pinned.ProxySettings
}

// Chromium applies every file in its managed policy directory, and where two
// set the same policy the one that sorts last wins. The pin has to sit beside
// policy.json and sort after it, and be staged outside the directory.
func TestDefaultPinLayout(t *testing.T) {
	dir := filepath.Dir(DefaultPin.Path)
	if filepath.Dir(policy.PolicyPath) != dir {
		t.Fatalf("pin %s is not beside %s", DefaultPin.Path, policy.PolicyPath)
	}
	if filepath.Base(DefaultPin.Path) <= filepath.Base(policy.PolicyPath) {
		t.Fatalf("pin %s does not sort after %s", DefaultPin.Path, policy.PolicyPath)
	}
	if rel, err := filepath.Rel(dir, DefaultPin.StageDir); err != nil || !strings.HasPrefix(rel, "..") {
		t.Fatalf("pin is staged in %s, inside the policy directory %s", DefaultPin.StageDir, dir)
	}
}

// Once the pin is in place Chromium ignores the proxy flags entirely, so the
// pin has to reproduce the egress proxy from the base flags, and the bypass list
// from the private hosts the control plane sent, or from the base flags when it
// sent none.
func TestPinSyncFollowsBaseFlagsAndPolicy(t *testing.T) {
	const proxy = "--proxy-server=http://192.0.2.1:3129"
	const defaultBypass = "--proxy-bypass-list=10.0.0.0/8;172.16.0.0/12;192.168.0.0/16;100.64.0.0/10;fc00::/7"
	private := []string{"10.1.0.0/16", "preview.internal:8443"}
	tests := []struct {
		name   string
		base   []string
		policy Policy
		want   proxySettings
	}{
		{
			name:   "no private hosts keeps the image default bypass",
			base:   []string{"--kiosk", proxy, defaultBypass},
			policy: Policy{Filtered: true},
			want:   proxySettings{ProxyMode: "fixed_servers", ProxyServer: "http://192.0.2.1:3129", ProxyBypassList: "10.0.0.0/8;172.16.0.0/12;192.168.0.0/16;100.64.0.0/10;fc00::/7"},
		},
		{
			name:   "private hosts replace the base bypass list",
			base:   []string{proxy, defaultBypass},
			policy: Policy{Filtered: true, PrivateHosts: &private},
			want:   proxySettings{ProxyMode: "fixed_servers", ProxyServer: "http://192.0.2.1:3129", ProxyBypassList: "10.1.0.0/16;preview.internal:8443"},
		},
		{
			name:   "empty private hosts bypass nothing",
			base:   []string{proxy, defaultBypass},
			policy: Policy{Filtered: true, PrivateHosts: &[]string{}},
			want:   proxySettings{ProxyMode: "fixed_servers", ProxyServer: "http://192.0.2.1:3129"},
		},
		{
			name:   "last base proxy server wins",
			base:   []string{"--proxy-server=http://old:1", proxy},
			policy: Policy{Filtered: true},
			want:   proxySettings{ProxyMode: "fixed_servers", ProxyServer: "http://192.0.2.1:3129"},
		},
		{
			name:   "bare empty base bypass list",
			base:   []string{proxy, "--proxy-bypass-list"},
			policy: Policy{Filtered: true},
			want:   proxySettings{ProxyMode: "fixed_servers", ProxyServer: "http://192.0.2.1:3129"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := testPin(t)
			if err := p.Sync(tt.policy, tt.base); err != nil {
				t.Fatalf("Sync: %v", err)
			}
			if got := readPin(t, p); got != tt.want {
				t.Fatalf("pin mismatch:\n got: %+v\nwant: %+v", got, tt.want)
			}
		})
	}
}

// The launcher and the egress policy handler both derive the pin from
// BaseFlags, so a session the control plane sends no private hosts for keeps
// the image's default bypass, or the bypass list in CHROMIUM_FLAGS.
func TestPinSyncFromBaseFlags(t *testing.T) {
	const proxy = "--proxy-server=http://192.0.2.1:3129"
	for chromiumFlags, want := range map[string]string{
		proxy: "10.0.0.0/8;172.16.0.0/12;192.168.0.0/16;100.64.0.0/10;fc00::/7",
		proxy + " --proxy-bypass-list=preview.internal": "preview.internal",
	} {
		p := testPin(t)
		if err := p.Sync(Policy{Filtered: true}, BaseFlags(chromiumFlags)); err != nil {
			t.Fatalf("Sync(%q): %v", chromiumFlags, err)
		}
		if got := readPin(t, p).ProxyBypassList; got != want {
			t.Fatalf("bypass list pinned from %q = %q, want %q", chromiumFlags, got, want)
		}
	}
}

// Chromium resolves the egress proxy's own address through host mapping rules,
// whatever the pin says, so a runtime rule could send every request to another
// proxy.
func TestDropHostMappingFlags(t *testing.T) {
	kept, dropped := DropHostMappingFlags([]string{
		"--kiosk",
		"--host-resolver-rules=MAP 192.0.2.1 198.51.100.7:8080",
		"--host-rules=MAP * 198.51.100.7",
		"-host-resolver-rules=MAP * 198.51.100.7",
		"--host-resolver-rules",
		"--host-resolver-rules-extra=1",
		"--proxy-server=http://198.51.100.7:8080",
	})
	wantKept := []string{"--kiosk", "--host-resolver-rules-extra=1", "--proxy-server=http://198.51.100.7:8080"}
	if !slices.Equal(kept, wantKept) {
		t.Fatalf("kept = %q, want %q", kept, wantKept)
	}
	if len(dropped) != 4 {
		t.Fatalf("dropped = %q, want the four host mapping flags", dropped)
	}
}

// Chromium applies every file in the policy directory, whatever its name, so a
// write that fails before the rename must not leave its temporary file there,
// nor in the staging directory, from where a later rename could move it.
func TestPinSyncFailureLeavesNoTemporaryFile(t *testing.T) {
	p := testPin(t)
	// A directory in the pin's place makes the rename fail after the write.
	if err := os.MkdirAll(filepath.Join(p.Path, "blocker"), 0o755); err != nil {
		t.Fatalf("block pin path: %v", err)
	}
	if err := p.Sync(Policy{Filtered: true}, []string{"--proxy-server=http://192.0.2.1:3129"}); err == nil {
		t.Fatal("Sync succeeded over a directory")
	}
	entries, err := os.ReadDir(filepath.Dir(p.Path))
	if err != nil {
		t.Fatalf("read policy dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(p.Path) {
		t.Fatalf("policy dir holds %v, want only the blocked pin path", entries)
	}
	entries, err = os.ReadDir(p.StageDir)
	if err != nil {
		t.Fatalf("read stage dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(filepath.Dir(p.Path)) {
		t.Fatalf("stage dir holds %v, want only the policy dir", entries)
	}
}

// A filtered session without a proxy to pin would come up with nothing
// stopping an extension from going direct, so the launcher must not start it.
func TestPinSyncRefusesFilteredSessionWithoutProxy(t *testing.T) {
	for _, base := range [][]string{nil, {"--proxy-server="}, {"--proxy-server"}} {
		p := testPin(t)
		if err := p.Sync(Policy{Filtered: true}, base); err == nil {
			t.Fatalf("Sync(filtered, %q) succeeded", base)
		}
		if _, err := os.Stat(p.Path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("Sync(filtered, %q) wrote a pin", base)
		}
	}
}

// Removing an allowlist hands the proxy back to the command line, so an
// extension the customer chose can set it again.
func TestPinSyncRemovesPinWhenUnfiltered(t *testing.T) {
	p := testPin(t)
	if err := p.Sync(Policy{Filtered: true}, []string{"--proxy-server=http://192.0.2.1:3129"}); err != nil {
		t.Fatalf("Sync(filtered): %v", err)
	}
	for range 2 {
		if err := p.Sync(Policy{}, nil); err != nil {
			t.Fatalf("Sync(false): %v", err)
		}
		unpinned, err := p.Matches(Policy{}, nil)
		if err != nil {
			t.Fatalf("Matches: %v", err)
		}
		if !unpinned {
			t.Fatal("pin still present after the session became unfiltered")
		}
	}
}

// The pin is only as strong as its directory: a user who can write there can
// add a policy file that sorts after the pin, or remove it.
func TestPinSyncClosesThePolicyDirectories(t *testing.T) {
	p := testPin(t)
	for _, dir := range []string{p.StageDir, filepath.Dir(p.Path)} {
		if err := os.Chmod(dir, 0o777); err != nil {
			t.Fatalf("open %s: %v", dir, err)
		}
	}
	if err := p.Sync(Policy{Filtered: true}, []string{"--proxy-server=http://192.0.2.1:3129"}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	for _, dir := range []string{p.StageDir, filepath.Dir(p.Path)} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		if perm := info.Mode().Perm(); perm != 0o755 {
			t.Fatalf("%s left with mode %o, want 755", dir, perm)
		}
	}
}

// A file at the pin's path that is not the pin for this policy must not stop
// the egress policy handler from restarting Chromium to write the real one.
// That includes a pin written for other private hosts, which is what a
// restart that failed before the launcher ran leaves behind.
func TestPinMatchesRequiresThePolicysPin(t *testing.T) {
	base := []string{"--proxy-server=http://192.0.2.1:3129"}
	policy := Policy{Filtered: true, PrivateHosts: &[]string{"preview.internal:8443"}}
	p := testPin(t)
	if err := p.Sync(Policy{Filtered: true, PrivateHosts: &[]string{"10.1.0.0/16"}}, base); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	stale, err := os.ReadFile(p.Path)
	if err != nil {
		t.Fatalf("read pin: %v", err)
	}
	for _, content := range []string{
		"",
		"{}",
		string(stale),
		`{"ProxySettings":{"ProxyMode":"direct"},"WebRtcIPHandling":"disable_non_proxied_udp","WebRtcIPHandlingUrl":[],"DnsOverHttpsMode":"off","PolicyListMultipleSourceMergeList":[]}`,
		`{"ProxySettings":{"ProxyMode":"fixed_servers","ProxyServer":"http://192.0.2.1:3129","ProxyBypassList":"preview.internal:8443"},"WebRtcIPHandling":"default","WebRtcIPHandlingUrl":[],"DnsOverHttpsMode":"off","PolicyListMultipleSourceMergeList":[]}`,
		`{"ProxySettings":{"ProxyMode":"fixed_servers","ProxyServer":"http://192.0.2.1:3129","ProxyBypassList":"preview.internal:8443"},"WebRtcIPHandling":"disable_non_proxied_udp","DnsOverHttpsMode":"off","PolicyListMultipleSourceMergeList":[]}`,
		`{"ProxySettings":{"ProxyMode":"fixed_servers","ProxyServer":"http://192.0.2.1:3129","ProxyBypassList":"preview.internal:8443"},"WebRtcIPHandling":"disable_non_proxied_udp","WebRtcIPHandlingUrl":[],"PolicyListMultipleSourceMergeList":[]}`,
		`{"ProxySettings":{"ProxyMode":"fixed_servers","ProxyServer":"http://192.0.2.1:3129","ProxyBypassList":"preview.internal:8443"},"WebRtcIPHandling":"disable_non_proxied_udp","WebRtcIPHandlingUrl":[],"DnsOverHttpsMode":"off"}`,
	} {
		if err := os.WriteFile(p.Path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %q: %v", content, err)
		}
		pinned, err := p.Matches(policy, base)
		if err != nil {
			t.Fatalf("Matches(%q): %v", content, err)
		}
		if pinned {
			t.Fatalf("Matches(%q) accepted it as the pin", content)
		}
	}
	if err := p.Sync(policy, base); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if pinned, err := p.Matches(policy, base); err != nil || !pinned {
		t.Fatalf("Matches after Sync = %t, %v", pinned, err)
	}
	if unpinned, err := p.Matches(Policy{}, base); err != nil || unpinned {
		t.Fatalf("Matches(unfiltered) with a pin in place = %t, %v", unpinned, err)
	}
}

func TestWithDefaultPrivateNetworkBypass(t *testing.T) {
	tests := []struct {
		name  string
		flags []string
		want  []string
	}{
		{
			name: "image default",
			want: []string{DefaultPrivateNetworkBypassFlag},
		},
		{
			name:  "default follows unrelated flags",
			flags: []string{"--kiosk"},
			want:  []string{"--kiosk", DefaultPrivateNetworkBypassFlag},
		},
		{
			name:  "custom list replaces image default",
			flags: []string{"--proxy-bypass-list=preview.internal"},
			want:  []string{"--proxy-bypass-list=preview.internal"},
		},
		{
			name:  "explicit empty list clears image default",
			flags: []string{"--proxy-bypass-list="},
			want:  []string{"--proxy-bypass-list="},
		},
		{
			name:  "bare empty list clears image default",
			flags: []string{"--proxy-bypass-list"},
			want:  []string{"--proxy-bypass-list"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := WithDefaultPrivateNetworkBypass(tt.flags)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("WithDefaultPrivateNetworkBypass() mismatch:\n got: %#v\nwant: %#v", got, tt.want)
			}
		})
	}
}

func TestDefaultPrivateNetworkBypassPreservesRuntimePrecedence(t *testing.T) {
	configured := chromiumflags.MergeFlagsWithRuntimeTokens(
		"--proxy-bypass-list=preview.internal",
		[]string{DefaultPrivateNetworkBypassFlag},
	)
	got := WithDefaultPrivateNetworkBypass(configured)
	want := []string{
		"--proxy-bypass-list=preview.internal",
		DefaultPrivateNetworkBypassFlag,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("runtime precedence changed:\n got: %#v\nwant: %#v", got, want)
	}
}
