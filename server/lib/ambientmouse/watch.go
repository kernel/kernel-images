package ambientmouse

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/kernel/kernel-images/server/lib/cdpclient"
)

// UpstreamProvider reports Chromium's current browser-level DevTools URL.
type UpstreamProvider interface {
	Current() string
}

// WatchTargets returns a NavigationWatcher that follows page targets over its
// own browser-level DevTools connection. Target discovery needs no session on
// any page, so the watcher is invisible to pages and to the agent's own CDP
// connections. It reconnects whenever the connection drops, which is how it
// follows a Chromium restart.
func WatchTargets(upstream UpstreamProvider, log *slog.Logger) NavigationWatcher {
	return func(ctx context.Context, onNavigation func()) {
		backoff := 250 * time.Millisecond
		for ctx.Err() == nil {
			if url := upstream.Current(); url != "" {
				err := watchOnce(ctx, url, onNavigation)
				if ctx.Err() != nil {
					return
				}
				log.Warn("ambientmouse: navigation watcher disconnected", "err", err)
			}
			if !sleep(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, 2*time.Second)
		}
	}
}

func watchOnce(ctx context.Context, url string, onNavigation func()) error {
	client, err := cdpclient.DialWithEvents(ctx, url)
	if err != nil {
		return err
	}
	defer client.Close()
	if _, err := client.Send(ctx, "Target.setDiscoverTargets", map[string]any{"discover": true}, ""); err != nil {
		return err
	}
	pages := pageURLs{}
	for {
		select {
		case <-ctx.Done():
			return nil
		case msg, ok := <-client.Events():
			if !ok {
				return errors.New("connection closed")
			}
			if pages.navigated(msg) {
				onNavigation()
			}
		}
	}
}

type targetEventParams struct {
	TargetID   string `json:"targetId"`
	TargetInfo struct {
		TargetID string `json:"targetId"`
		Type     string `json:"type"`
		URL      string `json:"url"`
	} `json:"targetInfo"`
}

// pageURLs tracks the URL of every page target, keyed by target ID.
type pageURLs map[string]string

// navigated reports whether msg is a page target committing a different web
// URL. A fragment-only change is not a navigation: the document stays.
func (p pageURLs) navigated(msg cdpclient.Message) bool {
	var params targetEventParams
	switch msg.Method {
	case "Target.targetCreated", "Target.targetInfoChanged":
		if json.Unmarshal(msg.Params, &params) != nil || params.TargetInfo.Type != "page" {
			return false
		}
	case "Target.targetDestroyed":
		if json.Unmarshal(msg.Params, &params) == nil {
			delete(p, params.TargetID)
		}
		return false
	default:
		return false
	}
	info := params.TargetInfo
	url, _, _ := strings.Cut(info.URL, "#")
	prev := p[info.TargetID]
	p[info.TargetID] = url
	if msg.Method != "Target.targetInfoChanged" || url == prev {
		return false
	}
	return strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://")
}
