// Package webauthnhook answers pages' navigator.credentials.get/create
// publicKey requests with passkeys held outside the VM.
//
// While the hook is on it keeps its own DevTools connection to Chromium,
// separate from telemetry's. Over it, a script installed in every page and
// iframe document replaces the WebAuthn entry points and sends each request
// through a CDP binding. The hook adds the calling document's origin as
// Chromium reports it, forwards the request to a Signer (metro-api), and
// replies in the same execution context. Requests nothing answers fall back
// to the browser's own implementation. Turning the hook off closes the
// connection, which removes its bindings and scripts.
package webauthnhook

import (
	"context"
	_ "embed"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
)

// bindingName must match BINDING and replyFunction must match REPLY in hook.js.
const (
	bindingName   = "__kernelPasskey"
	replyFunction = "__kernelPasskeyReply"
)

var (
	// ErrNotConfigured means the hook cannot be turned on because the VM has
	// no metro API to answer it.
	ErrNotConfigured = errors.New("WebAuthn hook is not configured: KERNEL_METRO_API_BASE_URL is not set")
	// ErrNotInstalled means the hook is on, but the documents already open
	// were not all hooked within installWait. New documents still get it.
	ErrNotInstalled = errors.New("WebAuthn hook is on, but the documents already open were not all hooked in time")
)

//go:embed hook.js
var hookSource string

type Config struct {
	// StatePath keeps the enabled flag across restarts of this process.
	StatePath string
	Signer    Signer
	Logger    *slog.Logger
}

// Upstream reports Chromium's DevTools URL and its changes.
// *devtoolsproxy.UpstreamManager implements it.
type Upstream interface {
	Current() string
	Subscribe() (<-chan string, func())
}

type Hook struct {
	cfg       Config
	upstream  Upstream
	log       *slog.Logger
	lifecycle context.Context
	enabled   atomic.Bool

	setMu   sync.Mutex         // serializes Stop and Set
	stopRun context.CancelFunc // setMu; non-nil while the connection loop runs
	runDone chan struct{}      // setMu

	mu   sync.Mutex
	conn *connection // started connection, nil while there is none
}

// New restores the flag a previous run of the process left, connecting to
// Chromium if it is on. Connections live until ctx ends, Stop, or the hook is
// turned off.
func New(ctx context.Context, cfg Config, upstream Upstream) *Hook {
	h := &Hook{cfg: cfg, upstream: upstream, log: cfg.Logger, lifecycle: ctx}
	if loadEnabled(cfg.StatePath, cfg.Logger) {
		h.enabled.Store(true)
		h.startLocked()
	}
	return h
}

// Stop closes the connection without changing the flag.
func (h *Hook) Stop() {
	h.setMu.Lock()
	defer h.setMu.Unlock()
	h.stopLocked()
}

// Set turns the hook on or off, persisting the flag first. Turning it on
// fails with ErrNotConfigured when the signer is not configured. Otherwise it
// connects and waits, within ctx and installWait, until the documents already
// open are hooked, returning ErrNotInstalled if they are not. Turning it off
// tells hooked documents to leave requests to the browser, then closes the
// connection.
func (h *Hook) Set(ctx context.Context, enabled bool) error {
	h.setMu.Lock()
	defer h.setMu.Unlock()
	if enabled && !h.cfg.Signer.Configured() {
		return ErrNotConfigured
	}
	if err := saveEnabled(h.cfg.StatePath, enabled); err != nil {
		return err
	}
	h.enabled.Store(enabled)
	if enabled {
		h.startLocked()
		return h.waitInstalled(ctx)
	}
	if conn := h.current(); conn != nil {
		conn.suspendAll()
	}
	h.stopLocked()
	return nil
}

func (h *Hook) current() *connection {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.conn
}

func (h *Hook) startLocked() {
	if h.stopRun != nil {
		return
	}
	ctx, cancel := context.WithCancel(h.lifecycle)
	done := make(chan struct{})
	h.stopRun, h.runDone = cancel, done
	go func() {
		defer close(done)
		h.run(ctx)
	}()
}

func (h *Hook) stopLocked() {
	if h.stopRun == nil {
		return
	}
	h.stopRun()
	<-h.runDone
	h.stopRun, h.runDone = nil, nil
}
