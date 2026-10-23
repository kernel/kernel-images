package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kernel/kernel-images/server/lib/cdpclient"
)

const snapshotStartPageURL = "https://start.duckduckgo.com/"

func prepareSnapshotStartPage(ctx context.Context, internalPort string) error {
	versionURL := "http://127.0.0.1:" + internalPort + "/json/version"
	devtoolsURL, err := cdpclient.BrowserWebSocketURL(ctx, versionURL)
	if err != nil {
		return err
	}
	navCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	navErr := cdpclient.DispatchStartURLAndWait(navCtx, devtoolsURL, "chrome://newtab/", snapshotStartPageURL)
	cancel()

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
