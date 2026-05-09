package scaletozero

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMiddlewareDisablesAndEnablesForExternalAddr(t *testing.T) {
	t.Parallel()
	mock := &mockScaleToZeroer{}
	handler := Middleware(mock)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.50:12345"
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, 1, mock.disableCalls)
	assert.Equal(t, 1, mock.enableCalls)
}

func TestMiddlewareSkipsLoopbackAddrs(t *testing.T) {
	t.Parallel()

	loopbackAddrs := []struct {
		name string
		addr string
	}{
		{"loopback-v4", "127.0.0.1:8080"},
		{"loopback-v6", "[::1]:8080"},
	}

	for _, tc := range loopbackAddrs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mock := &mockScaleToZeroer{}
			var called bool
			handler := Middleware(mock)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				w.WriteHeader(http.StatusOK)
			}))

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = tc.addr
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			assert.True(t, called, "handler should still be called")
			assert.Equal(t, http.StatusOK, rec.Code)
			assert.Equal(t, 0, mock.disableCalls, "should not disable for loopback addr")
			assert.Equal(t, 0, mock.enableCalls, "should not enable for loopback addr")
		})
	}
}

func TestMiddlewareDisableError(t *testing.T) {
	t.Parallel()
	mock := &mockScaleToZeroer{disableErr: assert.AnError}
	var called bool
	handler := Middleware(mock)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.50:12345"
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assert.False(t, called, "handler should not be called on disable error")
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, 0, mock.enableCalls)
}

func TestIsLoopbackAddr(t *testing.T) {
	t.Parallel()

	tests := []struct {
		addr     string
		loopback bool
	}{
		// Loopback
		{"127.0.0.1:80", true},
		{"[::1]:80", true},
		{"127.0.0.1", true},
		{"::1", true},
		// Non-loopback
		{"10.0.0.1:80", false},
		{"172.16.0.1:80", false},
		{"192.168.1.1:80", false},
		{"203.0.113.50:80", false},
		{"8.8.8.8:53", false},
		{"[2001:db8::1]:80", false},
		// Unparseable
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
