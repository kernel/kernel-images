package api

import (
	"context"
	"regexp"

	"github.com/kernel/kernel-images/server/lib/cdpclient"
	"github.com/kernel/kernel-images/server/lib/logger"
)

// executorNamePattern constrains Playwright and Browser REPL executor names.
var executorNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// executorTabs opens and closes the tabs named executors own. The API owns
// every executor tab, so a tab's ID is recorded before any executor process
// can use it and DELETE or shutdown can always find it.
type executorTabs interface {
	Open(ctx context.Context) (string, error)
	Close(ctx context.Context, targetID string) error
}

// cdpExecutorTabs manages executor tabs over the browser's DevTools
// connection.
type cdpExecutorTabs struct {
	withCDP func(context.Context, func(context.Context, *cdpclient.Client) error) error
}

// Open creates a background tab in the default browser context, so it does
// not take focus from the live view or change the active tab that the default
// executors bind to.
func (t cdpExecutorTabs) Open(ctx context.Context) (string, error) {
	var targetID string
	err := t.withCDP(ctx, func(ctx context.Context, c *cdpclient.Client) error {
		var err error
		targetID, err = c.CreateTarget(ctx, "about:blank", true)
		return err
	})
	return targetID, err
}

func (t cdpExecutorTabs) Close(ctx context.Context, targetID string) error {
	return t.withCDP(ctx, func(ctx context.Context, c *cdpclient.Client) error {
		return c.CloseTarget(ctx, targetID)
	})
}

// tabURLs returns the current URL of every target, keyed by target ID, or nil
// when the browser cannot report them.
func (s *ApiService) tabURLs(ctx context.Context) map[string]string {
	var urls map[string]string
	err := s.withCDPClient(ctx, func(ctx context.Context, c *cdpclient.Client) error {
		var err error
		urls, err = c.TargetURLs(ctx)
		return err
	})
	if err != nil {
		logger.FromContext(ctx).Warn("failed to read executor tab URLs", "error", err)
	}
	return urls
}
