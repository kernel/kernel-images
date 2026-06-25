package forkidentity

import (
	"fmt"
	"os"
	"strings"
)

type ExtensionConfig struct {
	InstanceName string `json:"instanceName"`
	MetroAPIURL  string `json:"metroApiUrl"`
}

func WaitEnabled() (bool, error) {
	raw := os.Getenv(WaitEnv)
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "0", "false", "no", "off":
		return false, nil
	case "1", "true", "yes", "on":
		return true, nil
	default:
		return false, fmt.Errorf("%s must be a boolean, got %q", WaitEnv, raw)
	}
}

func MetroAPIURL(payload Payload) string {
	return FirstNonEmpty(payload.Get("kernel_metro_api_base_url"), payload.Get("metro_api_url"), payload.Get("session_intel_url"))
}

func ExtensionAPIURL(payload Payload) string {
	return FirstNonEmpty(payload.Get("session_intel_url"), MetroAPIURL(payload))
}

func ExtensionConfigFromPayload(payload Payload) ExtensionConfig {
	return ExtensionConfig{
		InstanceName: payload.InstanceName(),
		MetroAPIURL:  ExtensionAPIURL(payload),
	}
}

func FirstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
