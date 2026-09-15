package cdpmonitor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kernel/kernel-images/server/lib/events"
	"github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/require"
)

type countedUpstream struct {
	*testUpstream
	reads atomic.Int32
}

func (u *countedUpstream) Current() string {
	u.reads.Add(1)
	return u.testUpstream.Current()
}

func TestInitialAcquisitionIsNotReconnection(t *testing.T) {
	for _, start := range []string{"no_url", "dial_failure"} {
		t.Run(start, func(t *testing.T) {
			t.Parallel()
			srv := newTestServer(t)
			defer srv.close()
			stop := make(chan struct{})
			defer close(stop)
			var probes atomic.Int32
			go listenAndRespond(srv, stop, func(msg cdpMessage) any {
				if msg.Method == "Browser.getVersion" {
					probes.Add(1)
				}
				return nil
			})
			var failures atomic.Int32
			unavailable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				failures.Add(1)
				http.Error(w, "fixture unavailable", http.StatusServiceUnavailable)
			}))
			defer unavailable.Close()
			badURL := "ws" + strings.TrimPrefix(unavailable.URL, "http")
			u := &countedUpstream{testUpstream: newTestUpstream("")}
			ec := newEventCollector()
			m := New(u, ec.publishFn(), 0, discardLogger, nil)
			require.NoError(t, m.SetTelemetry(false))
			defer m.Stop()
			lifecycle := func(since int) []events.Event {
				ec.mu.Lock()
				defer ec.mu.Unlock()
				var found []events.Event
				for _, event := range ec.events[since:] {
					if event.Type == EventMonitorDisconnected || event.Type == EventMonitorReconnected {
						found = append(found, event)
					}
				}
				return found
			}
			for cycle := 0; cycle < 2; cycle++ {
				u.mu.Lock()
				u.current = ""
				if start == "dial_failure" {
					u.current = badURL
				}
				u.mu.Unlock()
				checkpoint, reads, failed, probed := ec.checkpoint(), u.reads.Load(), failures.Load(), probes.Load()
				require.NoError(t, m.Start(context.Background()))
				require.Eventually(t, func() bool { return u.reads.Load() >= reads+3 }, 2*time.Second, time.Millisecond)
				if start == "dial_failure" {
					require.Eventually(t, func() bool { return failures.Load() >= failed+3 }, time.Second, time.Millisecond)
				}
				require.False(t, m.NetworkSnapshot().Up)
				u.notifyRestart(srv.wsURL())
				require.Eventually(t, func() bool { return m.NetworkSnapshot().Up }, 5*time.Second, time.Millisecond)
				// A healthy probe resets retry backoff before the genuine loss below.
				require.Eventually(t, func() bool { return probes.Load() > probed }, 6*time.Second, time.Millisecond)
				require.Never(t, func() bool { return len(lifecycle(checkpoint)) != 0 }, 300*time.Millisecond, time.Millisecond, "startup retries must not emit restart events")
				before := m.NetworkSnapshot()
				m.network.terminal("s", "r", "net::ERR_CONNECTION_RESET")
				require.Equal(t, before.Resets+1, m.NetworkSnapshot().Resets)
				// Lose a real connection and keep recovery failing across several retries.
				u.mu.Lock()
				u.current = badURL
				u.mu.Unlock()
				failed = failures.Load()
				m.lifeMu.Lock()
				conn := m.conn
				m.lifeMu.Unlock()
				require.NoError(t, conn.protocol.Close())
				ec.waitForNew(t, EventMonitorDisconnected, checkpoint, time.Second)
				require.Eventually(t, func() bool { return failures.Load() >= failed+2 }, 2*time.Second, time.Millisecond)
				require.Len(t, lifecycle(checkpoint), 1)
				if cycle == 0 {
					m.Stop() // A new Start must not inherit this unfinished recovery.
					continue
				}
				u.notifyRestart(srv.wsURL())
				ec.waitForNew(t, EventMonitorReconnected, checkpoint, 3*time.Second)
				require.Eventually(t, func() bool { return m.NetworkSnapshot().Up }, time.Second, time.Millisecond)
				require.Never(t, func() bool { return len(lifecycle(checkpoint)) != 2 }, 300*time.Millisecond, time.Millisecond)
				pair := lifecycle(checkpoint)
				require.Equal(t, EventMonitorDisconnected, pair[0].Type)
				require.Equal(t, EventMonitorReconnected, pair[1].Type)
				var data oapi.BrowserMonitorReconnectedEventData
				require.NoError(t, json.Unmarshal(pair[1].Data, &data))
				require.GreaterOrEqual(t, data.ReconnectDurationMs, int64(750))
				require.InDelta(t, (pair[1].Ts-pair[0].Ts)/1000, data.ReconnectDurationMs, 100)
				require.Equal(t, before.Resets+1, m.NetworkSnapshot().Resets)
				require.Equal(t, before.Completed+1, m.NetworkSnapshot().Completed)
			}
		})
	}
}
