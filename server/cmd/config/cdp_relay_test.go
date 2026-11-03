package config

import (
	"strings"
	"testing"
)

func TestCDPRelaySecretValidationAndLogging(t *testing.T) {
	cfg := &Config{CDPRelayToken: "short"}
	if err := validate(cfg); err == nil || !strings.Contains(err.Error(), "CDP_RELAY_TOKEN") {
		t.Fatalf("expected relay token validation, got %v", err)
	}
	cfg.CDPRelayToken = "0123456789abcdef0123456789abcdef"
	logged := cfg.LogValue().String()
	if strings.Contains(logged, cfg.CDPRelayToken) {
		t.Fatal("relay secret appeared in configuration logging")
	}
	if !strings.Contains(logged, "cdp_relay_enabled=true") {
		t.Fatalf("missing enabled indicator: %s", logged)
	}
}
