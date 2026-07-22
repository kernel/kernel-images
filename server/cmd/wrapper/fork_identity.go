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

func waitForForkIdentityIfEnabled(ctx context.Context, enabled bool) bool {
	if !enabled {
		return true
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

	logf("fork identity waiting payload=%s", forkidentity.PayloadFile)
	payload, err := waitForForkIdentityPayload(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			logf("fork identity wait canceled")
			return false
		}
		fatalf("fork identity payload wait: %v", err)
	}
	if err := applyForkIdentityPayload(payload); err != nil {
		fatalf("fork identity apply: %v", err)
	}
	if err := forkidentity.WriteAppliedMarker(payload.InstanceName()); err != nil {
		fatalf("fork identity applied file: %v", err)
	}
	logf("fork identity applied instance=%s", payload.InstanceName())
	return true
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
