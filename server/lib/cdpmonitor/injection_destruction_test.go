package cdpmonitor

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kernel/kernel-images/server/lib/browsersurface"
	"github.com/kernel/kernel-images/server/lib/cdpclient"
	"github.com/stretchr/testify/require"
)

func TestLateInjectionOutcomeAfterTargetRemoval(t *testing.T) {
	for _, removal := range []string{"destroy", "detach"} {
		for _, outcome := range []string{"success", "canceled", "timeout", "transport"} {
			for _, prior := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/prior_%t", removal, outcome, prior), func(t *testing.T) {
					srv := newTestServer(t)
					defer srv.close()
					var hold atomic.Bool
					blocked := make(chan cdpMessage, 1)
					stop := make(chan struct{})
					defer close(stop)
					go listenAndRespond(srv, stop, func(msg cdpMessage) any {
						if msg.Method == "Page.addScriptToEvaluateOnNewDocument" && hold.Load() {
							blocked <- msg
							return map[string]any{"method": "Test.unanswered"}
						}
						return nil
					})
					protocol, err := cdpclient.DialWithEvents(context.Background(), srv.wsURL())
					require.NoError(t, err)
					defer protocol.Close()
					m := New(newTestUpstream(""), newEventCollector().publishFn(), 0, discardLogger, nil)
					m.conn = &monitorConnection{protocol: protocol}
					m.sessions["s"] = targetInfo{targetID: "t", targetType: "page"}
					m.optionalSessions["s"] = ""
					timeout := 5 * time.Second
					if outcome == "timeout" {
						timeout = 100 * time.Millisecond
					}
					ctx, cancel := context.WithTimeout(context.Background(), timeout)
					defer cancel()
					if prior {
						require.NoError(t, m.injectScript(ctx, "s"))
						require.Contains(t, m.interactionTargets, "t")
					}
					hold.Store(true)
					done := make(chan error, 1)
					go func() { done <- m.injectScript(ctx, "s") }()
					var command cdpMessage
					select {
					case command = <-blocked:
					case <-time.After(time.Second):
						t.Fatal("registration was not sent")
					}
					m.sessionsMu.RLock()
					pending := len(m.pendingInjections)
					m.sessionsMu.RUnlock()
					require.Equal(t, 1, pending)
					// The tracker removes sessions before publishing target destruction.
					m.handleDetachedFromTarget(cdpTargetDetachedFromTargetParams{SessionID: "s"})
					if removal == "destroy" {
						m.handleSurfaceEvent(nil, browsersurface.Event{Kind: browsersurface.EventProtocol, Message: cdpclient.Message{Method: "Target.targetDestroyed", Params: []byte(`{"targetId":"t"}`)}})
						require.Empty(t, m.interactionTargets)
					}
					switch outcome {
					case "success":
						srv.sendToMonitor(t, map[string]any{"id": command.ID, "result": map[string]any{"identifier": "late-script"}})
					case "canceled":
						cancel()
					case "transport":
						require.NoError(t, protocol.Close())
					}
					select {
					case err = <-done:
					case <-time.After(time.Second):
						t.Fatal("registration did not finish")
					}
					require.Empty(t, m.pendingInjections, "completed registrations must not leave tombstones")
					if removal == "destroy" {
						require.Empty(t, m.interactionTargets, "late outcome resurrected a destroyed target")
						require.NoError(t, err, "a destroyed target cannot require uncertain-registration recovery")
					} else {
						require.Contains(t, m.interactionTargets, "t", "detach does not prove document destruction")
						switch outcome {
						case "success":
							require.NoError(t, err)
						case "canceled":
							require.ErrorIs(t, err, context.Canceled)
						case "timeout":
							require.ErrorIs(t, err, context.DeadlineExceeded)
						case "transport":
							require.ErrorIs(t, err, cdpclient.ErrOutcomeUnknown)
						}
					}
				})
			}
		}
	}
}
