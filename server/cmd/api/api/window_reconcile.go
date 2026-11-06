package api

import (
	"context"
	"log/slog"
	"time"

	"github.com/kernel/kernel-images/server/lib/cdpclient"
	"github.com/kernel/kernel-images/server/lib/logger"
	"github.com/kernel/kernel-images/server/lib/x11"
)

const (
	// windowCorrectionBudget caps how many times one browser start puts the
	// window back. One correction covers the case this guard exists for: a
	// window that leaves maximized once, through a saved geometry surfacing
	// after a restart or a stray resize, and stays there for the rest of the
	// session. Repeated demotions are a client that wants a window this size,
	// and past the budget it keeps it: a guard that always wins turns into a
	// fight the client cannot see or stop.
	windowCorrectionBudget = 3
	// windowCorrectionTimeout bounds one correction, so a stalled chromium
	// cannot wedge the watch.
	windowCorrectionTimeout = 3 * time.Second
	// windowProbeTimeout bounds the lookup of the window to watch.
	windowProbeTimeout = 2 * time.Second
	// windowWatchRetryDelay spaces retries while no window is mapped yet.
	windowWatchRetryDelay = time.Second
)

// StartWindowReconcile keeps a headful window covering the display after the
// promotion that runs on browser start.
//
// Promotion restores the maximized contract once per chromium start. It cannot
// cover what happens next: a CDP Browser.setWindowBounds over the proxy, the
// same command inside the playwright executor, a window manager restore, or a
// geometry a saved profile carries into the restart and wins with after the
// promotion has already run. None of those are distinguishable from the window
// state itself, so the guard watches the state.
//
// It is a no-op on the headless image, which has no window manager.
func (s *ApiService) StartWindowReconcile() {
	if s.detectDisplayMode(context.Background()) != "xorg" {
		return
	}
	ch, unsubscribe := s.upstreamMgr.Subscribe()
	go s.reconcileWindowLoop(ch, unsubscribe)
}

// reconcileWindowLoop runs one guard per browser start, replacing it when
// chromium restarts under a new devtools upstream. The initial URL is read
// through Current before entering the select so an upstream discovered before
// Subscribe ran is not missed.
func (s *ApiService) reconcileWindowLoop(ch <-chan string, unsubscribe func()) {
	defer unsubscribe()

	var cancel context.CancelFunc
	defer func() {
		if cancel != nil {
			cancel()
		}
	}()

	seen := ""
	guard := func(url string) {
		if cancel != nil {
			cancel()
		}
		ctx, c := context.WithCancel(s.lifecycleCtx)
		cancel = c
		go s.guardWindowCoverage(ctx, url)
	}

	for {
		if url := s.upstreamMgr.Current(); url != "" && url != seen {
			seen = url
			guard(url)
		}
		select {
		case <-s.lifecycleCtx.Done():
			return
		case url, ok := <-ch:
			if !ok {
				return
			}
			if url == "" || url == seen {
				continue
			}
			seen = url
			guard(url)
		}
	}
}

// guardWindowCoverage watches the browser window for this chromium start and
// restores it when it stops covering the display, until the correction budget
// is spent. The watch ends with the window: xprop exits when it is destroyed,
// and the next browser start brings its own guard and its own budget.
func (s *ApiService) guardWindowCoverage(ctx context.Context, url string) {
	log := logger.FromContext(s.lifecycleCtx)
	display := s.resolveDisplayFromEnv()
	g := &windowGuard{url: url, budget: windowCorrectionBudget, log: log}
	defer g.close()

	for ctx.Err() == nil {
		windowID, ok := x11.ActiveWindowID(display, windowProbeTimeout)
		if !ok {
			if !sleepUntil(ctx, windowWatchRetryDelay) {
				return
			}
			continue
		}

		watchCtx, stop := context.WithCancel(ctx)
		err := x11.WatchWindowCoverage(watchCtx, display, windowID, func(covered bool) {
			if !g.onCoverage(watchCtx, covered) {
				stop()
			}
		})
		stop()
		if g.spent() {
			return
		}
		if err != nil {
			log.Warn("window state watch ended", "error", err)
		}
		if !sleepUntil(ctx, windowWatchRetryDelay) {
			return
		}
	}
}

// windowGuard holds the state one browser start's guard carries between
// changes: the devtools upstream to correct through, the connection it reuses,
// and what is left of the correction budget.
type windowGuard struct {
	url    string
	budget int
	log    *slog.Logger
	client *cdpclient.Client
}

// onCoverage handles one reported window state and reports whether the guard
// keeps watching. A window that covers the display is left alone and costs
// nothing. One that does not is read over CDP before anything is changed,
// which is what decides a correction: the X atoms say the window is no longer
// maximized, the CDP state says whether it is something the guard should leave
// alone, like the fullscreen window of a kiosk session.
func (g *windowGuard) onCoverage(ctx context.Context, covered bool) bool {
	if covered || g.spent() {
		return !g.spent()
	}

	ctx, cancel := context.WithTimeout(ctx, windowCorrectionTimeout)
	defer cancel()

	client, err := g.connect(ctx)
	if err != nil {
		g.log.Warn("window restore could not reach the browser", "error", err)
		return true
	}
	if !reconcileWindow(ctx, client, g.log) {
		return true
	}

	g.budget--
	if g.spent() {
		g.log.Info("window correction budget spent, leaving the window as the client sets it")
		return false
	}
	return true
}

func (g *windowGuard) spent() bool { return g.budget <= 0 }

// connect reuses the guard's devtools connection, redialing once the transport
// is gone. A dropped connection never recovers on the same client.
func (g *windowGuard) connect(ctx context.Context) (*cdpclient.Client, error) {
	if g.client != nil && g.client.IsClosed() {
		g.client.Close()
		g.client = nil
	}
	if g.client != nil {
		return g.client, nil
	}
	client, err := cdpclient.Dial(ctx, g.url)
	if err != nil {
		return nil, err
	}
	g.client = client
	return client, nil
}

func (g *windowGuard) close() {
	if g.client != nil {
		g.client.Close()
		g.client = nil
	}
}

// reconcileWindow restores the maximized state when the window has been taken
// out of it and reports whether it acted.
//
// The line is emitted only on a pass that acts, so one line means one
// demotion, and its timestamp can be matched against the client's own call
// timeline to attribute it — including for a command that comes through the
// playwright executor, which the devtools proxy does not see.
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

// sleepUntil waits for d and reports whether the wait finished rather than the
// context ending.
func sleepUntil(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
