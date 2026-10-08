package egresspolicy

import (
	"slices"
	"testing"
)

// Runtime flags can be written by anyone holding the session's token, so the
// pin keeps only the bypass entries the control plane accepts as private hosts.
func TestPrivateBypassList(t *testing.T) {
	accepted := []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.1.0/24",
		"100.64.0.0/10",
		"fc00::/7",
		"fd00::/8",
		"10.1.2.3",
		"10.1.2.3:8080",
		"[fd00::1]",
		"[fd00::1]:8443",
		"preview.internal",
		"preview.internal:8443",
		"*.corp.example.com",
		"under_score.internal",
	}
	rejected := []string{
		"*",
		"<local>",
		"<-loopback>",
		"*.com",
		"*.co.uk",
		"*.internal",
		"*example.com",
		"a.*.example.com",
		"8.8.8.8",
		"8.8.8.0/24",
		"0.0.0.0/0",
		"10.0.0.1/8",
		"127.0.0.1",
		"169.254.169.254",
		"fd00::1",
		"::ffff:10.0.0.1",
		"0x0a.1.2.3",
		"http://preview.internal",
		"preview.internal:0",
		"preview.internal:65536",
		"-bad.internal",
		"bad..internal",
	}
	for _, entry := range accepted {
		if kept, dropped := privateBypassList(entry); kept != entry || len(dropped) != 0 {
			t.Errorf("privateBypassList(%q) = %q, %q; want it kept", entry, kept, dropped)
		}
	}
	for _, entry := range rejected {
		if kept, dropped := privateBypassList(entry); kept != "" || !slices.Equal(dropped, []string{entry}) {
			t.Errorf("privateBypassList(%q) = %q, %q; want it dropped", entry, kept, dropped)
		}
	}
}

func TestPrivateBypassListCapsEntries(t *testing.T) {
	var list string
	for i := range maxBypassEntries + 1 {
		if i > 0 {
			list += ";"
		}
		list += "10.0.0." + string(rune('0'+i%10))
	}
	_, dropped := privateBypassList(list)
	if len(dropped) != 1 {
		t.Fatalf("dropped %d entries, want 1 past the cap of %d", len(dropped), maxBypassEntries)
	}
}
