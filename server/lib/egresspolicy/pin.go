package egresspolicy

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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
// The pin is derived from the flags Chromium is launched with, so it is written
// by the launcher on every start. Once it is in place Chromium ignores
// --proxy-bypass-list as well as --proxy-server, which is why it carries both.
//
// WebRTC sends UDP straight to the network rather than through an HTTP proxy,
// so a page can reach any STUN or TURN server whatever the proxy says. The pin
// also restricts WebRTC to connections that go through the proxy, and clears
// the per-URL WebRtcIPHandlingUrl rules, which Chromium consults before
// WebRtcIPHandling and which a chrome_policy override could otherwise use to
// turn direct UDP back on.
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
}

type proxySettings struct {
	ProxyMode       string `json:"ProxyMode"`
	ProxyServer     string `json:"ProxyServer"`
	ProxyBypassList string `json:"ProxyBypassList,omitempty"`
}

const (
	pinProxyMode        = "fixed_servers"
	pinWebRtcIPHandling = "disable_non_proxied_udp"
)

// Sync writes the pin when the session is filtered, and removes it when it is
// not. It returns the bypass list entries it left out of the pin.
//
// The proxy server is taken from base, the flags the control plane launches
// the VM with, never from runtime flags: anyone holding the session's token
// can write those, and a runtime --proxy-server would otherwise be pinned in
// place of the egress proxy. The bypass list is taken from final, the flags
// Chromium is about to start with, because the control plane sets a session's
// private hosts at runtime; for the same reason it keeps only entries the
// control plane would accept as private hosts.
//
// The policy directory, and the staging directory the pin is renamed from,
// are made the writer's own and closed to everyone else, so the user Chromium
// runs as cannot add a policy file that sorts after the pin or remove it.
func (p Pin) Sync(filtered bool, base, final []string) ([]string, error) {
	if !filtered {
		return nil, p.Remove()
	}
	server, ok := lastFlagValue(base, "--proxy-server")
	if !ok || server == "" {
		return nil, errors.New("egress is filtered but Chromium's base flags have no --proxy-server to pin")
	}
	rawBypass, _ := lastFlagValue(final, "--proxy-bypass-list")
	bypass, dropped := privateBypassList(rawBypass)
	data, err := json.Marshal(pinPolicy{
		ProxySettings:       proxySettings{ProxyMode: pinProxyMode, ProxyServer: server, ProxyBypassList: bypass},
		WebRtcIPHandling:    pinWebRtcIPHandling,
		WebRtcIPHandlingURL: []json.RawMessage{},
	})
	if err != nil {
		return dropped, fmt.Errorf("encode proxy pin: %w", err)
	}
	for _, dir := range []string{p.StageDir, filepath.Dir(p.Path)} {
		if err := ownDir(dir); err != nil {
			return dropped, err
		}
	}
	tmp := filepath.Join(p.StageDir, filepath.Base(p.Path)+".tmp")
	defer os.Remove(tmp)
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return dropped, fmt.Errorf("write proxy pin: %w", err)
	}
	if err := os.Rename(tmp, p.Path); err != nil {
		return dropped, fmt.Errorf("replace proxy pin: %w", err)
	}
	return dropped, nil
}

// Present reports whether a pin is in place. A file at the pin's path that is
// not a pin does not count, so the caller writes a real one over it.
func (p Pin) Present() (bool, error) {
	data, err := os.ReadFile(p.Path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read proxy pin: %w", err)
	}
	var pinned struct {
		ProxySettings       proxySettings      `json:"ProxySettings"`
		WebRtcIPHandling    string             `json:"WebRtcIPHandling"`
		WebRtcIPHandlingURL *[]json.RawMessage `json:"WebRtcIPHandlingUrl"`
	}
	if json.Unmarshal(data, &pinned) != nil {
		return false, nil
	}
	return pinned.ProxySettings.ProxyMode == pinProxyMode &&
		pinned.ProxySettings.ProxyServer != "" &&
		pinned.WebRtcIPHandling == pinWebRtcIPHandling &&
		pinned.WebRtcIPHandlingURL != nil && len(*pinned.WebRtcIPHandlingURL) == 0, nil
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
