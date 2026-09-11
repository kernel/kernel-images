package scaletozero

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestHijackedWebSocketCancellationAndCloseReturnPromptly(t *testing.T) {
	config := testDrainConfig(2*time.Second, func(*net.TCPConn) (int, error) { return 1, nil })
	accepted := make(chan struct{})
	startWrite := make(chan struct{})
	writeResult := make(chan struct {
		duration time.Duration
		err      error
	}, 1)
	closeDuration := make(chan time.Duration, 1)
	handler := middleware(NewNoopController(), config)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tracked := r.Context().Value(connectionContextKey{}).(*drainConn)
		_ = tracked.SetWriteBuffer(4 << 10)
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			writeResult <- struct {
				duration time.Duration
				err      error
			}{err: err}
			return
		}
		close(accepted)
		<-startWrite
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		started := time.Now()
		err = conn.Write(ctx, websocket.MessageBinary, bytes.Repeat([]byte("x"), 32<<20))
		cancel()
		writeResult <- struct {
			duration time.Duration
			err      error
		}{duration: time.Since(started), err: err}
		started = time.Now()
		conn.CloseNow()
		closeDuration <- time.Since(started)
	}))

	listener, serverDone, server := serveTestHandler(t, handler)
	defer func() {
		require.NoError(t, server.Close())
		assert.ErrorIs(t, <-serverDone, http.ErrServerClosed)
	}()
	conn, _, err := websocket.Dial(context.Background(), "ws://"+listener.Addr().String(), nil)
	require.NoError(t, err)
	defer conn.CloseNow()
	<-accepted
	close(startWrite)

	select {
	case result := <-writeResult:
		require.Error(t, result.err)
		assert.Less(t, result.duration, 500*time.Millisecond)
	case <-time.After(time.Second):
		t.Fatal("WebSocket write cancellation blocked on response draining")
	}
	select {
	case elapsed := <-closeDuration:
		assert.Less(t, elapsed, 200*time.Millisecond)
	case <-time.After(time.Second):
		t.Fatal("WebSocket close blocked on response draining")
	}
}

func TestUntrackedWriteFailuresDoNotStartResponseRecovery(t *testing.T) {
	tests := []struct {
		name       string
		hijack     bool
		wantAborts int32
	}{
		{name: "pre-registration", wantAborts: 1},
		{name: "hijacked", hijack: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tracked, peer := newTCPPair(t)
			defer tracked.TCPConn.Close()

			var aborts atomic.Int32
			var duplicates atomic.Int32
			terminated := make(chan struct{}, 1)
			config := testDrainConfig(time.Second, outboundQueue)
			config.abort = func(*net.TCPConn) error {
				aborts.Add(1)
				return assert.AnError
			}
			config.duplicate = func(*net.TCPConn) (int, error) {
				duplicates.Add(1)
				return -1, assert.AnError
			}
			config.abortRetryInterval = time.Millisecond
			config.terminalRecoveryTimeout = 5 * time.Millisecond
			config.terminateGuest = func() {
				terminated <- struct{}{}
				runtime.Goexit()
			}
			tracked.configure(config)
			if tc.hijack {
				tracked.hijack()
			}

			require.NoError(t, peer.(*net.TCPConn).SetLinger(0))
			require.NoError(t, peer.Close())
			require.Eventually(t, func() bool {
				_, err := tracked.Write([]byte("response"))
				return err != nil
			}, time.Second, time.Millisecond)

			assert.Equal(t, tc.wantAborts, aborts.Load())
			assert.Zero(t, duplicates.Load())
			select {
			case <-terminated:
				t.Fatal("untracked write failure terminated the guest")
			case <-time.After(20 * time.Millisecond):
			}
		})
	}
}

func TestShutdownDoesNotBlockOnIdleResponseDrain(t *testing.T) {
	started := make(chan struct{})
	config := testDrainConfig(5*time.Second, func(*net.TCPConn) (int, error) {
		select {
		case <-started:
		default:
			close(started)
		}
		return 1, nil
	})
	handler := middleware(NewNoopController(), config)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "2")
		_, _ = io.WriteString(w, "ok")
	}))

	listener, serverDone, server := serveTestHandler(t, handler)
	conn, err := net.Dial("tcp", listener.Addr().String())
	require.NoError(t, err)
	defer conn.Close()
	_, err = fmt.Fprint(conn, "GET / HTTP/1.1\r\nHost: test\r\n\r\n")
	require.NoError(t, err)
	response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodGet})
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	before := time.Now()
	require.NoError(t, server.Shutdown(ctx))
	assert.Less(t, time.Since(before), 200*time.Millisecond)
	assert.ErrorIs(t, <-serverDone, http.ErrServerClosed)
}

func TestDisableAndAbortFailureNeverReturnsSuccess(t *testing.T) {
	ctrl := &mockScaleToZeroer{disableErr: assert.AnError}
	config := testDrainConfig(time.Second, outboundQueue)
	config.abort = func(*net.TCPConn) error { return assert.AnError }
	called := make(chan struct{}, 1)
	handler := middleware(ctrl, config)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called <- struct{}{}
	}))

	listener, serverDone, server := serveTestHandler(t, handler)
	defer func() {
		require.NoError(t, server.Close())
		assert.ErrorIs(t, <-serverDone, http.ErrServerClosed)
	}()
	conn, err := net.Dial("tcp", listener.Addr().String())
	require.NoError(t, err)
	defer conn.Close()
	_, err = fmt.Fprint(conn, "POST /mutate HTTP/1.1\r\nHost: test\r\nContent-Length: 0\r\n\r\n")
	require.NoError(t, err)
	response, readErr := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
	if readErr == nil {
		defer response.Body.Close()
		assert.GreaterOrEqual(t, response.StatusCode, http.StatusInternalServerError)
	}
	select {
	case <-called:
		t.Fatal("application handler ran after scale-to-zero disable failed")
	default:
	}
}

func TestHTTP10CloseDelimitedResponseWaitsForCloseAcknowledgement(t *testing.T) {
	const bodySize = 4 << 20
	base := newSignalController()
	ctrl := NewDebouncedController(base)
	outcome := make(chan responseDrainOutcome, 1)
	config := testDrainConfig(5*time.Second, outboundQueue)
	config.onComplete = func(value responseDrainOutcome) { outcome <- value }
	handler := middleware(ctrl, config)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		for remaining := bodySize; remaining > 0; remaining -= 32 << 10 {
			_, _ = w.Write(bytes.Repeat([]byte("x"), 32<<10))
			flusher.Flush()
		}
	}))

	listener, serverDone, server := serveTestHandler(t, handler)
	defer func() {
		require.NoError(t, server.Close())
		assert.ErrorIs(t, <-serverDone, http.ErrServerClosed)
	}()
	conn, err := net.Dial("tcp", listener.Addr().String())
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.(*net.TCPConn).SetReadBuffer(32<<10))
	_, err = fmt.Fprint(conn, "GET /stream HTTP/1.0\r\nHost: test\r\n\r\n")
	require.NoError(t, err)
	response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodGet})
	require.NoError(t, err)
	assert.Equal(t, int64(-1), response.ContentLength)
	assert.True(t, response.Close)
	written, err := io.Copy(io.Discard, response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	assert.Equal(t, int64(bodySize), written)

	select {
	case <-base.enabled:
	case <-time.After(time.Second):
		t.Fatal("scale-to-zero was not enabled after close acknowledgement")
	}
	assert.Equal(t, responseDrainCloseAcknowledged, <-outcome)
}

func TestResetClosedResponseCompletesWithQueuedBytes(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("TCP output queue inspection requires Linux")
	}
	tracked, clientConn := newTCPPair(t)
	client := clientConn.(*net.TCPConn)
	defer tracked.TCPConn.Close()
	require.NoError(t, tracked.SetWriteBuffer(64<<10))
	require.NoError(t, client.SetReadBuffer(4<<10))
	raw, err := tracked.SyscallConn()
	require.NoError(t, err)
	payload := make([]byte, 32<<20)
	written := 0
	var writeErr error
	require.NoError(t, raw.Write(func(fd uintptr) bool {
		for written < len(payload) {
			n, err := unix.Write(int(fd), payload[written:])
			written += n
			if errors.Is(err, unix.EAGAIN) {
				return true
			}
			if err != nil {
				writeErr = err
				return true
			}
		}
		return true
	}))
	require.NoError(t, writeErr)
	require.Positive(t, written)
	require.Less(t, written, len(payload))
	queuedBeforeReset, err := outboundQueue(tracked.TCPConn)
	require.NoError(t, err)
	require.Positive(t, queuedBeforeReset)

	fd, err := duplicateAndShutdownWrite(tracked.TCPConn)
	require.NoError(t, err)
	defer closeSocket(fd)
	require.NoError(t, tracked.TCPConn.Close())
	require.NoError(t, client.SetLinger(0))
	require.NoError(t, client.Close())

	config := testDrainConfig(time.Second, outboundQueue)
	started := time.Now()
	outcome, _, err := waitForClosedResponse(fd, config, time.Now().Add(config.timeout))
	require.NoError(t, err)
	assert.Equal(t, responseDrainConnectionClosed, outcome)
	assert.Less(t, time.Since(started), 250*time.Millisecond)
}

func TestTerminatedClosedResponseIgnoresQueuedBytes(t *testing.T) {
	config := testDrainConfig(time.Second, outboundQueue)
	config.outboundFD = func(int) (int, error) { return 123, nil }
	config.closeState = func(int) (responseCloseState, error) { return responseCloseTerminated, nil }

	outcome, queued, err := waitForClosedResponse(-1, config, time.Now().Add(config.timeout))
	require.NoError(t, err)
	assert.Equal(t, responseDrainConnectionClosed, outcome)
	assert.Equal(t, 123, queued)
}

func TestResponseDrainControlsScaleToZeroFile(t *testing.T) {
	const bodySize = 2 << 20
	scaleFile := filepath.Join(t.TempDir(), "scale_to_zero_disable")
	require.NoError(t, os.WriteFile(scaleFile, []byte("-"), 0o600))
	ctrl := NewDebouncedController(&unikraftCloudController{path: scaleFile})
	handler := middleware(ctrl, testDrainConfig(5*time.Second, outboundQueue))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(bodySize))
		_, _ = io.Copy(w, bytes.NewReader(make([]byte, bodySize)))
	}))
	listener, serverDone, server := serveTestHandler(t, handler)
	defer func() {
		require.NoError(t, server.Close())
		assert.ErrorIs(t, <-serverDone, http.ErrServerClosed)
	}()

	conn, err := net.Dial("tcp", listener.Addr().String())
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.(*net.TCPConn).SetReadBuffer(32<<10))
	_, err = fmt.Fprint(conn, "GET /large HTTP/1.1\r\nHost: test\r\n\r\n")
	require.NoError(t, err)
	response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodGet})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		value, readErr := os.ReadFile(scaleFile)
		return readErr == nil && string(value) == "+"
	}, time.Second, time.Millisecond)
	_, err = io.Copy(io.Discard, response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Eventually(t, func() bool {
		value, readErr := os.ReadFile(scaleFile)
		return readErr == nil && string(value) == "-"
	}, time.Second, time.Millisecond)
}

func TestPersistentConnectionRecoveryTerminatesGuestWithinBound(t *testing.T) {
	tracked, client := newTCPPair(t)
	defer client.Close()
	defer tracked.TCPConn.Close()

	activeBefore := ActiveResponseHolds()
	failedBefore := FailClosedResponseHolds()
	released := make(chan struct{})
	hold := newResponseHold(func() { close(released) })
	hold.finishHandler()
	config := testDrainConfig(20*time.Millisecond, outboundQueue)
	config.abort = func(*net.TCPConn) error { return assert.AnError }
	config.duplicate = func(*net.TCPConn) (int, error) { return -1, assert.AnError }
	terminated := make(chan struct{})
	config.terminateGuest = func() {
		close(terminated)
		runtime.Goexit()
	}
	drains := []*requestDrain{{hold: hold, config: config, log: slog.Default()}}

	go tracked.recoverResponseConnection(drains, config, responseDrainIOError, 1, assert.AnError)
	select {
	case <-terminated:
	case <-time.After(time.Second):
		t.Fatal("persistent connection recovery did not terminate the guest")
	}
	select {
	case <-released:
		t.Fatal("scale-to-zero hold released without a drained or aborted connection")
	default:
	}
	assert.Equal(t, activeBefore+1, ActiveResponseHolds())
	assert.Equal(t, failedBefore+1, FailClosedResponseHolds())

	failClosedResponseHolds.Add(-1)
	completeDrains(drains, responseDrainConnectionClosed, 0, nil)
	assert.Equal(t, activeBefore, ActiveResponseHolds())
	assert.Equal(t, failedBefore, FailClosedResponseHolds())
}

func TestTerminalDuplicationFailureClosesOriginalConnection(t *testing.T) {
	tracked, peer := newTCPPair(t)
	defer peer.Close()

	activeBefore := ActiveResponseHolds()
	failedBefore := FailClosedResponseHolds()
	released := make(chan struct{})
	outcomes := make(chan responseDrainOutcome, 1)
	hold := newResponseHold(func() { close(released) })
	hold.finishHandler()
	config := testDrainConfig(time.Second, outboundQueue)
	config.abort = func(*net.TCPConn) error { return assert.AnError }
	config.duplicate = func(*net.TCPConn) (int, error) { return -1, unix.ENOTCONN }
	var monitors atomic.Int32
	config.acquireMonitor = func() bool {
		monitors.Add(1)
		return true
	}
	config.releaseMonitor = func() { monitors.Add(-1) }
	config.onComplete = func(outcome responseDrainOutcome) { outcomes <- outcome }
	config.terminateGuest = func() { t.Fatal("terminal duplication failure terminated the guest") }
	drains := []*requestDrain{{hold: hold, config: config, log: slog.New(slog.NewTextHandler(io.Discard, nil))}}

	tracked.recoverResponseConnection(drains, config, responseDrainIOError, 1, assert.AnError)

	assert.Equal(t, int32(0), monitors.Load())
	assert.Equal(t, responseDrainConnectionClosed, <-outcomes)
	select {
	case <-released:
	default:
		t.Fatal("response hold was not released")
	}
	_, err := tracked.TCPConn.Write([]byte("closed"))
	assert.ErrorIs(t, err, net.ErrClosed)
	assert.Equal(t, activeBefore, ActiveResponseHolds())
	assert.Equal(t, failedBefore, FailClosedResponseHolds())
}

func TestPersistentClosedSocketRecoveryTerminatesGuestWithinBound(t *testing.T) {
	activeBefore := ActiveResponseHolds()
	failedBefore := FailClosedResponseHolds()
	released := make(chan struct{})
	hold := newResponseHold(func() { close(released) })
	hold.finishHandler()
	config := testDrainConfig(20*time.Millisecond, outboundQueue)
	config.outboundFD = func(int) (int, error) { return 1, assert.AnError }
	config.closeState = func(int) (responseCloseState, error) { return responseClosePending, assert.AnError }
	config.abortFD = func(int) error { return assert.AnError }
	config.setUserTimeout = func(int, time.Duration) error { return assert.AnError }
	terminated := make(chan struct{})
	config.terminateGuest = func() {
		close(terminated)
		runtime.Goexit()
	}
	drains := []*requestDrain{{hold: hold, config: config, log: slog.Default()}}

	require.True(t, config.acquireMonitor())
	go monitorClosedResponse(-1, drains, config, false, assert.AnError, "")
	select {
	case <-terminated:
	case <-time.After(time.Second):
		t.Fatal("persistent closed-socket recovery did not terminate the guest")
	}
	select {
	case <-released:
		t.Fatal("scale-to-zero hold released without a drained or aborted connection")
	default:
	}
	assert.Equal(t, activeBefore+1, ActiveResponseHolds())
	assert.Equal(t, failedBefore+1, FailClosedResponseHolds())

	failClosedResponseHolds.Add(-1)
	completeDrains(drains, responseDrainConnectionClosed, 0, nil)
	assert.Equal(t, activeBefore, ActiveResponseHolds())
	assert.Equal(t, failedBefore, FailClosedResponseHolds())
}

func TestNewRequestCancelsPreviousIdleDrain(t *testing.T) {
	var requests atomic.Int32
	config := testDrainConfig(50*time.Millisecond, func(*net.TCPConn) (int, error) {
		if requests.Load() < 2 {
			return 1, nil
		}
		return 0, nil
	})
	handler := middleware(NewNoopController(), config)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := requests.Add(1)
		if request == 2 {
			time.Sleep(100 * time.Millisecond)
		}
		w.Header().Set("Content-Length", "2")
		_, _ = io.WriteString(w, "ok")
	}))
	listener, serverDone, server := serveTestHandler(t, handler)
	defer func() {
		require.NoError(t, server.Close())
		assert.ErrorIs(t, <-serverDone, http.ErrServerClosed)
	}()

	transport := &http.Transport{MaxConnsPerHost: 1}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	for i := 0; i < 2; i++ {
		response, err := client.Get("http://" + listener.Addr().String())
		require.NoError(t, err)
		body, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		assert.Equal(t, "ok", string(body))
	}
}

func TestWritesRefreshDeadlineDuringProgress(t *testing.T) {
	const bodySize = 32 << 20
	const timeout = 200 * time.Millisecond
	tests := []struct {
		name     string
		transfer func(*testing.T, *drainConn) (int64, error)
	}{
		{
			name: "write",
			transfer: func(_ *testing.T, conn *drainConn) (int64, error) {
				n, err := conn.Write(make([]byte, bodySize))
				return int64(n), err
			},
		},
		{
			name: "read_from",
			transfer: func(t *testing.T, conn *drainConn) (int64, error) {
				file, err := os.Create(filepath.Join(t.TempDir(), "response.bin"))
				require.NoError(t, err)
				defer file.Close()
				require.NoError(t, file.Truncate(bodySize))
				return conn.ReadFrom(file)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tracked, client := newTCPPair(t)
			defer client.Close()
			defer tracked.TCPConn.Close()
			require.NoError(t, tracked.SetWriteBuffer(64<<10))

			config := testDrainConfig(timeout, outboundQueue)
			var deadlines atomic.Int32
			config.setDeadline = func(conn *net.TCPConn, deadline time.Time) error {
				deadlines.Add(1)
				return conn.SetWriteDeadline(deadline)
			}
			tracked.configure(config)

			readDone := make(chan error, 1)
			go func() {
				remaining := bodySize
				buffer := make([]byte, 64<<10)
				for remaining > 0 {
					n, readErr := client.Read(buffer)
					remaining -= n
					if readErr != nil {
						readDone <- readErr
						return
					}
					time.Sleep(time.Millisecond)
				}
				readDone <- nil
			}()

			started := time.Now()
			written, err := tc.transfer(t, tracked)
			require.NoError(t, err)
			assert.Equal(t, int64(bodySize), written)
			assert.Greater(t, time.Since(started), timeout)
			require.NoError(t, <-readDone)
			assert.GreaterOrEqual(t, deadlines.Load(), int32(bodySize/responseWriteChunkSize))
		})
	}
}

func TestReadFromFlattensExistingLimits(t *testing.T) {
	source := bytes.NewReader(make([]byte, 2<<20))
	inner := &io.LimitedReader{R: source, N: 2 << 20}
	outer := &io.LimitedReader{R: inner, N: 3 << 20}
	chunk, parents, exhausted := nextReadFromChunk(outer)

	assert.False(t, exhausted)
	assert.Same(t, source, chunk.R)
	assert.Equal(t, int64(responseWriteChunkSize), chunk.N)
	require.Len(t, parents, 2)
	assert.Same(t, outer, parents[0])
	assert.Same(t, inner, parents[1])
}

func TestReadFromPreservesExistingLimit(t *testing.T) {
	const bodySize = 4 << 20
	tracked, client := newTCPPair(t)
	defer client.Close()
	defer tracked.TCPConn.Close()
	tracked.configure(testDrainConfig(time.Second, outboundQueue))
	file, err := os.Create(filepath.Join(t.TempDir(), "response.bin"))
	require.NoError(t, err)
	defer file.Close()
	require.NoError(t, file.Truncate(bodySize))
	limited := &io.LimitedReader{R: file, N: bodySize}
	readDone := make(chan error, 1)
	go func() {
		_, err := io.CopyN(io.Discard, client, bodySize)
		readDone <- err
	}()

	written, err := tracked.ReadFrom(limited)
	require.NoError(t, err)
	assert.Equal(t, int64(bodySize), written)
	assert.Zero(t, limited.N)
	require.NoError(t, <-readDone)
}

type failingSourceReader struct {
	remaining int
	err       error
}

func (r *failingSourceReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, r.err
	}
	if len(p) > r.remaining {
		p = p[:r.remaining]
	}
	r.remaining -= len(p)
	return len(p), nil
}

func TestReadFromSourceErrorClosesGracefully(t *testing.T) {
	const bodySize = 2 << 20
	sourceErr := errors.New("source failed")
	activeBefore := ActiveResponseHolds()
	tracked, client := newTCPPair(t)
	defer client.Close()
	defer tracked.TCPConn.Close()

	outcomes := make(chan responseDrainOutcome, 1)
	config := testDrainConfig(time.Second, outboundQueue)
	config.onComplete = func(outcome responseDrainOutcome) { outcomes <- outcome }
	var aborts atomic.Int32
	config.abort = func(conn *net.TCPConn) error {
		aborts.Add(1)
		return abortConnection(conn)
	}
	tracked.configure(config)
	hold := newResponseHold(func() {})
	hold.finishHandler()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	require.True(t, tracked.addDrain(&requestDrain{hold: hold, config: config, log: log}))

	readDone := make(chan struct {
		body []byte
		err  error
	}, 1)
	go func() {
		body, err := io.ReadAll(client)
		readDone <- struct {
			body []byte
			err  error
		}{body: body, err: err}
	}()

	written, err := tracked.ReadFrom(&failingSourceReader{remaining: bodySize, err: sourceErr})
	require.ErrorIs(t, err, sourceErr)
	assert.Equal(t, int64(bodySize), written)
	require.NoError(t, tracked.Close())
	result := <-readDone
	require.NoError(t, result.err)
	assert.Len(t, result.body, bodySize)
	assert.Zero(t, aborts.Load())
	assert.Equal(t, responseDrainSourceReadError, <-outcomes)
	assert.Eventually(t, func() bool { return ActiveResponseHolds() == activeBefore }, time.Second, time.Millisecond)
}

func TestReadFromErrorClassification(t *testing.T) {
	sendfileErr := &net.OpError{Op: "readfrom", Net: "tcp", Err: &os.SyscallError{Syscall: "sendfile", Err: unix.EIO}}
	assert.False(t, isReadFromWriteError(nil, sendfileErr))
	assert.True(t, isReadFromWriteError(bytes.NewReader(nil), os.ErrDeadlineExceeded))

	source, peer := net.Pipe()
	defer source.Close()
	defer peer.Close()
	assert.False(t, isReadFromWriteError(source, os.ErrDeadlineExceeded))
}

func TestCloseMonitorLimitBoundsResources(t *testing.T) {
	const monitorLimit = 4
	const connectionCount = 12
	activeHoldsBefore := ActiveResponseHolds()
	activeMonitorsBefore := ActiveResponseCloseMonitors()
	rejectionsBefore := ResponseCloseMonitorRejections()
	slots := make(chan struct{}, monitorLimit)
	config := testDrainConfig(100*time.Millisecond, outboundQueue)
	config.acquireMonitor = func() bool {
		select {
		case slots <- struct{}{}:
			return true
		default:
			return false
		}
	}
	config.releaseMonitor = func() { <-slots }
	config.setUserTimeout = func(int, time.Duration) error { return nil }
	config.outboundFD = func(int) (int, error) { return 1, nil }
	config.closeState = func(int) (responseCloseState, error) { return responseClosePending, nil }
	outcomes := make(chan responseDrainOutcome, connectionCount)
	config.onComplete = func(outcome responseDrainOutcome) { outcomes <- outcome }
	var aborts atomic.Int32
	config.abort = func(conn *net.TCPConn) error {
		aborts.Add(1)
		return abortConnection(conn)
	}

	type connectionPair struct {
		tracked *drainConn
		client  net.Conn
	}
	pairs := make([]connectionPair, 0, connectionCount)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for range connectionCount {
		tracked, client := newTCPPair(t)
		hold := newResponseHold(func() {})
		hold.finishHandler()
		require.True(t, tracked.addDrain(&requestDrain{hold: hold, config: config, log: log}))
		pairs = append(pairs, connectionPair{tracked: tracked, client: client})
	}
	for _, pair := range pairs {
		require.NoError(t, pair.tracked.Close())
	}

	require.Eventually(t, func() bool {
		return ActiveResponseCloseMonitors() == activeMonitorsBefore+monitorLimit
	}, time.Second, time.Millisecond)
	assert.Equal(t, int32(connectionCount-monitorLimit), aborts.Load())
	assert.Equal(t, rejectionsBefore+connectionCount-monitorLimit, ResponseCloseMonitorRejections())
	assert.Equal(t, activeHoldsBefore+monitorLimit, ActiveResponseHolds())
	assert.Len(t, slots, monitorLimit)

	for _, pair := range pairs {
		require.NoError(t, pair.client.Close())
	}
	require.Eventually(t, func() bool {
		return ActiveResponseCloseMonitors() == activeMonitorsBefore && ActiveResponseHolds() == activeHoldsBefore
	}, time.Second, time.Millisecond)
	assert.Empty(t, slots)
	counts := make(map[responseDrainOutcome]int)
	for range connectionCount {
		counts[<-outcomes]++
	}
	assert.Equal(t, connectionCount-monitorLimit, counts[responseDrainMonitorLimit])
	assert.Equal(t, monitorLimit, counts[responseDrainTimeoutHit])
}

func TestPipelinedRequestsKeepOnePendingHold(t *testing.T) {
	const requestCount = 1000
	activeBefore := ActiveResponseHolds()
	base := &mockScaleToZeroer{}
	ctrl := NewDebouncedController(base)
	var allowDrain atomic.Bool
	config := testDrainConfig(time.Second, func(*net.TCPConn) (int, error) {
		if allowDrain.Load() {
			return 0, nil
		}
		return 1, nil
	})
	trackedConn := make(chan *drainConn, 1)
	handler := middleware(ctrl, config)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case trackedConn <- r.Context().Value(connectionContextKey{}).(*drainConn):
		default:
		}
		w.Header().Set("Content-Length", "1")
		_, _ = io.WriteString(w, "x")
	}))
	listener, serverDone, server := serveTestHandler(t, handler)
	defer func() {
		require.NoError(t, server.Close())
		assert.ErrorIs(t, <-serverDone, http.ErrServerClosed)
	}()

	conn, err := net.Dial("tcp", listener.Addr().String())
	require.NoError(t, err)
	defer conn.Close()
	requests := bytes.NewBuffer(make([]byte, 0, requestCount*32))
	for range requestCount {
		_, _ = fmt.Fprint(requests, "GET / HTTP/1.1\r\nHost: test\r\n\r\n")
	}
	_, err = conn.Write(requests.Bytes())
	require.NoError(t, err)
	reader := bufio.NewReader(conn)
	for range requestCount {
		response, readErr := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
		require.NoError(t, readErr)
		body, readErr := io.ReadAll(response.Body)
		require.NoError(t, readErr)
		require.NoError(t, response.Body.Close())
		assert.Equal(t, "x", string(body))
	}

	tracked := <-trackedConn
	tracked.mu.Lock()
	pending := len(tracked.pending)
	tracked.mu.Unlock()
	assert.Equal(t, 1, pending)
	assert.Equal(t, activeBefore+1, ActiveResponseHolds())
	base.mu.Lock()
	assert.Equal(t, 1, base.disableCalls)
	assert.Equal(t, 0, base.enableCalls)
	base.mu.Unlock()

	allowDrain.Store(true)
	require.Eventually(t, func() bool {
		base.mu.Lock()
		defer base.mu.Unlock()
		return base.enableCalls == 1 && ActiveResponseHolds() == activeBefore
	}, time.Second, time.Millisecond)
	tracked.mu.Lock()
	assert.Empty(t, tracked.pending)
	tracked.mu.Unlock()
}

func TestKeepAliveResponsesDoNotWaitForMaximumPollInterval(t *testing.T) {
	config := testDrainConfig(time.Second, outboundQueue)
	config.maxPollInterval = responseDrainMaxPollInterval
	handler := middleware(NewNoopController(), config)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "2")
		_, _ = io.WriteString(w, "ok")
	}))
	listener, serverDone, server := serveTestHandler(t, handler)
	defer func() {
		require.NoError(t, server.Close())
		assert.ErrorIs(t, <-serverDone, http.ErrServerClosed)
	}()

	transport := &http.Transport{MaxConnsPerHost: 1}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	started := time.Now()
	for i := 0; i < 100; i++ {
		response, err := client.Get("http://" + listener.Addr().String())
		require.NoError(t, err)
		_, err = io.Copy(io.Discard, response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
	}
	assert.Less(t, time.Since(started), 5*time.Second)
}
