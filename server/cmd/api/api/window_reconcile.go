package api

import (
	"context"
	"log/slog"
	"time"

	"github.com/kernel/kernel-images/server/lib/cdpclient"
	"github.com/kernel/kernel-images/server/lib/logger"
)

const (
	// windowReconcileInterval spaces reconcile passes. A pass is two small
	// local CDP round trips, and the interval trades that cost against how long
	// a demoted window can stay demoted — short enough that someone watching
	// the live view is unlikely to catch one.
	windowReconcileInterval = 5 * time.Second
	// windowReconcileTimeout bounds a single pass, so a stalled chromium cannot
	// delay the next tick.
	windowReconcileTimeout = 3 * time.Second
)

// StartWindowReconcile keeps a headful window maximized for the life of the
// session.
//
// Promotion on start restores the maximized contract across a chromium restart.
// This covers everything that can demote the window while the session runs:
// a CDP command over the proxy, a CDP command issued inside the playwright
// executor, a window manager restore, or a geometry a saved profile carries
// into a restart. None of those are distinguishable from the window state
// itself, so reasserting the state is what covers all of them — a client that
// shrinks the window gets a window that fills the desktop again on the next
// pass.
//
// It is a no-op on the headless image, which has no window manager.
func (s *ApiService) StartWindowReconcile() {
	if s.detectDisplayMode(context.Background()) != "xorg" {
		return
	}
	go s.reconcileWindowLoop()
}

// reconcileWindowLoop reconciles on a ticker, holding one devtools connection
// for the current upstream and redialing when chromium restarts or the
// transport drops.
func (s *ApiService) reconcileWindowLoop() {
	log := logger.FromContext(s.lifecycleCtx)
	ticker := time.NewTicker(windowReconcileInterval)
	defer ticker.Stop()

	var client *cdpclient.Client
	clientURL := ""
	defer func() {
		if client != nil {
			client.Close()
		}
	}()

	for {
		select {
		case <-s.lifecycleCtx.Done():
			return
		case <-ticker.C:
		}

		url := s.upstreamMgr.Current()
		if url == "" {
			continue
		}
		if client != nil && (clientURL != url || client.IsClosed()) {
			client.Close()
			client = nil
		}
		if client == nil {
			c, err := s.dialWindowReconcile(url)
			if err != nil {
				continue
			}
			client, clientURL = c, url
		}

		ctx, cancel := context.WithTimeout(s.lifecycleCtx, windowReconcileTimeout)
		reconcileWindow(ctx, client, log)
		cancel()
	}
}

// dialWindowReconcile dials the devtools upstream for one reconcile
// connection.
func (s *ApiService) dialWindowReconcile(url string) (*cdpclient.Client, error) {
	ctx, cancel := context.WithTimeout(s.lifecycleCtx, windowReconcileTimeout)
	defer cancel()
	return cdpclient.Dial(ctx, url)
}

// reconcileWindow restores the maximized state when the window has been taken
// out of it and reports whether it acted.
//
// The line is emitted only on a pass that acts, which paces itself: the next
// pass sees a maximized window unless someone demoted it again, so one line
// means one demotion, and its timestamp can be matched against the client's own
// call timeline to attribute it.
//
// A maximized window and a fullscreen one are both left alone, so a kiosk
// session is unaffected.
func reconcileWindow(ctx context.Context, client *cdpclient.Client, log *slog.Logger) bool {
	bounds, err := client.GetWindowBounds(ctx)
	if err != nil {
		return false
	}
	if bounds.WindowState == "maximized" || bounds.WindowState == "fullscreen" {
		return false
	}
	log.Info("window was not maximized, restoring",
		"window_id", bounds.WindowID,
		"window_state", bounds.WindowState,
		"width", bounds.Width,
		"height", bounds.Height,
	)
	if err := client.SetWindowBoundsMaximized(ctx); err != nil {
		log.Warn("window restore failed", "error", err)
		return false
	}
	return true
}
