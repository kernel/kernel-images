package cdpmonitor

import (
	"fmt"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/kernel/kernel-images/server/lib/metrics"
	"github.com/stretchr/testify/require"
)

func TestNetworkConcurrentScrapesAndEvents(t *testing.T) {
	m := New(newTestUpstream(""), newEventCollector().publishFn(), 0, discardLogger, nil)
	h := metrics.Handler(discardLogger, metrics.NewNetworkCollector(func() (uint64, uint64, bool, map[string][2]uint64) {
		s := m.NetworkSnapshot()
		return s.Resets, s.Completed, s.Up, s.Failures
	}))
	scrape := func() {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", nil))
		var total, completed, resets, resetBucket uint64
		series := 0
		for _, line := range strings.Split(rr.Body.String(), "\n") {
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			fields := strings.Fields(line)
			value, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				t.Error(err)
				return
			}
			switch {
			case fields[0] == "kernel_chromium_connection_resets_total":
				resets = value
			case fields[0] == "kernel_chromium_network_requests_completed_total":
				completed = value
			case strings.HasPrefix(fields[0], "kernel_chromium_network_failures_total{"):
				series++
				total += value
				if strings.Contains(fields[0], `error_code="ERR_CONNECTION_RESET"`) {
					resetBucket += value
				}
			}
		}
		if series != 170 || total > completed || resets != resetBucket || total != completed {
			t.Errorf("inconsistent scrape: series=%d failures=%d completed=%d resets=%d resetBucket=%d", series, total, completed, resets, resetBucket)
		}
	}
	scrape() // All 170 series are present at zero before the first failure.
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for i := range 1000 {
				m.dispatchEvent(cdpMessage{Method: "Network.loadingFailed", SessionID: "s", Params: []byte(fmt.Sprintf(`{"requestId":"%d","errorText":"net::ERR_CONNECTION_RESET","canceled":%t}`, i, i%2 == 0))})
			}
		})
	}
	for range 2 {
		wg.Go(func() {
			for range 50 {
				scrape()
			}
		})
	}
	wg.Wait()
	scrape()
	assertNetworkTotals(t, m.NetworkSnapshot(), 1000, 1000)
	require.Equal(t, [2]uint64{500, 500}, m.NetworkSnapshot().Failures["ERR_CONNECTION_RESET"])
}
