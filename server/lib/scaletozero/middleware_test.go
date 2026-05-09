package scaletozero

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mockController struct {
	mu           sync.Mutex
	acquireCalls int
	releaseCalls int
	acquireErr   error
	releaseErr   error
}

func (m *mockController) Acquire(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.acquireCalls++
	return m.acquireErr
}

func (m *mockController) Release(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.releaseCalls++
	return m.releaseErr
}

func (m *mockController) Disable(ctx context.Context) error { return nil }
func (m *mockController) Enable(ctx context.Context) error  { return nil }

func TestMiddlewareAcquiresAndReleasesForExternalAddr(t *testing.T) {
	t.Parallel()
	mock := &mockController{}
	handler := Middleware(mock)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.50:12345"
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, 1, mock.acquireCalls)
	assert.Equal(t, 1, mock.releaseCalls)
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
			mock := &mockController{}
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
			assert.Equal(t, 0, mock.acquireCalls, "should not acquire for loopback addr")
			assert.Equal(t, 0, mock.releaseCalls, "should not release for loopback addr")
		})
	}
}

func TestMiddlewareAcquireError(t *testing.T) {
	t.Parallel()
	mock := &mockController{acquireErr: assert.AnError}
	var called bool
	handler := Middleware(mock)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.50:12345"
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assert.False(t, called, "handler should not be called on acquire error")
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, 0, mock.releaseCalls)
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
