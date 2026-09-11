package cdpmonitor

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNetworkCounters(t *testing.T) {
	c := newNetworkCounters()
	require.Equal(t, NetworkSnapshot{}, c.snapshot())
	for i, errorText := range []string{"net::ERR_CONNECTION_RESET", "net::ERR_CONNECTION_REFUSED", "net::ERR_ABORTED", "ERR_CONNECTION_RESET", "net::ERR_CONNECTION_RESET extra", ""} {
		c.terminal("session", fmt.Sprint(i), errorText)
		c.terminal("session", fmt.Sprint(i), errorText)
	}
	require.Equal(t, uint64(1), c.snapshot().Resets)
	require.Equal(t, uint64(6), c.snapshot().Completed)
	// Unknown starts count, and colliding request IDs in different sessions don't deduplicate.
	c.terminal("worker", "0", "net::ERR_CONNECTION_RESET")
	require.Equal(t, uint64(2), c.snapshot().Resets)
	c.newGeneration()
	c.terminal("session", "0", "net::ERR_CONNECTION_RESET")
	require.Equal(t, uint64(3), c.snapshot().Resets)
	require.Equal(t, uint64(8), c.snapshot().Completed)
	c.terminal("s", "first-wins", "")
	c.terminal("s", "first-wins", "net::ERR_CONNECTION_RESET")
	require.Equal(t, uint64(3), c.snapshot().Resets)
}

func TestNetworkCountersBounded(t *testing.T) {
	c := newNetworkCounters()
	for i := range terminalHistorySize + 100 {
		c.terminal("s", fmt.Sprint(i), "")
	}
	require.Len(t, c.seen, terminalHistorySize)
	// FIFO eviction intentionally ends the deduplication guarantee.
	c.terminal("s", "0", "")
	require.Equal(t, uint64(terminalHistorySize+101), c.snapshot().Completed)
	c.newGeneration()
	require.Empty(t, c.seen)
	before := c.snapshot()
	for _, key := range []networkRequestKey{{"", "r"}, {"s", ""}, {strings.Repeat("s", 257), "r"}, {"s", strings.Repeat("r", 257)}} {
		c.terminal(key.sessionID, key.requestID, "net::ERR_CONNECTION_RESET")
	}
	require.Equal(t, before, c.snapshot())
}

func TestNetworkCountersConcurrent(t *testing.T) {
	c := newNetworkCounters()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for i := range 1000 {
				c.terminal("s", fmt.Sprint(i), "net::ERR_CONNECTION_RESET")
				s := c.snapshot()
				if s.Resets != s.Completed {
					t.Errorf("inconsistent counter snapshot: %+v", s)
					return
				}
			}
		})
	}
	wg.Wait()
	require.Equal(t, uint64(1000), c.snapshot().Completed)
}

func TestMetricsOnlyDispatch(t *testing.T) {
	m := New(newTestUpstream(""), newEventCollector().publishFn(), 0, discardLogger, nil)
	require.NoError(t, m.SetTelemetry(false))
	for range 3 { // redirect starts do not affect terminal counters
		m.dispatchEvent(cdpMessage{Method: "Network.requestWillBeSent", SessionID: "s", Params: []byte(`{"requestId":"r","redirectResponse":{}}`)})
	}
	for range 2 {
		m.dispatchEvent(cdpMessage{Method: "Network.loadingFailed", SessionID: "s", Params: []byte(`{"requestId":"r","errorText":"net::ERR_CONNECTION_RESET"}`)})
	}
	m.dispatchEvent(cdpMessage{Method: "Network.loadingFinished", SessionID: "s", Params: []byte(`{"requestId":"unknown"}`)})
	require.Equal(t, NetworkSnapshot{Resets: 1, Completed: 2}, m.NetworkSnapshot())
	require.Empty(t, m.pendingRequests)
	require.Empty(t, m.computedStates)
}
