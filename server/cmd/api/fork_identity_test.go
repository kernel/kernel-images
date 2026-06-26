package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kernel/kernel-images/server/lib/forkidentity"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestForkIdentityConfigHandlerReturnsNotFoundWithoutPayload(t *testing.T) {
	useTempForkIdentityFiles(t)

	req := httptest.NewRequest(http.MethodGet, "/internal/fork-identity/config", nil)
	rec := httptest.NewRecorder()
	forkIdentityConfigHandler(slog.Default()).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestForkIdentityConfigHandlerReturnsAcceptedWhileWaiting(t *testing.T) {
	useTempForkIdentityFiles(t)
	t.Setenv(forkidentity.WaitEnv, "true")

	req := httptest.NewRequest(http.MethodGet, "/internal/fork-identity/config", nil)
	rec := httptest.NewRecorder()
	forkIdentityConfigHandler(slog.Default()).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusAccepted, rec.Code)
}

func TestForkIdentityConfigHandlerReturnsExtensionConfig(t *testing.T) {
	useTempForkIdentityFiles(t)
	payload := forkidentity.Payload{
		"instance_name":              "browser-1",
		"metro_api_url":              "https://metro.example.test/browser/kernel",
		"kernel_metro_api_base_url":  "https://kernel-metro.example.test/browser/kernel",
		"session_intel_url":          "https://intel.example.test",
		"future_identity_field_name": "future-value",
	}
	writeForkIdentityPayloadForTest(t, payload)

	req := httptest.NewRequest(http.MethodGet, "/internal/fork-identity/config", nil)
	rec := httptest.NewRecorder()
	forkIdentityConfigHandler(slog.Default()).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var got forkidentity.ExtensionConfig
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, forkidentity.ExtensionConfig{
		InstanceName: "browser-1",
		MetroAPIURL:  "https://intel.example.test",
	}, got)
}

func TestForkIdentityConfigHandlerReturnsAcceptedUntilPayloadApplied(t *testing.T) {
	useTempForkIdentityFiles(t)
	t.Setenv(forkidentity.WaitEnv, "true")
	payload := forkidentity.Payload{
		"instance_name":     "browser-1",
		"session_intel_url": "https://intel.example.test",
	}
	writeForkIdentityPayloadForTest(t, payload)

	req := httptest.NewRequest(http.MethodGet, "/internal/fork-identity/config", nil)
	rec := httptest.NewRecorder()
	forkIdentityConfigHandler(slog.Default()).ServeHTTP(rec, req)
	require.Equal(t, http.StatusAccepted, rec.Code)

	require.NoError(t, forkidentity.WriteAppliedMarker("browser-1"))
	rec = httptest.NewRecorder()
	forkIdentityConfigHandler(slog.Default()).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var got forkidentity.ExtensionConfig
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, forkidentity.ExtensionConfig{
		InstanceName: "browser-1",
		MetroAPIURL:  "https://intel.example.test",
	}, got)
}

func TestForkIdentityHandlerReturnsConflictWhenDisabled(t *testing.T) {
	useTempForkIdentityFiles(t)

	req := httptest.NewRequest(http.MethodPost, "/internal/fork-identity", strings.NewReader(`{
		"instance_name": "browser-1",
		"session_intel_url": "https://intel.example.test"
	}`))
	rec := httptest.NewRecorder()
	forkIdentityHandler(slog.Default()).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusConflict, rec.Code)
}

func TestForkIdentityHandlerRejectsBadPayload(t *testing.T) {
	useTempForkIdentityFiles(t)
	t.Setenv(forkidentity.WaitEnv, "true")

	req := httptest.NewRequest(http.MethodPost, "/internal/fork-identity", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	forkIdentityHandler(slog.Default()).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestForkIdentityHandlerRejectsAfterApplied(t *testing.T) {
	useTempForkIdentityFiles(t)
	t.Setenv(forkidentity.WaitEnv, "true")
	require.NoError(t, forkidentity.WriteAppliedMarker("browser-1"))

	req := httptest.NewRequest(http.MethodPost, "/internal/fork-identity", strings.NewReader(`{
		"instance_name": "browser-2",
		"session_intel_url": "https://intel.example.test"
	}`))
	rec := httptest.NewRecorder()
	forkIdentityHandler(slog.Default()).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusConflict, rec.Code)
}

func TestForkIdentityHandlerWritesPayloadAndWaitsForAppliedMarker(t *testing.T) {
	useTempForkIdentityFiles(t)
	t.Setenv(forkidentity.WaitEnv, "true")

	req := httptest.NewRequest(http.MethodPost, "/internal/fork-identity", strings.NewReader(`{
		"instance_name": "browser-1",
		"session_intel_url": "https://intel.example.test"
	}`))
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		forkIdentityHandler(slog.Default()).ServeHTTP(rec, req)
		close(done)
	}()

	require.Eventually(t, func() bool {
		_, err := os.Stat(forkidentity.PayloadFile)
		return err == nil
	}, time.Second, 10*time.Millisecond)
	payload, err := forkidentity.ReadPayload()
	require.NoError(t, err)
	assert.Equal(t, "browser-1", payload.InstanceName())

	require.NoError(t, forkidentity.WriteAppliedMarker("browser-1"))
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler did not return after applied marker")
	}
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func useTempForkIdentityFiles(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	oldPayloadFile := forkidentity.PayloadFile
	oldAppliedFile := forkidentity.AppliedFile
	forkidentity.PayloadFile = filepath.Join(dir, "fork-identity.json")
	forkidentity.AppliedFile = filepath.Join(dir, "fork-identity-applied")
	t.Cleanup(func() {
		forkidentity.PayloadFile = oldPayloadFile
		forkidentity.AppliedFile = oldAppliedFile
	})
}

func writeForkIdentityPayloadForTest(t *testing.T, payload forkidentity.Payload) {
	t.Helper()
	data, err := json.Marshal(payload)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(forkidentity.PayloadFile, data, 0o600))
}
