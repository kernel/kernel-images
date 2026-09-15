package metrics

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNetworkCollectorZerosAndIndependentChromeFailure(t *testing.T) {
	c := NewNetworkCollector(func() (uint64, uint64, bool) { return 0, 0, false })
	w := &Writer{}
	require.NoError(t, c.Collect(context.Background(), w))
	for _, name := range []string{"kernel_chromium_connection_resets_total", "kernel_chromium_network_requests_completed_total", "kernel_chromium_network_monitor_up"} {
		require.Contains(t, string(w.Bytes()), name+" 0\n")
	}
	require.NotContains(t, string(w.Bytes()), "{")
	srv := fakeCDP(t)
	defer srv.Close()
	chrome := NewChromeCollector(staticUpstream("ws" + strings.TrimPrefix(srv.URL, "http")))
	chrome.histograms = []UMAHistogram{{Name: "Fail.Me"}}
	require.Error(t, chrome.Collect(context.Background(), &Writer{}))
	h := Handler(slog.Default(), chrome, c)
	for range 2 {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", nil))
		require.Contains(t, rr.Body.String(), string(w.Bytes()))
		require.NotContains(t, rr.Body.String(), "kernel_chromium_up")
	}
}
