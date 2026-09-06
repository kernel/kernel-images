package scaletozero

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
