package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/kernel/kernel-images/server/lib/cdpclient"
)

const (
	snapshotStartPageURL = "https://start.duckduckgo.com/"
	envoyBootstrapPath   = "/etc/envoy/bootstrap.yaml"
)

func prepareSnapshotStartPage(ctx context.Context, internalPort string) (retErr error) {
	seedEnvoyStarted := false
	seedEnvoyCleaned := false
	defer func() {
		if !seedEnvoyCleaned {
			retErr = errors.Join(retErr, cleanupSeedEnvoyIfStarted(seedEnvoyStarted))
		}
	}()

	if envoyEnabled() {
		if !isExecutable("/usr/local/bin/init-envoy.sh") {
			return fmt.Errorf("envoy is enabled but init-envoy.sh is unavailable")
		}
		if err := runStream("envoy-init", "/usr/local/bin/init-envoy.sh"); err != nil {
			return fmt.Errorf("start seed envoy: %w", err)
		}
		seedEnvoyStarted = true
		if err := waitForTCP(ctx, "127.0.0.1", "3128", 25*time.Second); err != nil {
			return err
		}
	}

	versionURL := "http://127.0.0.1:" + internalPort + "/json/version"
	devtoolsURL, err := cdpclient.BrowserWebSocketURL(ctx, versionURL)
	if err != nil {
		return err
	}
	navCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	navErr := cdpclient.DispatchStartURLAndWait(navCtx, devtoolsURL, "chrome://newtab/", snapshotStartPageURL)
	cancel()

	cleanupErr := cleanupSeedEnvoyIfStarted(seedEnvoyStarted)
	seedEnvoyCleaned = true
	if cleanupErr != nil {
		return cleanupErr
	}
	if navErr == nil {
		logf("snapshot start page ready url=%s", snapshotStartPageURL)
		return nil
	}
	if errors.Is(navErr, context.Canceled) {
		return navErr
	}

	logf("WARNING: snapshot start page unavailable, using about:blank: %v", navErr)
	blankCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := cdpclient.DispatchStartURL(blankCtx, devtoolsURL, "about:blank"); err != nil {
		return fmt.Errorf("reset snapshot start page: %w", err)
	}
	return nil
}

func cleanupSeedEnvoyIfStarted(started bool) error {
	if !started {
		return nil
	}
	return cleanupSeedEnvoy()
}

func cleanupSeedEnvoy() error {
	return cleanupSeedEnvoyWith(
		envoyBootstrapPath,
		func() { stopAll("envoy") },
		func() bool { return tcpOK("127.0.0.1", "3128") },
	)
}

func cleanupSeedEnvoyWith(bootstrapPath string, stop func(), running func() bool) error {
	stop()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !running() {
			if err := os.Remove(bootstrapPath); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("remove seed envoy bootstrap: %w", err)
			}
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("seed envoy did not stop")
}

func waitForTCP(ctx context.Context, host, port string, timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if tcpOK(host, port) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return fmt.Errorf("%s:%s did not become ready after %s", host, port, timeout)
		case <-ticker.C:
		}
	}
}
