package scaletozero

import (
	"context"
	"os"
	"sync"

	"github.com/onkernel/kernel-images/server/lib/logger"
)

// Unikraft scale-to-zero control file
// https://unikraft.cloud/docs/api/v1/instances/#scaletozero_app
const unikraftScaleToZeroFile = "/uk/libukp/scale_to_zero_disable"

type Controller interface {
	// Disable turns scale-to-zero off.
	Disable(ctx context.Context) error
	// Enable re-enables scale-to-zero after it has previously been disabled.
	Enable(ctx context.Context) error
	// Drain resets the active count to zero and re-enables scale-to-zero.
	// After Drain is called the controller is frozen: subsequent Disable and
	// Enable calls become no-ops. The frozen state is in-memory only and is
	// cleared on process restart. This is intended for graceful shutdown /
	// restart scenarios where we want to guarantee scale-to-zero stays enabled.
	Drain(ctx context.Context) error
}

type unikraftCloudController struct {
	path    string
	mu      sync.Mutex
	drained bool
}

func NewUnikraftCloudController() Controller {
	return &unikraftCloudController{path: unikraftScaleToZeroFile, drained: false}
}

func (c *unikraftCloudController) Disable(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.drained {
		return nil
	}
	return c.write(ctx, "+")
}

func (c *unikraftCloudController) Enable(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.drained {
		return nil
	}
	return c.write(ctx, "-")
}

func (c *unikraftCloudController) Drain(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.drained {
		err := c.write(ctx, "=0")
		if err != nil {
			return err
		}
	}
	c.drained = true
	return nil
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
func (NoopController) Drain(context.Context) error   { return nil }

// Oncer wraps a Controller and ensures that Disable and Enable are called at most once.
type Oncer struct {
	ctrl        Controller
	disableOnce sync.Once
	enableOnce  sync.Once
	drainedOnce sync.Once
	disableErr  error
	enableErr   error
	drainedErr  error
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

func (o *Oncer) Drain(ctx context.Context) error {
	o.drainedOnce.Do(func() { o.drainedErr = o.ctrl.Drain(ctx) })
	return o.drainedErr
}

type DebouncedController struct {
	ctrl        Controller
	mu          sync.Mutex
	disabled    bool
	drained     bool
	activeCount int
}

func NewDebouncedController(ctrl Controller) Controller {
	return &DebouncedController{ctrl: ctrl}
}

func (c *DebouncedController) Disable(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.drained {
		return nil
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

	if c.drained {
		return nil
	}

	if c.activeCount > 0 {
		c.activeCount--
	}

	// nothing to do
	if c.activeCount > 0 || !c.disabled {
		return nil
	}

	if err := c.ctrl.Enable(ctx); err != nil {
		return err
	}

	c.disabled = false
	return nil
}

func (c *DebouncedController) Drain(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.drained = true
	c.activeCount = 0

	if !c.disabled {
		return nil
	}

	if err := c.ctrl.Drain(ctx); err != nil {
		return err
	}

	c.disabled = false
	return nil
}
