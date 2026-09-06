package scaletozero

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMiddlewareDisablesUntilResponseFinalization(t *testing.T) {
	base := newSignalController()
	ctrl := NewDebouncedController(base)
	tracked, client := newTCPPair(t)
	defer client.Close()
	defer tracked.TCPConn.Close()

	handler := middleware(ctrl, testDrainConfig(time.Second, func(*net.TCPConn) (int, error) {
		return 0, nil
	}))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := externalRequest(http.MethodGet, "/json/version", tracked)

	handler.ServeHTTP(httptest.NewRecorder(), req)
	select {
	case <-base.enabled:
		t.Fatal("scale-to-zero enabled before response finalization")
	default:
	}

	tracked.startIdleDrain()
	select {
	case <-base.enabled:
	case <-time.After(time.Second):
		t.Fatal("scale-to-zero was not enabled after response drain")
	}
}

func TestMiddlewareSkipsLoopbackAddrs(t *testing.T) {
	t.Parallel()

	loopbackAddrs := []string{"127.0.0.1:8080", "[::1]:8080"}
	for _, addr := range loopbackAddrs {
		t.Run(addr, func(t *testing.T) {
			t.Parallel()
			mock := &mockScaleToZeroer{}
			var called bool
			handler := Middleware(mock)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				w.WriteHeader(http.StatusOK)
			}))

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = addr
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)

			assert.True(t, called)
			assert.Equal(t, http.StatusOK, recorder.Code)
			assert.Equal(t, 0, mock.disableCalls)
			assert.Equal(t, 0, mock.enableCalls)
		})
	}
}

func TestMiddlewareDisableErrorAbortsConnection(t *testing.T) {
	t.Parallel()
	mock := &mockScaleToZeroer{disableErr: assert.AnError}
	tracked, client := newTCPPair(t)
	defer client.Close()
	defer tracked.TCPConn.Close()
	var called bool
	handler := Middleware(mock)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))

	assert.PanicsWithValue(t, http.ErrAbortHandler, func() {
		handler.ServeHTTP(httptest.NewRecorder(), externalRequest(http.MethodGet, "/", tracked))
	})

	assert.False(t, called)
	assert.True(t, tracked.closed)
	assert.Equal(t, 0, mock.enableCalls)
}

func TestMiddlewareRejectsUntrackedConnection(t *testing.T) {
	ctrl := &mockScaleToZeroer{}
	var called bool
	handler := Middleware(ctrl)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	req := httptest.NewRequest(http.MethodGet, "/fs/read_file", nil)
	req.RemoteAddr = "192.0.2.1:1234"

	assert.PanicsWithValue(t, http.ErrAbortHandler, func() {
		handler.ServeHTTP(httptest.NewRecorder(), req)
	})

	assert.False(t, called)
	assert.Equal(t, 0, ctrl.disableCalls)
	assert.Equal(t, 0, ctrl.enableCalls)
}

func TestMiddlewareWaitsForTCPResponseDrain(t *testing.T) {
	const bodySize = 2 << 20

	type handlerResult struct {
		at  time.Time
		err error
	}

	base := newSignalController()
	ctrl := NewDebouncedController(base)
	handlerDone := make(chan handlerResult, 1)
	var sawQueued bool
	var queueEmptyAt time.Time
	var queueEmptyOnce sync.Once
	drain := testDrainConfig(5*time.Second, func(conn *net.TCPConn) (int, error) {
		queued, err := outboundQueue(conn)
		if queued > 0 {
			sawQueued = true
		} else if err == nil {
			queueEmptyOnce.Do(func() { queueEmptyAt = time.Now() })
		}
		return queued, err
	})
	handler := middleware(ctrl, drain)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(bodySize))
		_, err := io.Copy(w, bytes.NewReader(make([]byte, bodySize)))
		handlerDone <- handlerResult{at: time.Now(), err: err}
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
	_, err = fmt.Fprint(conn, "GET /fs/read_file HTTP/1.1\r\nHost: test\r\n\r\n")
	require.NoError(t, err)

	response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodGet})
	require.NoError(t, err)
	defer response.Body.Close()

	buffer := make([]byte, 4<<10)
	read := 0
	for read < bodySize {
		n, readErr := response.Body.Read(buffer)
		read += n
		if readErr != nil {
			require.ErrorIs(t, readErr, io.EOF)
			break
		}
		time.Sleep(time.Millisecond)
	}

	enabledAt := <-base.enabled
	result := <-handlerDone
	require.NoError(t, result.err)
	assert.True(t, sawQueued)
	assert.False(t, queueEmptyAt.IsZero())
	assert.False(t, enabledAt.Before(queueEmptyAt))
}

func TestMiddlewareDrainsAfterChunkedResponseFinalization(t *testing.T) {
	base := newSignalController()
	ctrl := NewDebouncedController(base)
	handlerErr := make(chan error, 1)
	handler := middleware(ctrl, testDrainConfig(time.Second, outboundQueue))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := w.Write(bytes.Repeat([]byte("x"), 64<<10))
		handlerErr <- err
	}))

	listener, serverDone, server := serveTestHandler(t, handler)
	defer func() {
		require.NoError(t, server.Close())
		assert.ErrorIs(t, <-serverDone, http.ErrServerClosed)
	}()

	conn, err := net.Dial("tcp", listener.Addr().String())
	require.NoError(t, err)
	defer conn.Close()
	_, err = fmt.Fprint(conn, "GET /stream HTTP/1.1\r\nHost: test\r\nConnection: close\r\n\r\n")
	require.NoError(t, err)

	response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodGet})
	require.NoError(t, err)
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	assert.Equal(t, []string{"chunked"}, response.TransferEncoding)
	assert.Len(t, body, 64<<10)
	require.NoError(t, <-handlerErr)
	select {
	case <-base.enabled:
	case <-time.After(time.Second):
		t.Fatal("scale-to-zero was not enabled after chunked response drained")
	}
}

func TestMiddlewareResponseDrainTimeoutAbortsConnection(t *testing.T) {
	base := newSignalController()
	ctrl := NewDebouncedController(base)
	const ceiling = 50 * time.Millisecond
	var aborted bool
	drain := testDrainConfig(ceiling, func(*net.TCPConn) (int, error) { return 1, nil })
	drain.abort = func(conn *net.TCPConn) error {
		aborted = true
		return abortConnection(conn)
	}
	handler := middleware(ctrl, drain)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "2")
		_, _ = io.WriteString(w, "ok")
	}))

	listener, serverDone, server := serveTestHandler(t, handler)
	defer func() {
		require.NoError(t, server.Close())
		assert.ErrorIs(t, <-serverDone, http.ErrServerClosed)
	}()

	conn, err := net.Dial("tcp", listener.Addr().String())
	require.NoError(t, err)
	defer conn.Close()
	started := time.Now()
	_, err = fmt.Fprint(conn, "GET /anything HTTP/1.1\r\nHost: test\r\n\r\n")
	require.NoError(t, err)

	select {
	case enabledAt := <-base.enabled:
		assert.GreaterOrEqual(t, enabledAt.Sub(started), ceiling)
		assert.Less(t, enabledAt.Sub(started), 3*ceiling)
	case <-time.After(time.Second):
		t.Fatal("scale-to-zero hold was not released after abort")
	}
	assert.True(t, aborted)
}

func TestMiddlewareIOErrorAbortsConnection(t *testing.T) {
	base := newSignalController()
	ctrl := NewDebouncedController(base)
	var aborted bool
	drain := testDrainConfig(time.Second, func(*net.TCPConn) (int, error) {
		return 1, assert.AnError
	})
	drain.abort = func(conn *net.TCPConn) error {
		aborted = true
		return abortConnection(conn)
	}
	handler := middleware(ctrl, drain)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "response")
	}))

	listener, serverDone, server := serveTestHandler(t, handler)
	defer func() {
		require.NoError(t, server.Close())
		assert.ErrorIs(t, <-serverDone, http.ErrServerClosed)
	}()
	conn, err := net.Dial("tcp", listener.Addr().String())
	require.NoError(t, err)
	defer conn.Close()
	_, err = fmt.Fprint(conn, "GET /anything HTTP/1.1\r\nHost: test\r\n\r\n")
	require.NoError(t, err)

	select {
	case <-base.enabled:
	case <-time.After(time.Second):
		t.Fatal("scale-to-zero hold was not released after abort")
	}
	assert.True(t, aborted)
}

func TestMiddlewareRetriesAbortBeforeReleasingHold(t *testing.T) {
	failClosedBefore := FailClosedResponseHolds()
	base := newSignalController()
	ctrl := NewDebouncedController(base)
	drain := testDrainConfig(time.Second, func(*net.TCPConn) (int, error) {
		return 1, assert.AnError
	})
	drain.abortRetryInterval = 50 * time.Millisecond
	firstAttempt := make(chan struct{})
	attempts := 0
	drain.abort = func(conn *net.TCPConn) error {
		attempts++
		if attempts == 1 {
			close(firstAttempt)
			return assert.AnError
		}
		return abortConnection(conn)
	}
	handler := middleware(ctrl, drain)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "response")
	}))

	listener, serverDone, server := serveTestHandler(t, handler)
	defer func() {
		require.NoError(t, server.Close())
		assert.ErrorIs(t, <-serverDone, http.ErrServerClosed)
	}()
	conn, err := net.Dial("tcp", listener.Addr().String())
	require.NoError(t, err)
	defer conn.Close()
	_, err = fmt.Fprint(conn, "GET /anything HTTP/1.1\r\nHost: test\r\n\r\n")
	require.NoError(t, err)
	<-firstAttempt
	select {
	case <-base.enabled:
		t.Fatal("scale-to-zero enabled before connection recovery")
	default:
	}

	select {
	case <-base.enabled:
	case <-time.After(time.Second):
		t.Fatal("scale-to-zero hold was not released after abort retry succeeded")
	}
	assert.GreaterOrEqual(t, attempts, 2)
	assert.Eventually(t, func() bool {
		return FailClosedResponseHolds() == failClosedBefore
	}, time.Second, time.Millisecond)
}

func TestMiddlewareWriteDeadlineCoversHandler(t *testing.T) {
	base := newSignalController()
	ctrl := NewDebouncedController(base)
	const ceiling = 50 * time.Millisecond
	handlerDone := make(chan error, 1)
	handler := middleware(ctrl, testDrainConfig(ceiling, outboundQueue))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.Copy(w, bytes.NewReader(make([]byte, 32<<20)))
		handlerDone <- err
	}))

	listener, serverDone, server := serveTestHandler(t, handler)
	defer func() {
		require.NoError(t, server.Close())
		assert.ErrorIs(t, <-serverDone, http.ErrServerClosed)
	}()
	conn, err := net.Dial("tcp", listener.Addr().String())
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.(*net.TCPConn).SetReadBuffer(4<<10))
	_, err = fmt.Fprint(conn, "GET /large HTTP/1.1\r\nHost: test\r\n\r\n")
	require.NoError(t, err)

	select {
	case err := <-handlerDone:
		require.Error(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("handler write was not bounded by the response deadline")
	}
	select {
	case <-base.enabled:
	case <-time.After(time.Second):
		t.Fatal("scale-to-zero hold was not released after timed-out connection was aborted")
	}
}

func TestMiddlewareClearsWriteDeadlineAfterDrain(t *testing.T) {
	base := newSignalController()
	ctrl := NewDebouncedController(base)
	tracked, client := newTCPPair(t)
	defer client.Close()
	defer tracked.TCPConn.Close()

	var deadlines []time.Time
	drain := testDrainConfig(time.Second, func(*net.TCPConn) (int, error) { return 0, nil })
	drain.setDeadline = func(_ *net.TCPConn, deadline time.Time) error {
		deadlines = append(deadlines, deadline)
		return nil
	}
	handler := middleware(ctrl, drain)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		_, err := tracked.Write([]byte("response"))
		require.NoError(t, err)
	}))

	handler.ServeHTTP(httptest.NewRecorder(), externalRequest(http.MethodGet, "/", tracked))
	tracked.startIdleDrain()
	<-base.enabled

	require.Len(t, deadlines, 2)
	assert.False(t, deadlines[0].IsZero())
	assert.True(t, deadlines[1].IsZero())
}

func TestDrainConnClearsWriteDeadlineWhenHijacked(t *testing.T) {
	tracked, client := newTCPPair(t)
	defer client.Close()
	defer tracked.TCPConn.Close()

	var deadlines []time.Time
	config := testDrainConfig(time.Second, outboundQueue)
	config.setDeadline = func(_ *net.TCPConn, deadline time.Time) error {
		deadlines = append(deadlines, deadline)
		return nil
	}
	tracked.configure(config)
	tracked.hijack()
	_, err := tracked.Write([]byte("frame"))
	require.NoError(t, err)

	require.Len(t, deadlines, 1)
	assert.True(t, deadlines[0].IsZero())
}

func TestMiddlewareDoesNotReleaseBeforeClosedHandlerReturns(t *testing.T) {
	base := newSignalController()
	ctrl := NewDebouncedController(base)
	tracked, client := newTCPPair(t)
	defer client.Close()
	defer tracked.TCPConn.Close()
	handler := middleware(ctrl, testDrainConfig(time.Second, outboundQueue))(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		tracked.abortNow(abortConnection)
		select {
		case <-base.enabled:
			t.Fatal("scale-to-zero enabled while handler was still running")
		default:
		}
	}))

	handler.ServeHTTP(httptest.NewRecorder(), externalRequest(http.MethodGet, "/", tracked))
	select {
	case <-base.enabled:
	case <-time.After(time.Second):
		t.Fatal("scale-to-zero was not enabled after handler returned")
	}
}

func TestMiddlewareWriteDeadlineFailureAbortsConnection(t *testing.T) {
	ctrl := &mockScaleToZeroer{}
	tracked, client := newTCPPair(t)
	defer client.Close()
	defer tracked.TCPConn.Close()
	var aborted bool
	drain := testDrainConfig(time.Second, outboundQueue)
	drain.setDeadline = func(*net.TCPConn, time.Time) error { return assert.AnError }
	drain.abort = func(conn *net.TCPConn) error {
		aborted = true
		return abortConnection(conn)
	}
	handler := middleware(ctrl, drain)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		_, _ = tracked.Write([]byte("response"))
	}))

	handler.ServeHTTP(httptest.NewRecorder(), externalRequest(http.MethodGet, "/", tracked))

	assert.True(t, aborted)
	assert.Equal(t, 1, ctrl.disableCalls)
	assert.Equal(t, 1, ctrl.enableCalls)
}

func TestWaitForResponseDrainCompletesImmediatelyForEmptyQueue(t *testing.T) {
	outcome, queued, err := waitForResponseDrain(context.Background(), nil, responseDrainConfig{
		initialPollInterval: time.Millisecond,
		maxPollInterval:     time.Millisecond,
		outbound:            func(*net.TCPConn) (int, error) { return 0, nil },
	}, time.Now().Add(time.Second))

	assert.Equal(t, responseDrainComplete, outcome)
	assert.Zero(t, queued)
	assert.NoError(t, err)
}

func TestWaitForResponseDrainReportsIOError(t *testing.T) {
	wantErr := assert.AnError
	outcome, _, err := waitForResponseDrain(context.Background(), nil, responseDrainConfig{
		initialPollInterval: time.Millisecond,
		maxPollInterval:     time.Millisecond,
		outbound:            func(*net.TCPConn) (int, error) { return 0, wantErr },
	}, time.Now().Add(time.Second))

	assert.Equal(t, responseDrainIOError, outcome)
	assert.ErrorIs(t, err, wantErr)
}

func TestIsLoopbackAddr(t *testing.T) {
	t.Parallel()

	tests := []struct {
		addr     string
		loopback bool
	}{
		{"127.0.0.1:80", true},
		{"[::1]:8080", true},
		{"203.0.113.50:80", false},
		{"2001:db8::1", false},
		{"not-an-ip:80", false},
		{"", false},
	}

	for _, tc := range tests {
		t.Run(tc.addr, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.loopback, isLoopbackAddr(tc.addr))
		})
	}
}

func testDrainConfig(timeout time.Duration, outbound func(*net.TCPConn) (int, error)) responseDrainConfig {
	return responseDrainConfig{
		initialPollInterval: time.Millisecond,
		maxPollInterval:     time.Millisecond,
		timeout:             timeout,
		outbound:            outbound,
		setDeadline:         setWriteDeadline,
		abort:               abortConnection,
		abortRetryInterval:  time.Millisecond,
	}
}

func externalRequest(method, path string, conn *drainConn) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = "192.0.2.1:1234"
	return req.WithContext(connectionContext(req.Context(), conn))
}

func newTCPPair(t *testing.T) (*drainConn, net.Conn) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	accepted := make(chan *net.TCPConn, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			accepted <- conn.(*net.TCPConn)
		}
	}()
	client, err := net.Dial("tcp", listener.Addr().String())
	require.NoError(t, err)
	server := <-accepted
	require.NoError(t, listener.Close())
	return &drainConn{TCPConn: server, state: http.StateActive}, client
}

func serveTestHandler(t *testing.T, handler http.Handler) (net.Listener, <-chan error, *http.Server) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.RemoteAddr = "192.0.2.1:1234"
			handler.ServeHTTP(w, r)
		}),
	}
	serverDone := make(chan error, 1)
	go func() { serverDone <- Serve(server, listener) }()
	return listener, serverDone, server
}

type signalController struct {
	enabled chan time.Time
	once    sync.Once
}

func newSignalController() *signalController {
	return &signalController{enabled: make(chan time.Time, 1)}
}

func (*signalController) Disable(context.Context) error { return nil }

func (c *signalController) Enable(context.Context) error {
	c.once.Do(func() { c.enabled <- time.Now() })
	return nil
}
