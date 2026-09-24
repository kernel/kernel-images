package pagerecovery

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func navigation(status int) pausedResponse {
	return pausedResponse{
		method:       "GET",
		status:       status,
		isTopLevel:   true,
		isNavigation: true,
	}
}

func TestRetryableStatuses(t *testing.T) {
	for _, status := range []int{408, 429, 502, 503, 504, 507} {
		assert.True(t, navigation(status).retryable(), "status %d should be replayed", status)
	}
	for _, status := range []int{200, 204, 301, 400, 401, 403, 404, 410, 418, 500} {
		assert.False(t, navigation(status).retryable(), "status %d should be passed through", status)
	}
}

func TestOnlyTopLevelGetNavigationsReplay(t *testing.T) {
	base := navigation(429)

	subresource := base
	subresource.isNavigation = false
	assert.False(t, subresource.retryable())

	iframe := base
	iframe.isTopLevel = false
	assert.False(t, iframe.retryable())

	// The replay is a 307, which resends the body: a refused POST may still
	// have been recorded upstream.
	post := base
	post.method = "POST"
	assert.False(t, post.retryable())
}

func TestBrandedProxyErrorIsNotReplayed(t *testing.T) {
	response := navigation(502)
	response.headers = map[string]string{proxyErrorHeader: "upstream_unreachable"}
	assert.False(t, response.retryable(), "a Kernel proxy error is a Kernel signal, not a site refusal")
}

func TestTransientNetworkErrorsReplay(t *testing.T) {
	response := navigation(0)
	response.errorReason = "ConnectionReset"
	assert.True(t, response.retryable())

	response.errorReason = "BlockedByClient"
	assert.False(t, response.retryable())
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

	seconds := navigation(429)
	seconds.headers = map[string]string{"retry-after": "3"}
	delay, ok := seconds.retryAfter(now)
	require.True(t, ok)
	assert.Equal(t, 3*time.Second, delay)

	date := navigation(429)
	date.headers = map[string]string{"retry-after": now.Add(90 * time.Second).Format(http.TimeFormat)}
	delay, ok = date.retryAfter(now)
	require.True(t, ok)
	assert.Equal(t, 90*time.Second, delay)

	// A date already past asks for no wait, which is different from asking for
	// nothing: the caller still honours the budget either way.
	past := navigation(429)
	past.headers = map[string]string{"retry-after": now.Add(-time.Minute).Format(http.TimeFormat)}
	delay, ok = past.retryAfter(now)
	require.True(t, ok)
	assert.Zero(t, delay)

	for _, raw := range []string{"", "soon", "-5", "3.5"} {
		malformed := navigation(429)
		malformed.headers = map[string]string{"retry-after": raw}
		_, ok := malformed.retryAfter(now)
		assert.False(t, ok, "%q should read as absent, not as zero", raw)
	}
}
