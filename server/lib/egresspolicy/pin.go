package egresspolicy

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultPinPath is where the proxy pin is written. Chromium merges every file
// in its managed policy directory, so the pin sits beside policy.json rather
// than inside it and never races the writers of that file. Where two files set
// the same policy, the one that sorts last wins, so the name keeps the pin
// ahead of policy.json.
const DefaultPinPath = "/etc/chromium/policies/managed/zz-kernel-egress.json"

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
// also restricts WebRTC to connections that go through the proxy.
type Pin struct {
	Path string
}

type pinPolicy struct {
	ProxySettings    proxySettings `json:"ProxySettings"`
	WebRtcIPHandling string        `json:"WebRtcIPHandling"`
}

type proxySettings struct {
	ProxyMode       string `json:"ProxyMode"`
	ProxyServer     string `json:"ProxyServer"`
	ProxyBypassList string `json:"ProxyBypassList,omitempty"`
}

// Sync writes the pin for the given Chromium flags when the session is
// filtered, and removes it when it is not.
func (p Pin) Sync(filtered bool, flags []string) error {
	if !filtered {
		return p.Remove()
	}
	server, ok := lastFlagValue(flags, "--proxy-server")
	if !ok || server == "" {
		return errors.New("egress is filtered but Chromium has no --proxy-server to pin")
	}
	bypass, _ := lastFlagValue(flags, "--proxy-bypass-list")
	data, err := json.Marshal(pinPolicy{
		ProxySettings:    proxySettings{ProxyMode: "fixed_servers", ProxyServer: server, ProxyBypassList: bypass},
		WebRtcIPHandling: "disable_non_proxied_udp",
	})
	if err != nil {
		return fmt.Errorf("encode proxy pin: %w", err)
	}
	// Staged outside the policy directory: Chromium reads every file in it,
	// whatever the name, so a temporary file left there would pin the proxy
	// where Present and Remove cannot see it.
	tmp := filepath.Join(filepath.Dir(filepath.Dir(p.Path)), filepath.Base(p.Path)+".tmp")
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write proxy pin: %w", err)
	}
	if err := os.Rename(tmp, p.Path); err != nil {
		return fmt.Errorf("replace proxy pin: %w", err)
	}
	return nil
}

// Present reports whether the pin is in place.
func (p Pin) Present() (bool, error) {
	_, err := os.Stat(p.Path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat proxy pin: %w", err)
	}
	return true, nil
}

// Remove deletes the pin. Chromium reloads its policy directory on its own, so
// the proxy reverts to the command line without a restart.
func (p Pin) Remove() error {
	if err := os.Remove(p.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove proxy pin: %w", err)
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
