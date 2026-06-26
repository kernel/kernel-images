package forkidentity

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const MaxPayloadBytes = 64 * 1024

type Payload map[string]string

func (p Payload) Get(key string) string {
	return strings.TrimSpace(p[key])
}

func (p Payload) InstanceName() string {
	return p.Get("instance_name")
}

func (p Payload) Validate() error {
	if p.InstanceName() == "" {
		return fmt.Errorf("instance_name is required")
	}
	if ExtensionAPIURL(p) == "" {
		return fmt.Errorf("one of session_intel_url, kernel_metro_api_base_url, or metro_api_url is required")
	}
	return nil
}

func ReadPayload() (Payload, error) {
	data, err := os.ReadFile(PayloadFile)
	if err != nil {
		return nil, err
	}
	var payload Payload
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("decode payload: %w", err)
	}
	return payload, payload.Validate()
}

func WritePayload(payload Payload) error {
	if err := payload.Validate(); err != nil {
		return err
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(PayloadFile), 0o755); err != nil {
		return err
	}
	tmp := PayloadFile + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, PayloadFile)
}

func WriteAppliedMarker(instanceName string) error {
	if err := os.MkdirAll(filepath.Dir(AppliedFile), 0o755); err != nil {
		return err
	}
	return os.WriteFile(AppliedFile, []byte(strings.TrimSpace(instanceName)+"\n"), 0o644)
}

func ReadAppliedMarker() (string, error) {
	data, err := os.ReadFile(AppliedFile)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func AppliedMarkerMatches(instanceName string) (bool, error) {
	applied, err := ReadAppliedMarker()
	if err != nil {
		return false, err
	}
	return applied == strings.TrimSpace(instanceName), nil
}

func WaitAppliedMarker(instanceName string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		matches, err := AppliedMarkerMatches(instanceName)
		if err != nil {
			return err
		}
		if matches {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for applied marker")
}
