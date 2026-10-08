package egresspolicy

import (
	"net/netip"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/net/publicsuffix"
)

// The rules below mirror the control plane's validation of a session's
// private_hosts, which is the only legitimate source of a runtime
// --proxy-bypass-list. Anyone holding the session's token can write runtime
// flags too, and once a bypass list is pinned it routes traffic around the
// egress proxy, so the pin carries only entries the control plane would have
// accepted.

const (
	maxBypassEntries  = 32
	maxBypassEntryLen = 255
)

var (
	bypassEntryRegex = regexp.MustCompile(`^[A-Za-z0-9.*:/\[\]_-]+$`)
	bypassLabelRegex = regexp.MustCompile(`^[A-Za-z0-9_](?:[A-Za-z0-9_-]{0,61}[A-Za-z0-9_])?$`)
)

// privateRanges are the only ranges an IP or CIDR entry may cover: RFC1918,
// CGNAT, and IPv6 ULA.
var privateRanges = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("fc00::/7"),
}

// privateBypassList keeps the entries of a --proxy-bypass-list value that the
// control plane would accept as private hosts, and returns the rest as
// dropped. Dropping an entry sends its traffic through the egress proxy, so a
// rejected entry fails closed.
func privateBypassList(list string) (kept string, dropped []string) {
	var entries []string
	for _, entry := range strings.Split(list, ";") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if len(entries) < maxBypassEntries && validBypassEntry(entry) {
			entries = append(entries, entry)
		} else {
			dropped = append(dropped, entry)
		}
	}
	return strings.Join(entries, ";"), dropped
}

func validBypassEntry(entry string) bool {
	if len(entry) > maxBypassEntryLen || !bypassEntryRegex.MatchString(entry) || strings.Contains(entry, "://") {
		return false
	}
	if strings.Contains(entry, "/") {
		prefix, err := netip.ParsePrefix(entry)
		return err == nil && !prefix.Addr().Is4In6() && prefix == prefix.Masked() && prefixIsPrivate(prefix)
	}
	host, ok := hostWithoutPort(entry)
	if !ok {
		return false
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return !addr.Is4In6() && addrIsPrivate(addr)
	}
	if isNumericIPv4(host) {
		return false
	}
	if wildcards := strings.Count(host, "*"); wildcards > 0 {
		if wildcards != 1 || !strings.HasPrefix(host, "*.") {
			return false
		}
		host = strings.TrimPrefix(host, "*.")
		if strings.Count(host, ".") < 1 || isPublicSuffix(host) {
			return false
		}
	}
	return validHostname(host)
}

func prefixIsPrivate(prefix netip.Prefix) bool {
	for _, r := range privateRanges {
		if r.Bits() <= prefix.Bits() && r.Contains(prefix.Addr()) {
			return true
		}
	}
	return false
}

func addrIsPrivate(addr netip.Addr) bool {
	for _, r := range privateRanges {
		if r.Contains(addr) {
			return true
		}
	}
	return false
}

func hostWithoutPort(entry string) (string, bool) {
	if strings.HasPrefix(entry, "[") {
		end := strings.IndexByte(entry, ']')
		if end < 0 {
			return "", false
		}
		host := entry[1:end]
		if addr, err := netip.ParseAddr(host); err != nil || !addr.Is6() {
			return "", false
		}
		rest := entry[end+1:]
		if rest == "" {
			return host, true
		}
		if !strings.HasPrefix(rest, ":") || strings.ContainsAny(rest[1:], "[]:") {
			return "", false
		}
		return host, validPort(rest[1:])
	}
	if strings.ContainsAny(entry, "[]") {
		return "", false
	}
	if addr, err := netip.ParseAddr(entry); err == nil {
		// Chromium only matches an exact IPv6 address in bracketed form.
		return entry, !addr.Is6() || addr.Is4In6()
	}
	if strings.Count(entry, ":") > 1 {
		return "", false
	}
	if i := strings.LastIndexByte(entry, ':'); i >= 0 {
		return entry[:i], validPort(entry[i+1:])
	}
	return entry, true
}

func validPort(value string) bool {
	port, err := strconv.ParseUint(value, 10, 16)
	return err == nil && port != 0
}

// isNumericIPv4 reports whether Chromium would parse host as an IPv4 address
// rather than a hostname, as it does with forms such as 0x7f.1.
func isNumericIPv4(host string) bool {
	for _, part := range strings.Split(host, ".") {
		if part == "" {
			return false
		}
		digits := "0123456789"
		if strings.HasPrefix(part, "0x") || strings.HasPrefix(part, "0X") {
			part, digits = part[2:], "0123456789abcdefABCDEF"
		}
		for _, c := range part {
			if !strings.ContainsRune(digits, c) {
				return false
			}
		}
	}
	return true
}

func isPublicSuffix(host string) bool {
	host = strings.ToLower(host)
	suffix, _ := publicsuffix.PublicSuffix(host)
	return suffix == host
}

func validHostname(host string) bool {
	if host == "" || strings.HasPrefix(host, ".") || strings.HasSuffix(host, ".") || strings.Contains(host, "..") {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if !bypassLabelRegex.MatchString(label) {
			return false
		}
	}
	return true
}
