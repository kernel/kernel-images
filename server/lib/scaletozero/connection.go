package scaletozero

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const responseWriteChunkSize = 1 << 20

var errResponseMonitorLimit = errors.New("response close monitor limit reached")

type drainListener struct {
	net.Listener
}

type drainConn struct {
	*net.TCPConn
	mu              sync.Mutex
	terminalMu      sync.Mutex
	pending         []*requestDrain
	state           http.ConnState
	generation      uint64
	drainCancel     context.CancelFunc
	closed          bool
	closing         bool
	hijackedConn    bool
	writeTimeout    time.Duration
	setDeadline     func(*net.TCPConn, time.Time) error
	abortConnection func(*net.TCPConn) error
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
}

func (c *drainConn) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		timeout, setDeadline, abort, hijacked, err := c.writeConfig()
		if err != nil {
			return total, err
		}
		if hijacked {
			n, err := c.TCPConn.Write(p)
			return total + n, err
		}
		if timeout > 0 {
			if err := setDeadline(c.TCPConn, time.Now().Add(timeout)); err != nil {
				c.abortNowWithOutcome(abort, responseDrainWriteError, err)
				return total, err
			}
		}

		chunk := p
		if len(chunk) > responseWriteChunkSize {
			chunk = chunk[:responseWriteChunkSize]
		}
		n, err := c.TCPConn.Write(chunk)
		total += n
		if err == nil && n != len(chunk) {
			err = io.ErrShortWrite
		}
		if err != nil {
			c.abortNowWithOutcome(abort, responseDrainWriteError, err)
			return total, err
		}
		p = p[n:]
	}
	return total, nil
}

func (c *drainConn) ReadFrom(r io.Reader) (int64, error) {
	var total int64
	for {
		timeout, setDeadline, abort, hijacked, err := c.writeConfig()
		if err != nil {
			return total, err
		}
		if hijacked {
			n, err := c.TCPConn.ReadFrom(r)
			return total + n, err
		}
		if timeout > 0 {
			if err := setDeadline(c.TCPConn, time.Now().Add(timeout)); err != nil {
				c.abortNowWithOutcome(abort, responseDrainWriteError, err)
				return total, err
			}
		}

		limited, parents, exhausted := nextReadFromChunk(r)
		if exhausted {
			return total, nil
		}
		n, err := c.TCPConn.ReadFrom(limited)
		total += n
		for _, parent := range parents {
			parent.N -= n
		}
		if errors.Is(err, io.EOF) {
			return total, nil
		}
		if err != nil {
			if isReadFromWriteError(limited.R, err) {
				c.abortNowWithOutcome(abort, responseDrainWriteError, err)
			} else {
				c.markCurrentFailure(responseDrainSourceReadError, err)
			}
			return total, err
		}
		if limited.N > 0 || anyLimitExhausted(parents) {
			return total, nil
		}
	}
}

func (c *drainConn) writeConfig() (time.Duration, func(*net.TCPConn, time.Time) error, func(*net.TCPConn) error, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.closing {
		return 0, nil, nil, false, net.ErrClosed
	}
	if c.hijackedConn {
		return 0, nil, nil, true, nil
	}
	return c.writeTimeout, c.setDeadline, c.abortConnection, false, nil
}

func nextReadFromChunk(r io.Reader) (*io.LimitedReader, []*io.LimitedReader, bool) {
	limit := int64(responseWriteChunkSize)
	parents := make([]*io.LimitedReader, 0, 1)
	for {
		parent, ok := r.(*io.LimitedReader)
		if !ok {
			break
		}
		if parent.N <= 0 {
			return nil, parents, true
		}
		parents = append(parents, parent)
		limit = min(limit, parent.N)
		r = parent.R
	}
	return &io.LimitedReader{R: r, N: limit}, parents, false
}

func anyLimitExhausted(parents []*io.LimitedReader) bool {
	for _, parent := range parents {
		if parent.N == 0 {
			return true
		}
	}
	return false
}

func isReadFromWriteError(source io.Reader, err error) bool {
	if _, ambiguous := source.(net.Conn); ambiguous {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return errors.Is(err, net.ErrClosed) ||
		errors.Is(err, unix.EPIPE) ||
		errors.Is(err, unix.ECONNRESET) ||
		errors.Is(err, unix.ECONNABORTED) ||
		errors.Is(err, unix.ETIMEDOUT) ||
		errors.Is(err, unix.ENETDOWN) ||
		errors.Is(err, unix.ENETRESET) ||
		errors.Is(err, unix.ENETUNREACH) ||
		errors.Is(err, unix.EHOSTDOWN) ||
		errors.Is(err, unix.EHOSTUNREACH)
}

func (c *drainConn) markCurrentFailure(outcome responseDrainOutcome, err error) {
	c.mu.Lock()
	var drain *requestDrain
	if len(c.pending) > 0 {
		drain = c.pending[len(c.pending)-1]
	}
	c.mu.Unlock()
	if drain != nil {
		drain.fail(outcome, err)
	}
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
		previous := c.pending
		c.pending = []*requestDrain{drain}
		c.mu.Unlock()
		completeDrains(previous, responseDrainConnectionReused, 0, nil)
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

	c.closed = true
	c.closing = true
	c.drainCancel = nil
	drains := c.takePendingLocked()
	c.mu.Unlock()
	go c.recoverResponseConnection(drains, config, outcome, queued, err)
}

func (c *drainConn) abortNow(abort func(*net.TCPConn) error) {
	c.abortNowWithOutcome(abort, responseDrainConnectionClosed, nil)
}

func (c *drainConn) abortNowWithOutcome(abort func(*net.TCPConn) error, outcome responseDrainOutcome, cause error) {
	config := defaultResponseDrainConfig()
	if abort != nil {
		config.abort = abort
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	if len(c.pending) > 0 {
		config = c.pending[len(c.pending)-1].config
		if abort != nil {
			config.abort = abort
		}
	}
	c.closed = true
	c.closing = true
	drains := c.takePendingLocked()
	c.mu.Unlock()

	c.terminalMu.Lock()
	err := config.abort(c.TCPConn)
	c.terminalMu.Unlock()
	if len(drains) == 0 {
		if err != nil && !isTerminalConnectionError(err) {
			_ = c.TCPConn.Close()
		}
		return
	}
	if err == nil || isTerminalConnectionError(err) {
		completeDrains(drains, outcome, 0, cause)
		return
	}
	go c.recoverResponseConnection(drains, config, outcome, 0, errors.Join(cause, err))
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
	if !config.acquireMonitor() {
		responseCloseMonitorRejections.Add(1)
		abortErr := config.abort(c.TCPConn)
		if abortErr == nil || isTerminalConnectionError(abortErr) {
			completeDrains(drains, responseDrainMonitorLimit, 0, errResponseMonitorLimit)
			return nil
		}
		go c.recoverResponseConnection(drains, config, responseDrainMonitorLimit, 0, errors.Join(errResponseMonitorLimit, abortErr))
		return abortErr
	}

	fd, err := config.duplicate(c.TCPConn)
	if err == nil {
		closeErr := c.TCPConn.Close()
		go monitorClosedResponse(fd, drains, config, false, nil, "")
		return closeErr
	}
	config.releaseMonitor()
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
	go c.recoverResponseConnection(drains, config, responseDrainConnectionClosed, 0, errors.Join(err, abortErr))
	return errors.Join(err, abortErr)
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

func (c *drainConn) recoverResponseConnection(drains []*requestDrain, config responseDrainConfig, outcome responseDrainOutcome, queued int, cause error) {
	failedCount := len(drains)
	if failedCount > 0 {
		failClosedResponseHolds.Add(int64(failedCount))
	}
	retryInterval := config.abortRetryInterval
	deadline := time.Now().Add(config.terminalRecoveryTimeout)
	for {
		c.terminalMu.Lock()
		abortErr := config.abort(c.TCPConn)
		if abortErr == nil || isTerminalConnectionError(abortErr) {
			c.terminalMu.Unlock()
			if failedCount > 0 {
				failClosedResponseHolds.Add(-int64(failedCount))
			}
			completeDrains(drains, outcome, queued, cause)
			return
		}

		fd := -1
		duplicateErr := errResponseMonitorLimit
		monitorAcquired := config.acquireMonitor()
		if monitorAcquired {
			fd, duplicateErr = config.duplicate(c.TCPConn)
		} else {
			responseCloseMonitorRejections.Add(1)
		}
		if duplicateErr == nil {
			closeErr := c.TCPConn.Close()
			c.terminalMu.Unlock()
			go monitorClosedResponse(fd, drains, config, true, errors.Join(cause, abortErr, closeErr), outcome)
			return
		}
		if monitorAcquired {
			config.releaseMonitor()
		}
		if isTerminalConnectionError(duplicateErr) {
			_ = c.TCPConn.Close()
			c.terminalMu.Unlock()
			if failedCount > 0 {
				failClosedResponseHolds.Add(-int64(failedCount))
			}
			completeDrains(drains, responseDrainConnectionClosed, queued, cause)
			return
		}
		c.terminalMu.Unlock()

		recordResponseDrainOutcome(responseDrainAbortError)
		terminalErr := errors.Join(cause, abortErr, duplicateErr)
		if time.Now().After(deadline) {
			terminateAfterResponseFailure(drains, config, terminalErr)
		}
		if log := firstDrainLog(drains); log != nil {
			log.Error("failed to terminate response connection; retrying while scale-to-zero remains held", "outcome", responseDrainAbortError, "error", terminalErr)
		}
		time.Sleep(min(retryInterval, time.Until(deadline)))
		retryInterval = min(retryInterval*2, responseAbortMaxRetryInterval)
	}
}

func isSafeClosedResponseOutcome(outcome responseDrainOutcome) bool {
	return outcome == responseDrainCloseAcknowledged || outcome == responseDrainConnectionClosed
}

func monitorClosedResponse(fd int, drains []*requestDrain, config responseDrainConfig, failed bool, cause error, failureOutcome responseDrainOutcome) {
	activeResponseCloseMonitors.Add(1)
	defer activeResponseCloseMonitors.Add(-1)
	defer config.releaseMonitor()

	userTimeoutErr := config.setUserTimeout(fd, config.terminalRecoveryTimeout)
	outcome, queued, err := waitForClosedResponse(fd, config, time.Now().Add(config.timeout))
	cause = errors.Join(cause, userTimeoutErr, err)
	if isSafeClosedResponseOutcome(outcome) {
		_ = config.closeFD(fd)
		if failed {
			failClosedResponseHolds.Add(-int64(len(drains)))
		}
		if failureOutcome != "" {
			outcome = failureOutcome
		}
		completeDrains(drains, outcome, queued, cause)
		return
	}

	if !failed {
		failClosedResponseHolds.Add(int64(len(drains)))
	}
	retryInterval := config.abortRetryInterval
	deadline := time.Now().Add(config.terminalRecoveryTimeout)
	for {
		abortErr := config.abortFD(fd)
		if abortErr == nil || isTerminalConnectionError(abortErr) {
			failClosedResponseHolds.Add(-int64(len(drains)))
			if failureOutcome != "" {
				outcome = failureOutcome
			}
			completeDrains(drains, outcome, queued, cause)
			return
		}

		currentOutcome, currentQueued, inspectErr := waitForClosedResponse(fd, config, time.Now())
		if isSafeClosedResponseOutcome(currentOutcome) || isTerminalConnectionError(inspectErr) {
			_ = config.closeFD(fd)
			failClosedResponseHolds.Add(-int64(len(drains)))
			if !isSafeClosedResponseOutcome(currentOutcome) {
				currentOutcome = responseDrainConnectionClosed
			}
			if failureOutcome != "" {
				currentOutcome = failureOutcome
			}
			completeDrains(drains, currentOutcome, currentQueued, cause)
			return
		}
		userTimeoutErr = config.setUserTimeout(fd, config.terminalRecoveryTimeout)
		recordResponseDrainOutcome(responseDrainAbortError)
		terminalErr := errors.Join(cause, abortErr, inspectErr, userTimeoutErr)
		if time.Now().After(deadline) {
			terminateAfterResponseFailure(drains, config, terminalErr)
		}
		if log := firstDrainLog(drains); log != nil {
			log.Error("failed to abort closed response; retrying while scale-to-zero remains held", "outcome", responseDrainAbortError, "error", terminalErr)
		}
		time.Sleep(min(retryInterval, time.Until(deadline)))
		retryInterval = min(retryInterval*2, responseAbortMaxRetryInterval)
	}
}

func terminateAfterResponseFailure(drains []*requestDrain, config responseDrainConfig, err error) {
	recordResponseDrainOutcome(responseDrainGuestTermination)
	if log := firstDrainLog(drains); log != nil {
		log.Error("response connection could not be terminated; terminating guest", "outcome", responseDrainGuestTermination, "error", err)
	}
	config.terminateGuest()
	panic("scale-to-zero guest termination returned")
}

func waitForClosedResponse(fd int, config responseDrainConfig, deadline time.Time) (responseDrainOutcome, int, error) {
	interval := config.initialPollInterval
	queued := 0
	for {
		var err error
		queued, err = config.outboundFD(fd)
		if err != nil {
			return responseDrainIOError, queued, err
		}
		state, err := config.closeState(fd)
		if err != nil {
			return responseDrainIOError, queued, err
		}
		switch {
		case state == responseCloseTerminated:
			return responseDrainConnectionClosed, queued, nil
		case queued == 0 && state == responseCloseAcknowledged:
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
