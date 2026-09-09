package browsersurface

import (
	"context"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWorkerDiscoveryIsOptIn(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, existing := range []bool{false, true} {
			t.Run(testMode(enabled, existing), func(t *testing.T) {
				protocol := newFakeProtocol()
				targets := []targetInfo{
					{TargetID: "worker", Type: "worker", URL: "https://example.com/worker.js"},
					{TargetID: "shared_worker", Type: "shared_worker", URL: "https://example.com/shared.js"},
					{TargetID: "service_worker", Type: "service_worker", URL: "https://example.com/service.js"},
				}
				protocol.targetInfos = make([]targetInfo, 0)
				if existing {
					protocol.targetInfos = targets
				}
				var options []Option
				if enabled {
					options = append(options, WithAdditionalTargets("worker", "shared_worker", "service_worker"))
				}
				tracker := New(protocol, options...)
				events, unsubscribe := tracker.Subscribe()
				defer unsubscribe()
				defer close(protocol.events)
				require.NoError(t, tracker.Start(context.Background()))
				protocol.emitTarget("Target.attachedToTarget", map[string]any{
					"sessionId": "worker-session", "targetInfo": targets[0],
				})
				for _, target := range targets {
					// Repeated discovery must not create another session.
					protocol.emitTarget("Target.targetCreated", map[string]any{"targetInfo": target})
				}
				attached := make(map[string]bool)
				created := 0
				timeout := time.After(time.Second)
				for created != len(targets) || (enabled && len(attached) != len(targets)) {
					select {
					case event := <-events:
						if event.Kind == EventSessionAttached {
							require.True(t, enabled)
							require.False(t, attached[event.Target.Type])
							attached[event.Target.Type] = true
							require.True(t, tracker.SessionExists(event.SessionID))
						}
						if event.Kind == EventProtocol && event.Message.Method == "Target.targetCreated" {
							created++
						}
					case <-timeout:
						t.Fatal("worker discovery events did not arrive")
					}
				}
				if enabled {
					require.Eventually(t, func() bool {
						protocol.mu.Lock()
						defer protocol.mu.Unlock()
						return protocol.autoAttachCalls["worker-session"] == 1
					}, time.Second, time.Millisecond, "nested dedicated-worker discovery must be enabled")
				}
				protocol.mu.Lock()
				calls := maps.Clone(protocol.attachCalls)
				types := slices.Clone(protocol.discoveredTypes)
				pageCalls := maps.Clone(protocol.pageEnableCalls)
				protocol.mu.Unlock()
				for _, target := range targets {
					if enabled {
						if target.Type == "worker" {
							require.Zero(t, calls[target.TargetID], "dedicated workers attach only through their parent")
						} else {
							require.Equal(t, 1, calls[target.TargetID])
						}
						require.Contains(t, types, target.Type)
					} else {
						require.Zero(t, calls[target.TargetID])
						require.NotContains(t, types, target.Type)
					}
				}
				require.Empty(t, pageCalls, "worker sessions must not initialize Page domains")
			})
		}
	}
}

func testMode(enabled, existing bool) string {
	mode := "default"
	if enabled {
		mode = "workers"
	}
	if existing {
		return mode + "/existing"
	}
	return mode + "/new"
}

func TestSessionOnlyTrackingDoesNotWaitForLocations(t *testing.T) {
	protocol := newFakeProtocol()
	protocol.pageEnableFailures["session-a"] = 100
	tracker := New(protocol, WithoutLocations(), WithAdditionalTargets("worker"))
	events, unsubscribe := tracker.Subscribe()
	defer unsubscribe()
	defer close(protocol.events)
	require.NoError(t, tracker.Start(context.Background()))

	attached := 0
	for attached != 2 {
		select {
		case event := <-events:
			if event.Kind == EventSessionAttached {
				attached++
			}
		case <-time.After(time.Second):
			t.Fatal("page attachment waited for location initialization")
		}
	}
	protocol.mu.Lock()
	pageCalls := maps.Clone(protocol.pageEnableCalls)
	protocol.mu.Unlock()
	require.Empty(t, pageCalls)
	require.Empty(t, tracker.Snapshot().Tabs)

	// Children can arrive before their parents and have no Page frame tree.
	tracker.addSession("nested", "", targetInfo{TargetID: "nested", Type: "iframe", ParentFrameID: "oopif"})
	tracker.addSession("child", "", targetInfo{TargetID: "oopif", Type: "iframe", ParentFrameID: "page-a"})
	tracker.removeTarget("page-a")
	require.False(t, tracker.SessionExists("session-a"))
	require.False(t, tracker.SessionExists("child"))
	require.False(t, tracker.SessionExists("nested"))
	require.True(t, tracker.SessionExists("session-b"))
	tracker.addSession("late-worker", "session-a", targetInfo{TargetID: "worker", Type: "worker"})
	require.False(t, tracker.SessionExists("late-worker"))
}

func TestLateAttachResponseDoesNotRetainClosedTarget(t *testing.T) {
	tracker := New(newFakeProtocol(), WithoutLocations())
	defer tracker.stop()
	target := targetInfo{TargetID: "page-a", Type: "page", URL: "https://example.com/"}
	tracker.registerPage(target)
	tracker.removeTarget(target.TargetID)
	require.NoError(t, tracker.attachTarget(target))
	require.False(t, tracker.SessionExists("session-a"))
}
