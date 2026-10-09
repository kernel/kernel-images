package egresspolicy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/kernel/kernel-images/server/lib/chromiumflags"
)

// DefaultPin is the pin as the image lays it out. Chromium merges every file
// in its managed policy directory, so the pin sits beside policy.json rather
// than inside it and never races the writers of that file. Where two files set
// the same policy, the one that sorts last wins, so the name keeps the pin
// ahead of policy.json.
var DefaultPin = Pin{
	Path:     "/etc/chromium/policies/managed/zz-kernel-egress.json",
	StageDir: "/etc/chromium/policies",
}

// Pin fixes Chromium's proxy with managed policy while the session is filtered.
//
// Chromium is pointed at the egress proxy with --proxy-server, but it applies
// proxy settings in the order policy, extensions, command line. An extension
// with the proxy permission can therefore switch it to direct connections, and
// a CDP client can load such an extension with Extensions.loadUnpacked. Policy
// is the one source an extension cannot override.
//
// The pin is derived from the base flags Chromium is launched with and the
// policy, and is written by the launcher on every start. Once it is in place
// Chromium ignores --proxy-bypass-list as well as --proxy-server, which is why
// it carries both.
//
// WebRTC sends UDP straight to the network rather than through an HTTP proxy,
// so a page can reach any STUN or TURN server whatever the proxy says. The pin
// also restricts WebRTC to connections that go through the proxy, and clears
// the per-URL WebRtcIPHandlingUrl rules, which Chromium consults before
// WebRtcIPHandling and which a chrome_policy override could otherwise use to
// turn direct UDP back on.
//
// Chromium connects to a DNS-over-HTTPS server directly rather than through
// the proxy, whenever it resolves a name itself, as it does for a bypassed
// hostname. The pin turns DNS-over-HTTPS off, so a chrome_policy override
// cannot name a server for it to connect to.
type Pin struct {
	// Path is the pin's file in Chromium's managed policy directory.
	Path string
	// StageDir is where the pin is written before it is renamed into place.
	// It must be outside the policy directory, since Chromium applies every
	// file there whatever its name, and on the same filesystem.
	StageDir string
}

type pinPolicy struct {
	ProxySettings       proxySettings     `json:"ProxySettings"`
	WebRtcIPHandling    string            `json:"WebRtcIPHandling"`
	WebRtcIPHandlingURL []json.RawMessage `json:"WebRtcIPHandlingUrl"`
	DnsOverHttpsMode    string            `json:"DnsOverHttpsMode"`
}

type proxySettings struct {
	ProxyMode       string `json:"ProxyMode"`
	ProxyServer     string `json:"ProxyServer"`
	ProxyBypassList string `json:"ProxyBypassList,omitempty"`
}

const (
	pinProxyMode        = "fixed_servers"
	pinWebRtcIPHandling = "disable_non_proxied_udp"
	pinDnsOverHttpsMode = "off"
)

// Sync writes the pin when the session is filtered, and removes it when it is
// not. base is the flags the control plane launches the VM with, before any
// runtime flags are merged in.
//
// Nothing in the pin comes from runtime flags: anyone holding the session's
// token can write those, and a pinned runtime --proxy-server or
// --proxy-bypass-list would route traffic around the egress proxy. The proxy
// server is the base flags' own. The bypass list is the policy's private hosts,
// which only the control plane can set, or the base flags' bypass list when the
// control plane set none.
//
// The policy directory, and the staging directory the pin is renamed from,
// are made the writer's own and closed to everyone else, so the user Chromium
// runs as cannot add a policy file that sorts after the pin or remove it.
func (p Pin) Sync(policy Policy, base []string) error {
	if !policy.Filtered {
		return p.Remove()
	}
	data, err := encodePin(policy, base)
	if err != nil {
		return err
	}
	for _, dir := range []string{p.StageDir, filepath.Dir(p.Path)} {
		if err := ownDir(dir); err != nil {
			return err
		}
	}
	tmp := filepath.Join(p.StageDir, filepath.Base(p.Path)+".tmp")
	defer os.Remove(tmp)
	if err := writeSynced(tmp, data); err != nil {
		return fmt.Errorf("write proxy pin: %w", err)
	}
	if err := os.Rename(tmp, p.Path); err != nil {
		return fmt.Errorf("replace proxy pin: %w", err)
	}
	return nil
}

// encodePin returns the pin Sync writes for a filtered policy.
func encodePin(policy Policy, base []string) ([]byte, error) {
	server, ok := lastFlagValue(base, "--proxy-server")
	if !ok || server == "" {
		return nil, errors.New("egress is filtered but Chromium's base flags have no --proxy-server to pin")
	}
	var bypass string
	if policy.PrivateHosts != nil {
		bypass = strings.Join(*policy.PrivateHosts, ";")
	} else {
		bypass, _ = lastFlagValue(base, "--proxy-bypass-list")
	}
	data, err := json.Marshal(pinPolicy{
		ProxySettings:       proxySettings{ProxyMode: pinProxyMode, ProxyServer: server, ProxyBypassList: bypass},
		WebRtcIPHandling:    pinWebRtcIPHandling,
		WebRtcIPHandlingURL: []json.RawMessage{},
		DnsOverHttpsMode:    pinDnsOverHttpsMode,
	})
	if err != nil {
		return nil, fmt.Errorf("encode proxy pin: %w", err)
	}
	return append(data, '\n'), nil
}

// writeSynced writes data to path and flushes it to disk, so a crash after the
// rename that follows cannot leave a torn pin in the policy directory.
func writeSynced(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// hostMappingFlags rewrite the address Chromium connects to for a host,
// including the egress proxy's own address, so a runtime rule could send every
// request to a different proxy. Chromium honors them whatever the pin says.
//
// This is not every switch that touches the network. The token that writes
// runtime flags can also run processes in the VM, which egress without
// Chromium, so the list closes the routes found through Chromium's own
// configuration rather than defending against that token.
var hostMappingFlags = []string{"host-resolver-rules", "host-rules"}

// DropHostMappingFlags returns runtime flags without the ones that remap
// hosts, and the flags it removed. The launcher applies it while the session
// is filtered. Switches are matched by name whatever their dash prefix,
// since Chromium accepts "-name" as well as "--name".
func DropHostMappingFlags(runtime []string) (kept, dropped []string) {
	for _, flag := range runtime {
		name, _, _ := strings.Cut(strings.TrimLeft(flag, "-"), "=")
		if slices.Contains(hostMappingFlags, name) {
			dropped = append(dropped, flag)
			continue
		}
		kept = append(kept, flag)
	}
	return kept, dropped
}

// Matches reports whether the pin in place is exactly the one Sync writes for
// policy and base: none at all for an unfiltered policy. Anything else at the
// pin's path, including a pin written for other private hosts, does not match,
// so the caller has the real one written over it.
func (p Pin) Matches(policy Policy, base []string) (bool, error) {
	data, err := os.ReadFile(p.Path)
	if errors.Is(err, os.ErrNotExist) {
		return !policy.Filtered, nil
	}
	if err != nil {
		return false, fmt.Errorf("read proxy pin: %w", err)
	}
	if !policy.Filtered {
		return false, nil
	}
	want, err := encodePin(policy, base)
	if err != nil {
		return false, err
	}
	return bytes.Equal(data, want), nil
}

// Remove deletes the pin. Chromium reloads its policy directory on its own, so
// the proxy reverts to the command line without a restart.
func (p Pin) Remove() error {
	if err := os.Remove(p.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove proxy pin: %w", err)
	}
	return nil
}

// ownDir makes dir owned by the current user and writable by no one else.
func ownDir(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("stat %s: %w", dir, err)
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Geteuid() {
		if err := os.Chown(dir, os.Geteuid(), os.Getegid()); err != nil {
			return fmt.Errorf("take ownership of %s: %w", dir, err)
		}
	}
	if info.Mode().Perm()&0o022 != 0 {
		if err := os.Chmod(dir, info.Mode().Perm()&^0o022); err != nil {
			return fmt.Errorf("restrict %s: %w", dir, err)
		}
	}
	return nil
}

// DefaultPrivateNetworkBypassFlag is the image's bypass list, which sends
// private ranges around the proxy when the control plane sets no list of its
// own.
const DefaultPrivateNetworkBypassFlag = "--proxy-bypass-list=10.0.0.0/8;172.16.0.0/12;192.168.0.0/16;100.64.0.0/10;fc00::/7"

// WithDefaultPrivateNetworkBypass appends the image's bypass list unless flags
// already carry one.
func WithDefaultPrivateNetworkBypass(flags []string) []string {
	for _, flag := range flags {
		if flag == "--proxy-bypass-list" || strings.HasPrefix(flag, "--proxy-bypass-list=") {
			return flags
		}
	}
	return append(flags, DefaultPrivateNetworkBypassFlag)
}

// BaseFlags returns the flags the pin is derived from: chromiumFlags, the
// CHROMIUM_FLAGS the control plane launches the VM with, before any runtime
// flags, plus the image's default bypass list. The launcher writes the pin from
// them and the egress policy handler checks it against them, so both must call
// this.
func BaseFlags(chromiumFlags string) []string {
	return WithDefaultPrivateNetworkBypass(chromiumflags.MergeFlagsWithRuntimeTokens(chromiumFlags, nil))
}

// lastFlagValue returns the value of the last occurrence of name, which is the
// one Chromium uses. A bare flag has an empty value.
func lastFlagValue(flags []string, name string) (string, bool) {
	value, found := "", false
	for _, flag := range flags {
		switch {
		case flag == name:
			value, found = "", true
		case strings.HasPrefix(flag, name+"="):
			value, found = strings.TrimPrefix(flag, name+"="), true
		}
	}
	return value, found
}
