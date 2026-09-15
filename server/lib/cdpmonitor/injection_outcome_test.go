package cdpmonitor

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kernel/kernel-images/server/lib/cdpclient"
	"github.com/stretchr/testify/require"
)

func TestInjectionCleanupObligations(t *testing.T) {
	for _, outcome := range []string{"rejected", "unsupported", "timeout", "transport", "missing_id", "malformed_result", "evaluate_rejected", "evaluate_exception", "evaluate_timeout", "success_then_rejected"} {
		t.Run(outcome, func(t *testing.T) {
			srv := newTestServer(t)
			defer srv.close()
			var registrations, evaluations atomic.Int32
			stop := make(chan struct{})
			defer close(stop)
			go listenAndRespond(srv, stop, func(msg cdpMessage) any {
				switch msg.Method {
				case "Page.addScriptToEvaluateOnNewDocument":
					attempt := registrations.Add(1)
					if outcome == "rejected" || outcome == "unsupported" || (outcome == "success_then_rejected" && attempt > 1) {
						code := -32000
						if outcome == "unsupported" {
							code = -32601
						}
						return map[string]any{"id": msg.ID, "error": map[string]any{"code": code, "message": "fixture rejection"}}
					}
					if outcome == "transport" {
						srv.connMu.Lock()
						_ = srv.conn.CloseNow()
						srv.connMu.Unlock()
						return map[string]any{"method": "Test.unanswered"}
					}
					if outcome == "timeout" {
						return map[string]any{"method": "Test.unanswered"}
					}
					if outcome == "missing_id" {
						return map[string]any{"id": msg.ID, "result": map[string]any{}}
					}
					if outcome == "malformed_result" {
						return map[string]any{"id": msg.ID, "result": "invalid"}
					}
				case "Runtime.evaluate":
					evaluations.Add(1)
					if outcome == "evaluate_rejected" {
						return map[string]any{"id": msg.ID, "error": map[string]any{"code": -32000, "message": "fixture evaluation rejection"}}
					}
					if outcome == "evaluate_exception" {
						return map[string]any{"id": msg.ID, "result": map[string]any{"exceptionDetails": map[string]any{"text": "fixture exception"}}}
					}
					if outcome == "evaluate_timeout" {
						return map[string]any{"method": "Test.unanswered"}
					}
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
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			if outcome == "success_then_rejected" {
				require.NoError(t, m.injectScript(ctx, "s"))
			}
			err = m.injectScript(ctx, "s")
			if outcome == "rejected" || outcome == "unsupported" || outcome == "success_then_rejected" {
				var rejection *cdpclient.Error
				require.ErrorAs(t, err, &rejection)
			} else if outcome == "timeout" {
				require.ErrorIs(t, err, context.DeadlineExceeded)
			} else if outcome == "transport" {
				require.ErrorIs(t, err, cdpclient.ErrOutcomeUnknown)
			} else if outcome == "missing_id" || outcome == "malformed_result" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, "test-script", m.optionalSessions["s"], "evaluation failure does not undo future-document registration")
			}
			if outcome == "rejected" || outcome == "unsupported" {
				require.Empty(t, m.interactionTargets)
				require.Zero(t, evaluations.Load(), "a rejected registration must not fall back to live-document injection")
				require.NoError(t, m.disableOptionalDomains(context.Background(), "s", ""))
				require.EqualValues(t, 1, registrations.Load(), "clean targets must not run script cleanup")
			} else {
				require.Contains(t, m.interactionTargets, "t")
				if outcome == "success_then_rejected" {
					require.Equal(t, "test-script", m.optionalSessions["s"])
					require.EqualValues(t, 1, evaluations.Load())
					require.Error(t, m.disableOptionalDomains(context.Background(), "s", "test-script"))
					require.Contains(t, m.interactionTargets, "t", "failed cleanup must preserve the earlier obligation")
				}
			}
		})
	}
}

func TestInjectionOutcomeSurvivesDetach(t *testing.T) {
	srv := newTestServer(t)
	defer srv.close()
	protocol, err := cdpclient.DialWithEvents(context.Background(), srv.wsURL())
	require.NoError(t, err)
	defer protocol.Close()
	m := New(newTestUpstream(""), newEventCollector().publishFn(), 0, discardLogger, nil)
	m.conn = &monitorConnection{protocol: protocol}
	m.sessions["s"] = targetInfo{targetID: "t", targetType: "page"}
	canceled, cancelBeforeSend := context.WithCancel(context.Background())
	cancelBeforeSend()
	require.ErrorIs(t, m.injectScript(canceled, "s"), context.Canceled)
	require.Empty(t, m.interactionTargets, "cancellation before sending has no injection outcome")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- m.injectScript(ctx, "s") }()
	command := srv.readFromMonitor(t, time.Second)
	require.Equal(t, "Page.addScriptToEvaluateOnNewDocument", command.Method)
	m.handleDetachedFromTarget(cdpTargetDetachedFromTargetParams{SessionID: "s"})
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	require.Contains(t, m.interactionTargets, "t")
}
