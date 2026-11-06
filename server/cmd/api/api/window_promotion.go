package api

import (
	"context"
	"log/slog"
	"time"

	"github.com/kernel/kernel-images/server/lib/cdpclient"
	"github.com/kernel/kernel-images/server/lib/logger"
)

const (
	// promoteTimeout bounds a single promotion attempt: a fresh browser can
	// take a few seconds to open its first page target, which
	// SetWindowBoundsMaximized needs to resolve the window.
	promoteTimeout = 30 * time.Second
	// promoteRetryDelay spaces the retries while the first page target is
	// still missing.
	promoteRetryDelay = 500 * time.Millisecond
)

// StartWindowPromotion puts the browser window back into the maximized state
// after every chromium start on the headful image.
//
// The launcher seeds --window-size/--window-position to the X root so an
// un-maximized window restores to a sane, root-sized geometry instead of
// chromium's ~1050x1080 default sliver. Chromium 155 ships the window in
// normal state when --window-size is present — it overrides the maximized
// startup state — and an openbox normal window does not auto-track RANDR
// resizes the way a maximized one does. This promotion runs once per browser
// start (the devtools upstream URL changes on every chromium restart) and
// restores the maximized contract. It is a no-op on headless (Xvfb) images.
//
// SetWindowBoundsMaximized is idempotent and leaves fullscreen windows alone,
// so the promotion is safe on kiosk sessions and on starts that somehow
// already ended up maximized.
func (s *ApiService) StartWindowPromotion() {
	if s.detectDisplayMode(context.Background()) != "xorg" {
		return
	}
	ch, unsubscribe := s.upstreamMgr.Subscribe()
	go s.promoteWindowsLoop(ch, unsubscribe)
}

// promoteWindowsLoop promotes the window on each newly discovered upstream
// URL. The initial URL is read through Current before entering the select so
// an upstream that was already discovered before Subscribe ran is not missed;
// the subscription's latest-wins channel covers every later restart.
func (s *ApiService) promoteWindowsLoop(ch <-chan string, unsubscribe func()) {
	defer unsubscribe()
	seen := ""
	for {
		if url := s.upstreamMgr.Current(); url != "" && url != seen {
			seen = url
			s.promoteWindowMaximized(url)
		}
		select {
		case <-s.lifecycleCtx.Done():
			return
		case url, ok := <-ch:
			if !ok {
				return
			}
			if url == seen {
				continue
			}
			seen = url
			s.promoteWindowMaximized(url)
		}
	}
}

// promoteWindowMaximized dials the upstream and re-asserts the maximized
// window state, retrying within the deadline. A fresh browser can take a
// few seconds to open its first page target (SetWindowBoundsMaximized needs
// one), the devtools endpoint can refuse an early connection while chromium
// is still binding its socket, and a connected client can drop mid-attempt —
// each is retried, redialing once the transport is gone. Past the deadline
// the attempt is logged and abandoned: a window left in normal state still
// covers the desktop (that is what the seeded bounds are for); only the
// exact-root geometry and the RANDR auto-reflow are at stake.
func (s *ApiService) promoteWindowMaximized(url string) {
	log := logger.FromContext(s.lifecycleCtx)
	ctx, cancel := context.WithTimeout(s.lifecycleCtx, promoteTimeout)
	defer cancel()

	var client *cdpclient.Client
	defer func() {
		if client != nil {
			client.Close()
		}
	}()

	for {
		if client == nil {
			c, err := cdpclient.Dial(ctx, url)
			if err != nil {
				if !s.retryWindowPromotion(ctx, url, err, log) {
					return
				}
				continue
			}
			client = c
		}

		err := client.SetWindowBoundsMaximized(ctx)
		if err == nil {
			log.Info("window promoted to maximized state")
			return
		}
		// A dropped transport never recovers on this client; clear it
		// so the next pass redials.
		if client.IsClosed() {
			client.Close()
			client = nil
		}
		if !s.retryWindowPromotion(ctx, url, err, log) {
			return
		}
	}
}

// retryWindowPromotion reports whether another promotion pass is worth
// running, spacing passes with promoteRetryDelay. It returns false once the
// deadline is up or a newer upstream — a chromium restart — has taken over,
// abandoning this promotion; the newer URL gets its own.
func (s *ApiService) retryWindowPromotion(ctx context.Context, url string, cause error, log *slog.Logger) bool {
	if s.upstreamMgr.Current() != url || ctx.Err() != nil {
		log.Warn("window maximize abandoned", "error", cause)
		return false
	}
	select {
	case <-ctx.Done():
		log.Warn("window maximize abandoned", "error", cause)
		return false
	case <-time.After(promoteRetryDelay):
		return true
	}
}
