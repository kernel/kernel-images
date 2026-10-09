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

// DefaultPin sits beside policy.json rather than inside it, so it never races
// that file's writers. Chromium merges every file in the directory and the one
// that sorts last wins, so the name keeps the pin ahead of policy.json.
var DefaultPin = Pin{
	Path:     "/etc/chromium/policies/managed/zz-kernel-egress.json",
	StageDir: "/etc/chromium/policies",
}

// Pin fixes Chromium's network settings with managed policy while the session
// is filtered. Chromium applies proxy settings in the order policy, extensions,
// command line, so without it an extension can switch --proxy-server to direct.
//
// Besides ProxySettings, the pin restricts WebRTC to proxied connections, since
// WebRTC sends UDP around an HTTP proxy, and turns DNS-over-HTTPS off, since
// Chromium connects to a DoH server directly. It empties WebRtcIPHandlingUrl,
// which Chromium consults before WebRtcIPHandling, and
// PolicyListMultipleSourceMergeList, which would otherwise merge another file's
// WebRtcIPHandlingUrl rules into the pin's.
type Pin struct {
	// Path is the pin's file in Chromium's managed policy directory.
	Path string
	// StageDir is where the pin is written before it is renamed into place.
	// Chromium applies every file in the policy directory whatever its name,
	// so this must be outside it, on the same filesystem.
	StageDir string
}

type pinPolicy struct {
	ProxySettings       proxySettings     `json:"ProxySettings"`
	WebRtcIPHandling    string            `json:"WebRtcIPHandling"`
	WebRtcIPHandlingURL []json.RawMessage `json:"WebRtcIPHandlingUrl"`
	DnsOverHttpsMode    string            `json:"DnsOverHttpsMode"`
	ListMergeList       []string          `json:"PolicyListMultipleSourceMergeList"`
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
// not. base is the flags from BaseFlags.
//
// Nothing in the pin comes from runtime flags, which anyone holding the
// session's token can write. The proxy server comes from base, and the bypass
// list from the policy's private hosts, or from base when the policy has none.
//
// Both directories are made root-owned and closed to others, so the user
// Chromium runs as cannot add a policy file that sorts after the pin.
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
	server := lastFlagValue(base, "--proxy-server")
	if server == "" {
		return nil, errors.New("egress is filtered but Chromium's base flags have no --proxy-server to pin")
	}
	var bypass string
	if policy.PrivateHosts != nil {
		bypass = strings.Join(*policy.PrivateHosts, ";")
	} else {
		bypass = lastFlagValue(base, "--proxy-bypass-list")
	}
	data, err := json.Marshal(pinPolicy{
		ProxySettings:       proxySettings{ProxyMode: pinProxyMode, ProxyServer: server, ProxyBypassList: bypass},
		WebRtcIPHandling:    pinWebRtcIPHandling,
		WebRtcIPHandlingURL: []json.RawMessage{},
		DnsOverHttpsMode:    pinDnsOverHttpsMode,
		ListMergeList:       []string{},
	})
	if err != nil {
		return nil, fmt.Errorf("encode proxy pin: %w", err)
	}
	return append(data, '\n'), nil
}

// writeSynced writes data to path and flushes it, so a crash after the rename
// cannot leave a torn pin.
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

// hostMappingFlags can remap the egress proxy's own address, which Chromium
// honors whatever the pin says.
var hostMappingFlags = []string{"host-resolver-rules", "host-rules"}

// DropHostMappingFlags splits runtime flags into the ones to keep and the
// host-mapping ones to drop while filtered. Chromium accepts "-name" as well
// as "--name", so any dash prefix matches.
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

// Matches reports whether the pin on disk is exactly what Sync writes for
// policy and base, or absent for an unfiltered policy.
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

// Remove deletes the pin. Chromium picks the removal up without a restart.
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

// DefaultPrivateNetworkBypassFlag is the image's bypass list, used when the
// control plane sets none.
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

// BaseFlags returns CHROMIUM_FLAGS, without runtime flags, plus the image's
// default bypass list. The launcher writes the pin from these and the handler
// checks it against them, so both must call this.
func BaseFlags(chromiumFlags string) []string {
	return WithDefaultPrivateNetworkBypass(chromiumflags.MergeFlagsWithRuntimeTokens(chromiumFlags, nil))
}

// lastFlagValue returns the value of name's last occurrence, which is the one
// Chromium uses.
func lastFlagValue(flags []string, name string) string {
	var value string
	for _, flag := range flags {
		switch {
		case flag == name:
			value = ""
		case strings.HasPrefix(flag, name+"="):
			value = strings.TrimPrefix(flag, name+"=")
		}
	}
	return value
}
