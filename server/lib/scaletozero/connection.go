package scaletozero

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

type drainListener struct {
	net.Listener
}

type drainConn struct {
	*net.TCPConn
	mu               sync.Mutex
	terminalMu       sync.Mutex
	pending          []*requestDrain
	state            http.ConnState
	generation       uint64
	drainCancel      context.CancelFunc
	closed           bool
	closing          bool
	hijackedConn     bool
	writeTimeout     time.Duration
	setDeadline      func(*net.TCPConn, time.Time) error
	abortConnection  func(*net.TCPConn) error
	responseWriteErr error
	abortOutcome     responseDrainOutcome
	abortQueued      int
	abortErr         error
}

// Serve adds TCP response tracking to server before serving listener.
func Serve(server *http.Server, listener net.Listener) error {
	server.ConnContext = connectionContext
	server.ConnState = connectionState
	return server.Serve(&drainListener{Listener: listener})
}

func (l *drainListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		_ = conn.Close()
		return nil, errors.New("scale-to-zero response drain requires a TCP listener")
	}
	return &drainConn{TCPConn: tcp, state: http.StateNew}, nil
}

func (c *drainConn) configure(config responseDrainConfig) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writeTimeout = config.timeout
	c.setDeadline = config.setDeadline
	c.abortConnection = config.abort
	c.responseWriteErr = nil
}

func (c *drainConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	if c.closed || c.closing {
		c.mu.Unlock()
		return 0, net.ErrClosed
	}
	timeout := c.writeTimeout
	setDeadline := c.setDeadline
	abort := c.abortConnection
	c.mu.Unlock()

	if timeout > 0 {
		if err := setDeadline(c.TCPConn, time.Now().Add(timeout)); err != nil {
			c.recordWriteError(err)
			c.abortNow(abort)
			return 0, err
		}
	}

	n, err := c.TCPConn.Write(p)
	if err != nil {
		c.recordWriteError(err)
		c.abortNow(abort)
	}
	return n, err
}

func (c *drainConn) recordWriteError(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.responseWriteErr == nil {
		c.responseWriteErr = err
	}
}

func (c *drainConn) writeError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.responseWriteErr
}

func (c *drainConn) addDrain(drain *requestDrain) bool {
	c.mu.Lock()
	switch {
	case c.hijackedConn:
		c.mu.Unlock()
		drain.complete(responseDrainConnectionHijacked, 0, nil)
		return false
	case c.closed || c.closing:
		c.mu.Unlock()
		drain.complete(responseDrainConnectionClosed, 0, nil)
		return false
	default:
		c.pending = append(c.pending, drain)
		c.mu.Unlock()
		return true
	}
}

func (c *drainConn) setState(state http.ConnState) {
	switch state {
	case http.StateActive:
		c.activate()
	case http.StateIdle:
		c.startIdleDrain()
	case http.StateHijacked:
		c.hijack()
	}
}

func (c *drainConn) activate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.hijackedConn {
		return
	}
	c.state = http.StateActive
	c.generation++
	if c.drainCancel != nil {
		c.drainCancel()
		c.drainCancel = nil
	}
}

func (c *drainConn) startIdleDrain() {
	c.mu.Lock()
	if c.closed || c.closing || c.hijackedConn || len(c.pending) == 0 {
		c.mu.Unlock()
		return
	}
	c.state = http.StateIdle
	c.generation++
	generation := c.generation
	if c.drainCancel != nil {
		c.drainCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.drainCancel = cancel
	config := c.pending[len(c.pending)-1].config
	c.mu.Unlock()

	go c.runIdleDrain(ctx, generation, config)
}

func (c *drainConn) runIdleDrain(ctx context.Context, generation uint64, config responseDrainConfig) {
	outcome, queued, err := waitForResponseDrain(ctx, c.TCPConn, config, time.Now().Add(config.timeout))
	if errors.Is(err, context.Canceled) {
		return
	}

	c.mu.Lock()
	if c.closed || c.hijackedConn || c.generation != generation || c.state != http.StateIdle {
		c.mu.Unlock()
		return
	}
	if outcome == responseDrainComplete {
		if clearErr := config.setDeadline(c.TCPConn, time.Time{}); clearErr == nil {
			drains := c.takePendingLocked()
			c.drainCancel = nil
			c.mu.Unlock()
			completeDrains(drains, outcome, queued, nil)
			return
		} else {
			outcome = responseDrainDeadlineClearError
			err = clearErr
		}
	}

	c.closing = true
	c.abortOutcome = outcome
	c.abortQueued = queued
	c.abortErr = err
	c.drainCancel = nil
	c.mu.Unlock()
	go c.retryAbort(config)
}

func (c *drainConn) retryAbort(config responseDrainConfig) {
	failedCount := 0
	retryInterval := config.abortRetryInterval
	for {
		c.mu.Lock()
		if c.closed && len(c.pending) == 0 {
			c.mu.Unlock()
			if failedCount > 0 {
				failClosedResponseHolds.Add(-int64(failedCount))
			}
			return
		}
		c.mu.Unlock()

		c.terminalMu.Lock()
		err := config.abort(c.TCPConn)
		c.terminalMu.Unlock()
		if err == nil || isTerminalConnectionError(err) {
			c.mu.Lock()
			if c.closed && len(c.pending) == 0 {
				c.mu.Unlock()
				if failedCount > 0 {
					failClosedResponseHolds.Add(-int64(failedCount))
				}
				return
			}
			c.closed = true
			c.closing = false
			drains := c.takePendingLocked()
			outcome, queued, drainErr := c.abortOutcome, c.abortQueued, c.abortErr
			c.mu.Unlock()
			if failedCount > 0 {
				failClosedResponseHolds.Add(-int64(failedCount))
			}
			completeDrains(drains, outcome, queued, drainErr)
			return
		}

		recordResponseDrainOutcome(responseDrainAbortError)
		c.mu.Lock()
		drains := len(c.pending)
		if failedCount == 0 {
			failClosedResponseHolds.Add(int64(drains))
			failedCount = drains
		}
		log := firstDrainLog(c.pending)
		c.mu.Unlock()
		if log != nil {
			log.Error("failed to abort response connection; retrying while scale-to-zero remains held", "outcome", responseDrainAbortError, "error", err)
		}
		time.Sleep(retryInterval)
		retryInterval = min(retryInterval*2, responseAbortMaxRetryInterval)
	}
}

func (c *drainConn) abortNow(abort func(*net.TCPConn) error) {
	if abort == nil {
		abort = abortConnection
	}
	c.terminalMu.Lock()
	err := abort(c.TCPConn)
	c.terminalMu.Unlock()
	c.mu.Lock()
	if err != nil && !isTerminalConnectionError(err) {
		c.closing = true
		c.abortOutcome = responseDrainWriteError
		c.abortErr = err
		config := responseDrainConfig{abort: abort, abortRetryInterval: responseAbortRetryInterval}
		if len(c.pending) > 0 {
			config = c.pending[len(c.pending)-1].config
		}
		c.mu.Unlock()
		go c.retryAbort(config)
		return
	}
	c.closed = true
	c.closing = false
	drains := c.takePendingLocked()
	c.mu.Unlock()
	completeDrains(drains, responseDrainConnectionClosed, 0, nil)
}

func (c *drainConn) hijack() {
	c.mu.Lock()
	if c.closed || c.hijackedConn {
		c.mu.Unlock()
		return
	}
	c.hijackedConn = true
	c.state = http.StateHijacked
	c.generation++
	if c.drainCancel != nil {
		c.drainCancel()
		c.drainCancel = nil
	}
	c.writeTimeout = 0
	setDeadline := c.setDeadline
	drains := c.takePendingLocked()
	c.mu.Unlock()

	if setDeadline != nil {
		if err := setDeadline(c.TCPConn, time.Time{}); err != nil {
			recordResponseDrainOutcome(responseDrainDeadlineClearError)
			if log := firstDrainLog(drains); log != nil {
				log.Warn("failed to clear hijacked connection deadline", "outcome", responseDrainDeadlineClearError, "error", err)
			}
		}
	}
	completeDrains(drains, responseDrainConnectionHijacked, 0, nil)
}

func (c *drainConn) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.generation++
	if c.drainCancel != nil {
		c.drainCancel()
		c.drainCancel = nil
	}
	drains := c.takePendingLocked()
	hijacked := c.hijackedConn
	config := responseDrainConfig{}
	if len(drains) > 0 {
		config = drains[len(drains)-1].config
	}
	c.mu.Unlock()

	c.terminalMu.Lock()
	defer c.terminalMu.Unlock()
	if hijacked || len(drains) == 0 {
		return c.TCPConn.Close()
	}

	fd, err := duplicateAndShutdownWrite(c.TCPConn)
	if err != nil {
		if isTerminalConnectionError(err) {
			_ = c.TCPConn.Close()
			completeDrains(drains, responseDrainConnectionClosed, 0, nil)
			return nil
		}
		recordResponseDrainOutcome(responseDrainAbortError)
		abortErr := config.abort(c.TCPConn)
		if abortErr == nil || isTerminalConnectionError(abortErr) {
			completeDrains(drains, responseDrainConnectionClosed, 0, err)
			return nil
		}
		userTimeoutErr := setTCPUserTimeout(c.TCPConn, config.timeout)
		closeErr := c.TCPConn.Close()
		if userTimeoutErr == nil {
			failClosedResponseHolds.Add(int64(len(drains)))
			go func() {
				time.Sleep(config.timeout)
				failClosedResponseHolds.Add(-int64(len(drains)))
				completeDrains(drains, responseDrainConnectionClosed, 0, errors.Join(err, abortErr, closeErr))
			}()
			return closeErr
		}
		failClosedResponseHolds.Add(int64(len(drains)))
		if log := firstDrainLog(drains); log != nil {
			log.Error("failed to establish terminal response state; retaining scale-to-zero holds", "outcome", responseDrainAbortError, "error", errors.Join(err, abortErr, userTimeoutErr, closeErr))
		}
		return closeErr
	}

	closeErr := c.TCPConn.Close()
	go monitorClosedResponse(fd, drains, config)
	return closeErr
}

func (c *drainConn) takePendingLocked() []*requestDrain {
	drains := c.pending
	c.pending = nil
	return drains
}

func completeDrains(drains []*requestDrain, outcome responseDrainOutcome, queued int, err error) {
	for _, drain := range drains {
		drain.complete(outcome, queued, err)
	}
}

func firstDrainLog(drains []*requestDrain) *slog.Logger {
	if len(drains) == 0 {
		return nil
	}
	return drains[0].log
}

func connectionContext(ctx context.Context, conn net.Conn) context.Context {
	tracked, ok := conn.(*drainConn)
	if !ok {
		return ctx
	}
	return context.WithValue(ctx, connectionContextKey{}, tracked)
}

func connectionState(conn net.Conn, state http.ConnState) {
	if tracked, ok := conn.(*drainConn); ok {
		tracked.setState(state)
	}
}

func duplicateAndShutdownWrite(conn *net.TCPConn) (int, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return -1, err
	}
	fd := -1
	var opErr error
	if err := raw.Control(func(rawFD uintptr) {
		fd, opErr = unix.FcntlInt(rawFD, unix.F_DUPFD_CLOEXEC, 0)
	}); err != nil {
		return -1, err
	}
	if opErr != nil {
		return -1, opErr
	}
	if err := unix.Shutdown(fd, unix.SHUT_WR); err != nil {
		_ = unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

func monitorClosedResponse(fd int, drains []*requestDrain, config responseDrainConfig) {
	outcome, queued, err := waitForClosedResponse(fd, config, time.Now().Add(config.timeout))
	if outcome == responseDrainCloseAcknowledged {
		_ = unix.Close(fd)
		completeDrains(drains, outcome, queued, err)
		return
	}

	failed := false
	retryInterval := config.abortRetryInterval
	for {
		lingerErr := unix.SetsockoptLinger(fd, unix.SOL_SOCKET, unix.SO_LINGER, &unix.Linger{Onoff: 1})
		if lingerErr == nil || isTerminalConnectionError(lingerErr) {
			_ = unix.Close(fd)
			if failed {
				failClosedResponseHolds.Add(-int64(len(drains)))
			}
			completeDrains(drains, outcome, queued, err)
			return
		}
		if !failed {
			failClosedResponseHolds.Add(int64(len(drains)))
			failed = true
		}
		recordResponseDrainOutcome(responseDrainAbortError)
		if log := firstDrainLog(drains); log != nil {
			log.Error("failed to abort close-delimited response; retrying while scale-to-zero remains held", "outcome", responseDrainAbortError, "error", lingerErr)
		}
		time.Sleep(retryInterval)
		retryInterval = min(retryInterval*2, responseAbortMaxRetryInterval)
	}
}

func waitForClosedResponse(fd int, config responseDrainConfig, deadline time.Time) (responseDrainOutcome, int, error) {
	interval := config.initialPollInterval
	queued := 0
	for {
		var err error
		queued, err = unix.IoctlGetInt(fd, unix.TIOCOUTQ)
		if err != nil {
			return responseDrainIOError, queued, err
		}
		acked, err := closeAcknowledged(fd)
		if err != nil {
			return responseDrainIOError, queued, err
		}
		if queued == 0 && acked {
			return responseDrainCloseAcknowledged, 0, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return responseDrainTimeoutHit, queued, nil
		}
		time.Sleep(min(interval, remaining))
		interval = min(interval*2, config.maxPollInterval)
	}
}
