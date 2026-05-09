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

// Toggler is the low-level scale-to-zero control. Implementations directly
// flip the underlying state (e.g. write to the unikraft control file).
type Toggler interface {
	// Disable turns scale-to-zero off.
	Disable(ctx context.Context) error
	// Enable turns scale-to-zero on.
	Enable(ctx context.Context) error
}

// Controller is the high-level scale-to-zero control used by the rest of
// the server. It supports two independent holder mechanisms:
//
//  1. Refcounted holds via Acquire/Release. Multiple callers (HTTP
//     middleware, ffmpeg recorder, ...) may hold scale-to-zero off
//     concurrently; scale-to-zero re-enables only when the last hold is
//     released. Use the pair as Acquire(ctx); defer Release(ctx).
//
//  2. An idempotent persistent override via Disable/Enable. Disable puts
//     scale-to-zero in the off state and keeps it there until Enable is
//     called, independent of any refcounted holds. Both calls are
//     idempotent.
type Controller interface {
	// Acquire holds scale-to-zero disabled (refcounted). Pair with Release.
	Acquire(ctx context.Context) error
	// Release releases one refcounted hold. If no holds and no idempotent
	// disable remain, scale-to-zero re-enables (honoring any cooldown).
	Release(ctx context.Context) error
	// Disable idempotently puts scale-to-zero in the off state. The state
	// persists until Enable is called, even if all refcounted holds are
	// released. Repeated calls are no-ops.
	Disable(ctx context.Context) error
	// Enable releases the idempotent disable. If no refcounted holds remain,
	// scale-to-zero re-enables (honoring any cooldown). Calling without a
	// prior Disable is a no-op.
	Enable(ctx context.Context) error
}

type unikraftCloudToggler struct {
	path string
}

// NewUnikraftCloudToggler returns a Toggler that flips scale-to-zero by
// writing to the unikraft control file.
func NewUnikraftCloudToggler() Toggler {
	return &unikraftCloudToggler{path: unikraftScaleToZeroFile}
}

func (c *unikraftCloudToggler) Disable(ctx context.Context) error {
	return c.write(ctx, "+")
}

func (c *unikraftCloudToggler) Enable(ctx context.Context) error {
	return c.write(ctx, "-")
}

func (c *unikraftCloudToggler) write(ctx context.Context, char string) error {
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

func (NoopController) Acquire(context.Context) error { return nil }
func (NoopController) Release(context.Context) error { return nil }
func (NoopController) Disable(context.Context) error { return nil }
func (NoopController) Enable(context.Context) error  { return nil }

// Oncer wraps a Controller and ensures Acquire and Release fire at most once.
type Oncer struct {
	ctrl        Controller
	acquireOnce sync.Once
	releaseOnce sync.Once
	acquireErr  error
	releaseErr  error
}

func NewOncer(c Controller) *Oncer { return &Oncer{ctrl: c} }

func (o *Oncer) Acquire(ctx context.Context) error {
	o.acquireOnce.Do(func() { o.acquireErr = o.ctrl.Acquire(ctx) })
	return o.acquireErr
}

func (o *Oncer) Release(ctx context.Context) error {
	o.releaseOnce.Do(func() { o.releaseErr = o.ctrl.Release(ctx) })
	return o.releaseErr
}

// DebouncedController implements Controller by tracking both a refcount of
// active Acquire holders and a boolean idempotent-disable flag, debounced by
// an optional cooldown before scale-to-zero is re-enabled.
type DebouncedController struct {
	toggler       Toggler
	cooldown      time.Duration
	mu            sync.Mutex
	off           bool
	holdCount     int
	disabled      bool
	reenableTimer *time.Timer
}

// NewDebouncedController creates a DebouncedController with no re-enable cooldown.
func NewDebouncedController(t Toggler) *DebouncedController {
	return &DebouncedController{toggler: t}
}

// NewDebouncedControllerWithCooldown creates a DebouncedController that delays
// re-enabling scale-to-zero by the given cooldown after the last holder
// releases. A new Acquire call during the cooldown cancels the pending
// re-enable, avoiding rapid toggling from sequential requests.
func NewDebouncedControllerWithCooldown(t Toggler, cooldown time.Duration) *DebouncedController {
	return &DebouncedController{toggler: t, cooldown: cooldown}
}

func (c *DebouncedController) Acquire(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.reenableTimer != nil {
		c.reenableTimer.Stop()
		c.reenableTimer = nil
	}

	c.holdCount++
	if c.off {
		return nil
	}

	if err := c.toggler.Disable(ctx); err != nil {
		c.holdCount--
		return err
	}

	c.off = true
	return nil
}

func (c *DebouncedController) Release(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.holdCount > 0 {
		c.holdCount--
	}

	return c.maybeReenableLocked(ctx)
}

// Disable idempotently puts scale-to-zero in the off state. Cancels any
// pending cooldown timer. Repeated calls while already disabled are no-ops.
func (c *DebouncedController) Disable(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.reenableTimer != nil {
		c.reenableTimer.Stop()
		c.reenableTimer = nil
	}

	if c.disabled {
		return nil
	}

	if !c.off {
		if err := c.toggler.Disable(ctx); err != nil {
			return err
		}
		c.off = true
	}

	c.disabled = true
	return nil
}

// Enable releases the idempotent disable. If no refcounted holds remain,
// scale-to-zero is re-enabled (honoring any configured cooldown). Calling
// without a prior Disable is a no-op.
func (c *DebouncedController) Enable(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.disabled {
		return nil
	}
	c.disabled = false

	return c.maybeReenableLocked(ctx)
}

// maybeReenableLocked re-enables scale-to-zero if no holders (refcount or
// idempotent disable) remain. Caller must hold c.mu.
func (c *DebouncedController) maybeReenableLocked(ctx context.Context) error {
	if c.holdCount > 0 || c.disabled || !c.off {
		return nil
	}

	// No cooldown: re-enable immediately.
	if c.cooldown <= 0 {
		if err := c.toggler.Enable(ctx); err != nil {
			return err
		}
		c.off = false
		return nil
	}

	// Schedule re-enable after cooldown. If a new Acquire or Disable arrives
	// before the timer fires, it will be cancelled.
	c.reenableTimer = time.AfterFunc(c.cooldown, func() {
		c.mu.Lock()
		defer c.mu.Unlock()

		if c.holdCount > 0 || c.disabled || !c.off {
			return
		}

		if c.toggler.Enable(context.Background()) == nil {
			c.off = false
		}
	})

	return nil
}
