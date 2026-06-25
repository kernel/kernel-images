package forkidentity

import (
	"sort"
	"strings"
)

var KnownEnvKeys = []string{
	"INSTANCE_NAME",
	"INST_NAME",
	"METRO_NAME",
	"XDS_SERVER",
	"KERNEL_INSTANCE_JWT",
	"METRO_API_URL",
	"KERNEL_METRO_API_BASE_URL",
	"SESSION_INTEL_URL",
	"S2_STREAM",
}

func Env(payload Payload) map[string]string {
	values := map[string]string{}
	for key, value := range payload {
		envKey := payloadKeyToEnvKey(key)
		if envKey == "" || strings.TrimSpace(value) == "" {
			continue
		}
		values[envKey] = strings.TrimSpace(value)
	}
	if payload.InstanceName() != "" {
		values["INSTANCE_NAME"] = payload.InstanceName()
	}
	if values["INST_NAME"] == "" && payload.InstanceName() != "" {
		values["INST_NAME"] = payload.InstanceName()
	}
	if metroAPIURL := MetroAPIURL(payload); metroAPIURL != "" {
		values["KERNEL_METRO_API_BASE_URL"] = metroAPIURL
	}
	return values
}

func ClearEnvKeys(payload Payload) []string {
	keys := map[string]struct{}{}
	for _, key := range KnownEnvKeys {
		keys[key] = struct{}{}
	}
	for key := range payload {
		if envKey := payloadKeyToEnvKey(key); envKey != "" {
			keys[envKey] = struct{}{}
		}
	}
	out := make([]string, 0, len(keys))
	for key := range keys {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func payloadKeyToEnvKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	return strings.ToUpper(key)
}
