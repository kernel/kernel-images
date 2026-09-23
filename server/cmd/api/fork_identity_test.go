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

	"github.com/kernel/kernel-images/server/cmd/config"
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
	forkIdentityHandler(slog.Default(), nil).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusConflict, rec.Code)
}

func TestForkIdentityHandlerRejectsBadPayload(t *testing.T) {
	useTempForkIdentityFiles(t)
	t.Setenv(forkidentity.WaitEnv, "true")

	req := httptest.NewRequest(http.MethodPost, "/internal/fork-identity", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	forkIdentityHandler(slog.Default(), nil).ServeHTTP(rec, req)

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
	forkIdentityHandler(slog.Default(), nil).ServeHTTP(rec, req)

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
	applied := make(chan forkidentity.Payload, 1)
	done := make(chan struct{})
	go func() {
		forkIdentityHandler(slog.Default(), func(p forkidentity.Payload) { applied <- p }).ServeHTTP(rec, req)
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

	// The callback is what lets this process retarget the sinks that label
	// events with the instance identity.
	select {
	case p := <-applied:
		assert.Equal(t, "browser-1", p.InstanceName())
	default:
		t.Fatal("applied callback did not run")
	}
}

func TestForkIdentityHandlerSkipsCallbackWhenNotApplied(t *testing.T) {
	useTempForkIdentityFiles(t)
	t.Setenv(forkidentity.WaitEnv, "true")

	req := httptest.NewRequest(http.MethodPost, "/internal/fork-identity", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	called := false
	forkIdentityHandler(slog.Default(), func(forkidentity.Payload) { called = true }).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.False(t, called)
}

func useTempForkIdentityFiles(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	oldPayloadFile := forkidentity.PayloadFile
	oldAppliedFile := forkidentity.AppliedFile
	oldReadyFile := forkidentity.ReadyFile
	forkidentity.PayloadFile = filepath.Join(dir, "fork-identity.json")
	forkidentity.AppliedFile = filepath.Join(dir, "fork-identity-applied")
	forkidentity.ReadyFile = filepath.Join(dir, "fork-identity-ready")
	t.Cleanup(func() {
		forkidentity.PayloadFile = oldPayloadFile
		forkidentity.AppliedFile = oldAppliedFile
		forkidentity.ReadyFile = oldReadyFile
	})
}

func writeForkIdentityPayloadForTest(t *testing.T, payload forkidentity.Payload) {
	t.Helper()
	data, err := json.Marshal(payload)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(forkidentity.PayloadFile, data, 0o600))
}

func TestAppliedS2StreamPrefersForkIdentity(t *testing.T) {
	useTempForkIdentityFiles(t)
	markForkIdentityWaitArmed(t)
	cfg := &config.Config{S2Stream: "seed-stream"}

	// Nothing applied yet: the env this process started with belongs to the
	// parent, so there is nothing this instance may bind.
	stream, pending := appliedS2Stream(cfg)
	assert.True(t, pending)
	assert.Empty(t, stream)

	// A payload written but not yet applied is not the instance's identity.
	writeForkIdentityPayloadForTest(t, forkidentity.Payload{
		"instance_name":     "fork",
		"session_intel_url": "https://intel.example.test",
		"s2_stream":         "fork-stream",
	})
	stream, pending = appliedS2Stream(cfg)
	assert.True(t, pending)
	assert.Empty(t, stream)

	// Once applied it survives a restart of this process, which is the point.
	require.NoError(t, forkidentity.WriteAppliedMarker("fork"))
	stream, pending = appliedS2Stream(cfg)
	assert.False(t, pending)
	assert.Equal(t, "fork-stream", stream)
}

func TestAppliedS2StreamNeverFallsBackToSeedOnFork(t *testing.T) {
	useTempForkIdentityFiles(t)
	markForkIdentityWaitArmed(t)
	cfg := &config.Config{S2Stream: "seed-stream"}

	writeForkIdentityPayloadForTest(t, forkidentity.Payload{
		"instance_name":     "fork",
		"session_intel_url": "https://intel.example.test",
	})
	require.NoError(t, forkidentity.WriteAppliedMarker("fork"))

	stream, pending := appliedS2Stream(cfg)
	assert.False(t, pending)
	assert.Empty(t, stream, "an applied fork identity without a stream must not bind the parent's")
}

func TestAppliedS2StreamIgnoresMarkerForAnotherInstance(t *testing.T) {
	useTempForkIdentityFiles(t)
	markForkIdentityWaitArmed(t)
	cfg := &config.Config{S2Stream: "seed-stream"}

	writeForkIdentityPayloadForTest(t, forkidentity.Payload{
		"instance_name":     "fork",
		"session_intel_url": "https://intel.example.test",
		"s2_stream":         "fork-stream",
	})
	require.NoError(t, forkidentity.WriteAppliedMarker("someone-else"))

	stream, pending := appliedS2Stream(cfg)
	assert.True(t, pending)
	assert.Empty(t, stream)
}

func TestAppliedS2StreamIgnoresMarkerFromBeforeThisBoot(t *testing.T) {
	useTempForkIdentityFiles(t)
	cfg := &config.Config{S2Stream: "seed-stream"}

	// Applied files with no ready file: the wrapper has not entered the wait in
	// this boot yet, so it is about to clear these and hold for a new handoff.
	writeForkIdentityPayloadForTest(t, forkidentity.Payload{
		"instance_name":     "stale-fork",
		"session_intel_url": "https://intel.example.test",
		"s2_stream":         "stale-stream",
	})
	require.NoError(t, forkidentity.WriteAppliedMarker("stale-fork"))

	stream, pending := appliedS2Stream(cfg)
	assert.False(t, pending)
	assert.Equal(t, "seed-stream", stream)
}

func TestS2StreamResolverUsesHookPayloadWithoutReadyFile(t *testing.T) {
	useTempForkIdentityFiles(t)
	resolver := newS2StreamResolver(&config.Config{S2Stream: "seed-stream"}, slog.Default())

	resolver.RecordAppliedPayload(forkidentity.Payload{"s2_stream": "fork-stream"})

	assert.Equal(t, "fork-stream", resolver.Resolve())
}

// A telemetry config applied to a fork before its identity must not bind the
// parent's stream: the sink stays closed until the handoff has happened.
func TestS2StreamResolverHoldsBackSeedStreamWhilePending(t *testing.T) {
	useTempForkIdentityFiles(t)
	markForkIdentityWaitArmed(t)
	resolver := newS2StreamResolver(&config.Config{S2Stream: "seed-stream"}, slog.Default())

	assert.Empty(t, resolver.Resolve())

	resolver.RecordAppliedPayload(forkidentity.Payload{"instance_name": "fork", "session_intel_url": "https://intel.example.test"})
	assert.Empty(t, resolver.Resolve(), "a fork payload without a stream must not fall back to the parent's")

	resolver.RecordAppliedPayload(forkidentity.Payload{"s2_stream": "fork-stream"})
	assert.Equal(t, "fork-stream", resolver.Resolve())
}

// markForkIdentityWaitArmed stands in for the wrapper having entered the wait,
// which is what makes an applied identity current for this boot.
func markForkIdentityWaitArmed(t *testing.T) {
	t.Helper()
	require.NoError(t, os.WriteFile(forkidentity.ReadyFile, []byte("waiting\n"), 0o644))
}
