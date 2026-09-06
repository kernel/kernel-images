package scaletozero

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kernel/kernel-images/server/lib/logger"
	"golang.org/x/sys/unix"
)

const (
	responseDrainInitialPollInterval = time.Millisecond
	responseDrainMaxPollInterval     = 100 * time.Millisecond
	responseDrainTimeout             = 5 * time.Minute
	responseAbortRetryInterval       = time.Second
	responseAbortMaxRetryInterval    = 30 * time.Second
)

type connectionContextKey struct{}

type responseDrainConfig struct {
	initialPollInterval time.Duration
	maxPollInterval     time.Duration
	timeout             time.Duration
	outbound            func(*net.TCPConn) (int, error)
	setDeadline         func(*net.TCPConn, time.Time) error
	abort               func(*net.TCPConn) error
	abortRetryInterval  time.Duration
	onComplete          func(responseDrainOutcome)
}

type responseDrainOutcome string

const (
	responseDrainComplete           responseDrainOutcome = "drained"
	responseDrainCloseAcknowledged  responseDrainOutcome = "close_acknowledged"
	responseDrainTimeoutHit         responseDrainOutcome = "timeout"
	responseDrainIOError            responseDrainOutcome = "ioctl_error"
	responseDrainNonTCP             responseDrainOutcome = "untracked_connection"
	responseDrainWriteError         responseDrainOutcome = "write_error"
	responseDrainDeadlineClearError responseDrainOutcome = "deadline_clear_error"
	responseDrainConnectionClosed   responseDrainOutcome = "connection_closed"
	responseDrainConnectionHijacked responseDrainOutcome = "connection_hijacked"
	responseDrainAbortError         responseDrainOutcome = "abort_error"
)

var responseDrainCounters = map[responseDrainOutcome]*atomic.Uint64{
	responseDrainComplete:           {},
	responseDrainCloseAcknowledged:  {},
	responseDrainTimeoutHit:         {},
	responseDrainIOError:            {},
	responseDrainNonTCP:             {},
	responseDrainWriteError:         {},
	responseDrainDeadlineClearError: {},
	responseDrainConnectionClosed:   {},
	responseDrainConnectionHijacked: {},
	responseDrainAbortError:         {},
}

var activeResponseHolds atomic.Int64
var failClosedResponseHolds atomic.Int64

// ResponseDrainOutcomeCounts returns process-lifetime response drain counters.
func ResponseDrainOutcomeCounts() map[string]uint64 {
	counts := make(map[string]uint64, len(responseDrainCounters))
	for outcome, counter := range responseDrainCounters {
		counts[string(outcome)] = counter.Load()
	}
	return counts
}

func ActiveResponseHolds() int64 { return activeResponseHolds.Load() }

func FailClosedResponseHolds() int64 { return failClosedResponseHolds.Load() }

func recordResponseDrainOutcome(outcome responseDrainOutcome) {
	responseDrainCounters[outcome].Add(1)
}

type responseHold struct {
	mu             sync.Mutex
	handlerDone    bool
	connectionDone bool
	released       bool
	release        func()
}

type requestDrain struct {
	hold   *responseHold
	config responseDrainConfig
	log    *slog.Logger
}

func newResponseHold(release func()) *responseHold {
	activeResponseHolds.Add(1)
	return &responseHold{release: release}
}

func (h *responseHold) finishHandler() {
	h.finish(true)
}

func (h *responseHold) finishConnection() {
	h.finish(false)
}

func (h *responseHold) finish(handler bool) {
	h.mu.Lock()
	if handler {
		h.handlerDone = true
	} else {
		h.connectionDone = true
	}
	release := !h.released && h.handlerDone && h.connectionDone
	if release {
		h.released = true
	}
	h.mu.Unlock()
	if release {
		activeResponseHolds.Add(-1)
		h.release()
	}
}

func (d *requestDrain) complete(outcome responseDrainOutcome, queued int, err error) {
	recordResponseDrainOutcome(outcome)
	if d.config.onComplete != nil {
		d.config.onComplete(outcome)
	}
	attrs := []any{"outcome", outcome}
	if queued > 0 {
		attrs = append(attrs, "queued_bytes", queued)
	}
	if err != nil {
		attrs = append(attrs, "error", err)
	}
	switch outcome {
	case responseDrainComplete, responseDrainCloseAcknowledged, responseDrainConnectionClosed, responseDrainConnectionHijacked:
		d.log.Debug("response drain finished", attrs...)
	default:
		d.log.Warn("response drain finished", attrs...)
	}
	d.hold.finishConnection()
}

// Middleware holds scale-to-zero disabled until each non-loopback HTTP response
// is finalized and its TCP send queue drains or the connection terminates.
func Middleware(ctrl Controller) func(http.Handler) http.Handler {
	return middleware(ctrl, responseDrainConfig{
		initialPollInterval: responseDrainInitialPollInterval,
		maxPollInterval:     responseDrainMaxPollInterval,
		timeout:             responseDrainTimeout,
		outbound:            outboundQueue,
		setDeadline:         setWriteDeadline,
		abort:               abortConnection,
		abortRetryInterval:  responseAbortRetryInterval,
	})
}

func middleware(ctrl Controller, config responseDrainConfig) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isLoopbackAddr(r.RemoteAddr) {
				next.ServeHTTP(w, r)
				return
			}

			ctx := context.WithoutCancel(r.Context())
			log := logger.FromContext(ctx)
			conn, ok := r.Context().Value(connectionContextKey{}).(*drainConn)
			if !ok {
				recordResponseDrainOutcome(responseDrainNonTCP)
				log.Error("response drain unavailable", "outcome", responseDrainNonTCP)
				panic(http.ErrAbortHandler)
			}
			if err := ctrl.Disable(r.Context()); err != nil {
				log.Error("failed to disable scale-to-zero", "error", err)
				conn.abortNow(config.abort)
				panic(http.ErrAbortHandler)
			}

			hold := newResponseHold(func() {
				if err := ctrl.Enable(ctx); err != nil {
					log.Error("failed to release response scale-to-zero hold", "error", err)
				}
			})
			drain := &requestDrain{hold: hold, config: config, log: log}
			conn.configure(config)
			registered := conn.addDrain(drain)
			defer hold.finishHandler()
			if !registered {
				return
			}
			defer func() {
				if err := conn.writeError(); err != nil {
					recordResponseDrainOutcome(responseDrainWriteError)
					log.Warn("response write failed", "outcome", responseDrainWriteError, "error", err)
				}
			}()

			next.ServeHTTP(w, r)
		})
	}
}

func waitForResponseDrain(ctx context.Context, conn *net.TCPConn, config responseDrainConfig, deadline time.Time) (responseDrainOutcome, int, error) {
	queued, err := config.outbound(conn)
	if err != nil {
		return responseDrainIOError, queued, err
	}
	if queued == 0 {
		return responseDrainComplete, 0, nil
	}

	interval := config.initialPollInterval
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return responseDrainTimeoutHit, queued, nil
		}
		if interval > remaining {
			interval = remaining
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return "", queued, ctx.Err()
		case <-timer.C:
		}

		queued, err = config.outbound(conn)
		if err != nil {
			return responseDrainIOError, queued, err
		}
		if queued == 0 {
			return responseDrainComplete, 0, nil
		}
		interval = min(interval*2, config.maxPollInterval)
	}
}

func outboundQueue(conn *net.TCPConn) (int, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}

	var queued int
	var ioctlErr error
	if err := raw.Control(func(fd uintptr) {
		queued, ioctlErr = unix.IoctlGetInt(int(fd), unix.TIOCOUTQ)
	}); err != nil {
		return 0, err
	}
	return queued, ioctlErr
}

func setWriteDeadline(conn *net.TCPConn, deadline time.Time) error {
	return conn.SetWriteDeadline(deadline)
}

func abortConnection(conn *net.TCPConn) error {
	if err := conn.SetLinger(0); err != nil {
		return err
	}
	return conn.Close()
}

func isTerminalConnectionError(err error) bool {
	return errors.Is(err, net.ErrClosed) || errors.Is(err, unix.EBADF) || errors.Is(err, unix.ENOTCONN)
}

// isLoopbackAddr reports whether addr is a loopback address.
// addr may be an "ip:port" pair or a bare IP.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}
