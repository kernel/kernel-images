package scaletozero

import (
	"context"
	"os"
	"sync"
	"time"

	"github.com/kernel/kernel-images/server/lib/logger"
)

// Unikraft scale-to-zero control file
// https://unikraft.cloud/docs/api/v1/instances/#scaletozero_app
const unikraftScaleToZeroFile = "/uk/libukp/scale_to_zero_disable"

type Controller interface {
	// Disable turns scale-to-zero off.
	Disable(ctx context.Context) error
	// Enable re-enables scale-to-zero after it has previously been disabled.
	Enable(ctx context.Context) error
}

// PinnedController extends Controller with an out-of-band "pin" that holds
// scale-to-zero disabled independently of the request-driven refcount used by
// the HTTP middleware. While the pin is held, request-driven Enable calls do
// not re-enable scale-to-zero; only Unpin can release it.
//
// This is intended for explicit lifecycle control (e.g. a control-plane API
// reserving a VM in a hot pool) where the holder is not tied to an inflight
// HTTP request.
type PinnedController interface {
	Controller
	// Pin holds scale-to-zero disabled until Unpin is called. The pin is a
	// boolean, not a counter: repeated calls are idempotent.
	Pin(ctx context.Context) error
	// Unpin releases the pin. If no request-driven holders remain,
	// scale-to-zero is re-enabled (honoring any configured cooldown).
	Unpin(ctx context.Context) error
}

type unikraftCloudController struct {
	path string
}

func NewUnikraftCloudController() Controller {
	return &unikraftCloudController{path: unikraftScaleToZeroFile}
}

func (c *unikraftCloudController) Disable(ctx context.Context) error {
	return c.write(ctx, "+")
}

func (c *unikraftCloudController) Enable(ctx context.Context) error {
	return c.write(ctx, "-")
}

func (c *unikraftCloudController) write(ctx context.Context, char string) error {
	if _, err := os.Stat(c.path); err != nil {
		if os.IsNotExist(err) {
			logger.FromContext(ctx).Info("scale-to-zero control file not found, skipping write", "path", c.path, "value", char)
			return nil
		}
		logger.FromContext(ctx).Error("failed to stat scale-to-zero control file", "path", c.path, "err", err)
		return err
	}

	f, err := os.OpenFile(c.path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		logger.FromContext(ctx).Error("failed to open scale-to-zero control file", "path", c.path, "err", err)
		return err
	}
	defer f.Close()
	if _, err := f.Write([]byte(char)); err != nil {
		logger.FromContext(ctx).Error("failed to write scale-to-zero control file", "path", c.path, "err", err)
		return err
	}
	logger.FromContext(ctx).Info("scale-to-zero control file written", "path", c.path, "value", char)
	return nil
}

type NoopController struct{}

func NewNoopController() *NoopController { return &NoopController{} }

func (NoopController) Disable(context.Context) error { return nil }
func (NoopController) Enable(context.Context) error  { return nil }
func (NoopController) Pin(context.Context) error     { return nil }
func (NoopController) Unpin(context.Context) error   { return nil }

// Oncer wraps a Controller and ensures that Disable and Enable are called at most once.
type Oncer struct {
	ctrl        Controller
	disableOnce sync.Once
	enableOnce  sync.Once
	disableErr  error
	enableErr   error
}

func NewOncer(c Controller) *Oncer { return &Oncer{ctrl: c} }

func (o *Oncer) Disable(ctx context.Context) error {
	o.disableOnce.Do(func() { o.disableErr = o.ctrl.Disable(ctx) })
	return o.disableErr
}

func (o *Oncer) Enable(ctx context.Context) error {
	o.enableOnce.Do(func() { o.enableErr = o.ctrl.Enable(ctx) })
	return o.enableErr
}

type DebouncedController struct {
	ctrl          Controller
	cooldown      time.Duration
	mu            sync.Mutex
	disabled      bool
	activeCount   int
	pinned        bool
	reenableTimer *time.Timer
}

// NewDebouncedController creates a DebouncedController with no re-enable cooldown.
func NewDebouncedController(ctrl Controller) *DebouncedController {
	return &DebouncedController{ctrl: ctrl}
}

// NewDebouncedControllerWithCooldown creates a DebouncedController that delays
// re-enabling scale-to-zero by the given cooldown after the last active holder
// releases. A new Disable call during the cooldown cancels the pending
// re-enable, avoiding rapid toggling from sequential requests.
func NewDebouncedControllerWithCooldown(ctrl Controller, cooldown time.Duration) *DebouncedController {
	return &DebouncedController{ctrl: ctrl, cooldown: cooldown}
}

func (c *DebouncedController) Disable(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.reenableTimer != nil {
		c.reenableTimer.Stop()
		c.reenableTimer = nil
	}

	c.activeCount++
	if c.disabled {
		return nil
	}

	if err := c.ctrl.Disable(ctx); err != nil {
		c.activeCount--
		return err
	}

	c.disabled = true
	return nil
}

func (c *DebouncedController) Enable(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.activeCount > 0 {
		c.activeCount--
	}

	return c.maybeReenableLocked(ctx)
}

// Pin sets the out-of-band pin and ensures scale-to-zero is disabled.
// Idempotent: re-pinning while already pinned is a no-op. Cancels any pending
// cooldown timer.
func (c *DebouncedController) Pin(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.reenableTimer != nil {
		c.reenableTimer.Stop()
		c.reenableTimer = nil
	}

	if c.pinned {
		return nil
	}

	if !c.disabled {
		if err := c.ctrl.Disable(ctx); err != nil {
			return err
		}
		c.disabled = true
	}

	c.pinned = true
	return nil
}

// Unpin releases the pin. If no request-driven holders remain, scale-to-zero
// is re-enabled (honoring any configured cooldown). Idempotent: calling when no
// pin is held is a no-op.
func (c *DebouncedController) Unpin(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.pinned {
		return nil
	}
	c.pinned = false

	if err := c.maybeReenableLocked(ctx); err != nil {
		// Restore the pin so a retry can re-attempt the underlying Enable;
		// otherwise the caller has no API-driven recovery path.
		c.pinned = true
		return err
	}
	return nil
}

// maybeReenableLocked re-enables scale-to-zero if no holders (request-driven or
// pin) remain. Caller must hold c.mu.
func (c *DebouncedController) maybeReenableLocked(ctx context.Context) error {
	if c.activeCount > 0 || c.pinned || !c.disabled {
		return nil
	}

	// No cooldown: re-enable immediately (original behavior).
	if c.cooldown <= 0 {
		if err := c.ctrl.Enable(ctx); err != nil {
			return err
		}
		c.disabled = false
		return nil
	}

	// Schedule re-enable after cooldown. If a new Disable arrives before the
	// timer fires, it will be cancelled.
	c.reenableTimer = time.AfterFunc(c.cooldown, func() {
		c.mu.Lock()
		defer c.mu.Unlock()

		if c.activeCount > 0 || c.pinned || !c.disabled {
			return
		}

		if c.ctrl.Enable(context.Background()) == nil {
			c.disabled = false
		}
	})

	return nil
}
