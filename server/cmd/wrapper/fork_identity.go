package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/kernel/kernel-images/server/lib/forkidentity"
)

func forkIdentityWaitEnabled() (bool, error) {
	return forkidentity.WaitEnabled()
}

func armForkIdentityWait(enabled bool) {
	if !enabled {
		return
	}
	stopAll("envoy")

	for _, path := range []string{forkidentity.AppliedFile, forkidentity.PayloadFile} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			fatalf("fork identity reset %s: %v", path, err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(forkidentity.ReadyFile), 0o755); err != nil {
		fatalf("fork identity ready dir: %v", err)
	}
	if err := os.WriteFile(forkidentity.ReadyFile, []byte("waiting\n"), 0o644); err != nil {
		fatalf("fork identity ready file: %v", err)
	}
}

func waitForForkIdentityIfEnabled(ctx context.Context, enabled bool) (forkidentity.Payload, time.Time, bool) {
	if !enabled {
		return nil, time.Time{}, true
	}

	logf("fork identity waiting payload=%s", forkidentity.PayloadFile)
	payload, err := waitForForkIdentityPayload(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			logf("fork identity wait canceled")
			return nil, time.Time{}, false
		}
		fatalf("fork identity payload wait: %v", err)
	}
	deadline := time.Now().Add(forkidentity.ApplyTimeout - forkidentity.ApplyResponseMargin)
	if err := applyForkIdentityPayload(payload); err != nil {
		fatalf("fork identity apply: %v", err)
	}
	logf("fork identity environment applied instance=%s", payload.InstanceName())
	return payload, deadline, true
}

func waitForForkIdentityPayload(ctx context.Context) (forkidentity.Payload, error) {
	for {
		payload, err := forkidentity.ReadPayload()
		if err == nil {
			return payload, nil
		}
		if !os.IsNotExist(err) {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, ctx.Err()
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func writeForkIdentityAppliedMarker(instanceName string, allReady bool) error {
	if !allReady {
		return errors.New("services did not become ready")
	}
	return forkidentity.WriteAppliedMarker(instanceName)
}

func applyForkIdentityPayload(payload forkidentity.Payload) error {
	for _, key := range forkidentity.ClearEnvKeys(payload) {
		if err := os.Unsetenv(key); err != nil {
			return err
		}
	}
	for key, value := range forkidentity.Env(payload) {
		if err := os.Setenv(key, value); err != nil {
			return err
		}
	}
	return nil
}
