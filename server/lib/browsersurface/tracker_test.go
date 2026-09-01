package browsersurface

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/kernel/kernel-images/server/lib/cdpclient"
	"github.com/stretchr/testify/require"
)

type fakeProtocol struct {
	mu                  sync.Mutex
	windowIDs           map[string]int
	attachFailures      int
	attachCalls         map[string]int
	detachCalls         map[string]int
	pageEnableFailures  map[string]int
	pageEnableCalls     map[string]int
	frameTreeFailures   map[string]int
	beforeTargetsResult func()
	events              chan cdpclient.Message
	closed              chan struct{}
}

func newFakeProtocol() *fakeProtocol {
	return &fakeProtocol{
		windowIDs:          map[string]int{"page-a": 10, "page-b": 20},
		attachCalls:        make(map[string]int),
		detachCalls:        make(map[string]int),
		pageEnableFailures: make(map[string]int),
		pageEnableCalls:    make(map[string]int),
		frameTreeFailures:  make(map[string]int),
		events:             make(chan cdpclient.Message, 64),
		closed:             make(chan struct{}),
	}
}

func (f *fakeProtocol) Send(_ context.Context, method string, params any, sessionID string) (json.RawMessage, error) {
	marshal := func(value any) json.RawMessage {
		raw, _ := json.Marshal(value)
		return raw
	}
	switch method {
	case "Target.setDiscoverTargets":
		return marshal(map[string]any{}), nil
	case "Target.getTargets":
		f.mu.Lock()
		beforeResult := f.beforeTargetsResult
		f.beforeTargetsResult = nil
		f.mu.Unlock()
		if beforeResult != nil {
			beforeResult()
		}
		return marshal(map[string]any{"targetInfos": []map[string]any{
			{"targetId": "page-a", "type": "page", "title": "Store", "url": "https://store.example/"},
			{"targetId": "page-b", "type": "page", "title": "Travel", "url": "https://travel.example/"},
		}}), nil
	case "Browser.getWindowForTarget":
		targetID := params.(map[string]any)["targetId"].(string)
		f.mu.Lock()
		windowID := f.windowIDs[targetID]
		f.mu.Unlock()
		return marshal(map[string]any{"windowId": windowID}), nil
	case "Target.attachToTarget":
		targetID := params.(map[string]any)["targetId"].(string)
		f.mu.Lock()
		f.attachCalls[targetID]++
		if f.attachFailures > 0 {
			f.attachFailures--
			f.mu.Unlock()
			return nil, errors.New("temporary attach failure")
		}
		f.mu.Unlock()
		sessionID := map[string]string{
			"page-a": "session-a", "page-b": "session-b", "late-page": "late-session", "oopif": "oopif-session",
		}[targetID]
		return marshal(map[string]any{"sessionId": sessionID}), nil
	case "Target.detachFromTarget":
		detachedSessionID := params.(map[string]any)["sessionId"].(string)
		f.mu.Lock()
		f.detachCalls[detachedSessionID]++
		f.mu.Unlock()
		return marshal(map[string]any{}), nil
	case "Page.enable":
		f.mu.Lock()
		f.pageEnableCalls[sessionID]++
		if f.pageEnableFailures[sessionID] > 0 {
			f.pageEnableFailures[sessionID]--
			f.mu.Unlock()
			return nil, errors.New("temporary Page.enable failure")
		}
		f.mu.Unlock()
		return marshal(map[string]any{}), nil
	case "Page.getFrameTree":
		f.mu.Lock()
		if f.frameTreeFailures[sessionID] > 0 {
			f.frameTreeFailures[sessionID]--
			f.mu.Unlock()
			return nil, errors.New("temporary Page.getFrameTree failure")
		}
		f.mu.Unlock()
		if sessionID == "oopif-session" {
			return marshal(map[string]any{"frameTree": map[string]any{
				"frame": map[string]any{"id": "oopif", "url": "https://cross-origin.example/"},
			}}), nil
		}
		if sessionID == "late-session" {
			return marshal(map[string]any{"frameTree": map[string]any{
				"frame": map[string]any{"id": "root-late", "url": "https://late.example/"},
			}}), nil
		}
		if sessionID == "session-a" {
			return marshal(map[string]any{"frameTree": map[string]any{
				"frame": map[string]any{"id": "root-a", "url": "https://store.example/"},
				"childFrames": []any{map[string]any{
					"frame": map[string]any{"id": "outer", "parentId": "root-a", "url": "https://payments.example/"},
					"childFrames": []any{map[string]any{
						"frame": map[string]any{"id": "inner", "parentId": "outer", "url": "https://bank.example/"},
					}},
				}},
			}}), nil
		}
		return marshal(map[string]any{"frameTree": map[string]any{
			"frame": map[string]any{"id": "root-b", "url": "https://travel.example/"},
		}}), nil
	default:
		return marshal(map[string]any{}), nil
	}
}

func (f *fakeProtocol) Events() <-chan cdpclient.Message { return f.events }
func (f *fakeProtocol) Done() <-chan struct{}            { return f.closed }
func (f *fakeProtocol) IsClosed() bool                   { return false }

func (f *fakeProtocol) emitAttached(sessionID, targetID, title, url string) {
	f.emitAttachedFrom("", sessionID, targetID, title, url)
}

func (f *fakeProtocol) emitAttachedFrom(parentSessionID, sessionID, targetID, title, url string) {
	params, _ := json.Marshal(map[string]any{
		"sessionId": sessionID,
		"targetInfo": map[string]any{
			"targetId": targetID, "type": "page", "title": title, "url": url,
		},
	})
	f.events <- cdpclient.Message{
		Method: "Target.attachedToTarget", SessionID: parentSessionID, Params: params,
	}
}

func (f *fakeProtocol) emitTarget(method string, params any) {
	raw, _ := json.Marshal(params)
	f.events <- cdpclient.Message{Method: method, Params: raw}
}

func (f *fakeProtocol) setWindow(targetID string, windowID int) {
	f.mu.Lock()
	f.windowIDs[targetID] = windowID
	f.mu.Unlock()
}

func TestTrackerRejectsProtocolWithoutEvents(t *testing.T) {
	protocol := newFakeProtocol()
	protocol.events = nil
	tracker := New(protocol)

	err := tracker.Start(context.Background())
	require.EqualError(t, err, "start browser surface discovery: protocol events are unavailable")
}

func TestTrackerRetriesTransientSessionInitializationFailure(t *testing.T) {
	for _, test := range []struct {
		name      string
		configure func(*fakeProtocol)
	}{
		{name: "enable", configure: func(protocol *fakeProtocol) { protocol.pageEnableFailures["session-a"] = 1 }},
		{name: "frame tree", configure: func(protocol *fakeProtocol) { protocol.frameTreeFailures["session-a"] = 1 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			protocol := newFakeProtocol()
			test.configure(protocol)
			tracker := New(protocol)
			require.NoError(t, tracker.Start(context.Background()))

			require.Eventually(t, func() bool {
				protocol.mu.Lock()
				calls := protocol.pageEnableCalls["session-a"]
				protocol.mu.Unlock()
				_, resolved := tracker.Resolve("session-a", "root-a")
				return calls >= 2 && resolved
			}, time.Second, 10*time.Millisecond)
		})
	}
}

func TestTrackerReattachesAfterTerminalSessionInitializationFailure(t *testing.T) {
	protocol := newFakeProtocol()
	tracker := New(protocol)
	require.NoError(t, tracker.Start(context.Background()))
	require.Eventually(t, func() bool {
		_, resolved := tracker.Resolve("session-a", "root-a")
		return resolved
	}, time.Second, 10*time.Millisecond)

	tracker.stateMu.Lock()
	tracker.sessions["session-a"].initialized = false
	tracker.sessions["session-a"].initializing = true
	tracker.stateMu.Unlock()
	tracker.failSessionInitialization("session-a", errors.New("initialization deadline exceeded"))
	require.False(t, tracker.SessionExists("session-a"))

	require.NoError(t, tracker.RefreshTargets(context.Background()))
	require.Eventually(t, func() bool {
		_, resolved := tracker.Resolve("session-a", "root-a")
		return resolved
	}, time.Second, 10*time.Millisecond)
	protocol.mu.Lock()
	defer protocol.mu.Unlock()
	require.Equal(t, 2, protocol.attachCalls["page-a"])
	require.Equal(t, 1, protocol.detachCalls["session-a"])
}

func TestTrackerStopsInitializationRetriesAfterSessionRemoval(t *testing.T) {
	protocol := newFakeProtocol()
	protocol.pageEnableFailures["session-a"] = 1000
	tracker := New(protocol)
	require.NoError(t, tracker.Start(context.Background()))
	require.Eventually(t, func() bool {
		protocol.mu.Lock()
		defer protocol.mu.Unlock()
		return protocol.pageEnableCalls["session-a"] > 0
	}, time.Second, 10*time.Millisecond)

	protocol.emitTarget("Target.detachedFromTarget", map[string]any{"sessionId": "session-a"})
	require.Eventually(t, func() bool {
		return !tracker.SessionExists("session-a")
	}, time.Second, 10*time.Millisecond)
	protocol.mu.Lock()
	callsAfterRemoval := protocol.pageEnableCalls["session-a"]
	protocol.mu.Unlock()
	time.Sleep(4 * sessionInitRetryDelay)
	protocol.mu.Lock()
	defer protocol.mu.Unlock()
	require.Equal(t, callsAfterRemoval, protocol.pageEnableCalls["session-a"])
}

func TestTrackerMapsBrowserSurfaceAndPublishesLifecycleEvents(t *testing.T) {
	protocol := newFakeProtocol()
	tracker := New(protocol)
	events, cancel := tracker.Subscribe()
	defer cancel()
	require.NoError(t, tracker.Start(context.Background()))

	require.Eventually(t, func() bool {
		location, ok := tracker.Resolve("session-a", "inner")
		return ok && location.Frame != nil
	}, time.Second, 10*time.Millisecond)
	location, ok := tracker.Resolve("session-a", "inner")
	require.True(t, ok)
	require.Equal(t, Location{
		WindowID:  1,
		TabID:     1,
		PageTitle: "Store",
		PageURL:   "https://store.example/",
		Frame:     &FrameLocation{ID: 2, URL: "https://bank.example/"},
	}, location)
	root, ok := tracker.Resolve("session-a", "root-a")
	require.True(t, ok)
	require.Nil(t, root.Frame)

	protocol.emitTarget("Target.targetCreated", map[string]any{
		"targetInfo": map[string]any{
			"targetId": "oopif", "type": "iframe", "url": "https://cross-origin.example/",
			"parentFrameId": "root-a",
		},
	})
	require.Eventually(t, func() bool {
		location, resolved := tracker.Resolve("oopif-session", "oopif")
		return resolved && location.Frame != nil && location.Frame.ID == 3
	}, time.Second, 10*time.Millisecond)
	require.Eventually(t, func() bool {
		protocol.mu.Lock()
		defer protocol.mu.Unlock()
		return protocol.attachCalls["oopif"] == 1
	}, time.Second, 10*time.Millisecond)

	require.Equal(t, Snapshot{
		Windows: []WindowInfo{{ID: 1}, {ID: 2}},
		Tabs: []TabInfo{
			{ID: 1, WindowID: 1, PageTitle: "Store", PageURL: "https://store.example/"},
			{ID: 2, WindowID: 2, PageTitle: "Travel", PageURL: "https://travel.example/"},
		},
		Frames: []FrameInfo{
			{ID: 1, TabID: 1, URL: "https://payments.example/"},
			{ID: 2, TabID: 1, ParentFrameID: 1, URL: "https://bank.example/"},
			{ID: 3, TabID: 1, URL: "https://cross-origin.example/"},
		},
	}, tracker.Snapshot())

	ready := make(map[string]bool)
	require.Eventually(t, func() bool {
		select {
		case event := <-events:
			if event.Kind == EventSessionReady {
				ready[event.SessionID] = true
			}
		default:
		}
		return ready["session-a"] && ready["session-b"]
	}, time.Second, 10*time.Millisecond)

	protocol.setWindow("page-a", 20)
	tracker.RefreshWindows(context.Background())
	moved, ok := tracker.Resolve("session-a", "root-a")
	require.True(t, ok)
	require.Equal(t, 1, moved.TabID)
	require.Equal(t, 2, moved.WindowID)
}

func TestTrackerPreservesFramesDuringProcessSwap(t *testing.T) {
	protocol := newFakeProtocol()
	tracker := New(protocol)
	events, cancel := tracker.Subscribe()
	defer cancel()
	require.NoError(t, tracker.Start(context.Background()))
	require.Eventually(t, func() bool {
		_, ok := tracker.Resolve("session-a", "inner")
		return ok
	}, time.Second, 10*time.Millisecond)

	waitForDetach := func(reason string) {
		require.Eventually(t, func() bool {
			select {
			case event := <-events:
				if event.Kind != EventProtocol || event.Message.Method != "Page.frameDetached" {
					return false
				}
				var params struct {
					Reason string `json:"reason"`
				}
				return json.Unmarshal(event.Message.Params, &params) == nil && params.Reason == reason
			default:
				return false
			}
		}, time.Second, 10*time.Millisecond)
	}

	protocol.emitTarget("Page.frameDetached", map[string]any{"frameId": "outer", "reason": "swap"})
	waitForDetach("swap")
	_, ok := tracker.Resolve("session-a", "inner")
	require.True(t, ok)

	protocol.emitTarget("Page.frameDetached", map[string]any{"frameId": "outer", "reason": "remove"})
	waitForDetach("remove")
	require.Eventually(t, func() bool {
		_, ok := tracker.Resolve("session-a", "inner")
		return !ok
	}, time.Second, 10*time.Millisecond)
}

func TestTrackerRefreshDoesNotRemoveTabCreatedAfterSnapshot(t *testing.T) {
	protocol := newFakeProtocol()
	tracker := New(protocol)
	require.NoError(t, tracker.Start(context.Background()))
	protocol.mu.Lock()
	protocol.beforeTargetsResult = func() {
		protocol.emitTarget("Target.targetCreated", map[string]any{"targetInfo": map[string]any{
			"targetId": "concurrent", "type": "page", "title": "Concurrent", "url": "https://concurrent.example/",
		}})
		require.Eventually(t, func() bool {
			for _, tab := range tracker.Snapshot().Tabs {
				if tab.PageURL == "https://concurrent.example/" {
					return true
				}
			}
			return false
		}, time.Second, 10*time.Millisecond)
	}
	protocol.mu.Unlock()

	require.NoError(t, tracker.RefreshTargets(context.Background()))
	found := false
	for _, tab := range tracker.Snapshot().Tabs {
		if tab.PageURL == "https://concurrent.example/" {
			found = true
		}
	}
	require.True(t, found)
}

func TestTrackerRefreshRemovesMissingFrameTargetSession(t *testing.T) {
	protocol := newFakeProtocol()
	tracker := New(protocol)
	events, cancel := tracker.Subscribe()
	defer cancel()
	require.NoError(t, tracker.Start(context.Background()))

	protocol.emitTarget("Target.targetCreated", map[string]any{"targetInfo": map[string]any{
		"targetId": "oopif", "type": "iframe", "url": "https://cross-origin.example/",
		"parentFrameId": "root-a",
	}})
	require.Eventually(t, func() bool {
		_, resolved := tracker.Resolve("oopif-session", "oopif")
		return resolved
	}, time.Second, 10*time.Millisecond)

	// Target.getTargets omits oopif, simulating a missed targetDestroyed event.
	require.NoError(t, tracker.RefreshTargets(context.Background()))
	require.False(t, tracker.SessionExists("oopif-session"))
	_, resolved := tracker.Resolve("oopif-session", "oopif")
	require.False(t, resolved)
	require.Eventually(t, func() bool {
		select {
		case event := <-events:
			return event.Kind == EventSessionRemoved && event.SessionID == "oopif-session"
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond)
}

func TestTrackerRetriesTabsAfterAttachFailure(t *testing.T) {
	protocol := newFakeProtocol()
	protocol.attachFailures = 1
	tracker := New(protocol)
	require.NoError(t, tracker.Start(context.Background()))
	require.False(t, tracker.SessionExists("session-a"))

	require.NoError(t, tracker.RefreshTargets(context.Background()))
	require.Eventually(t, func() bool {
		location, ok := tracker.Resolve("session-a", "root-a")
		return ok && location.TabID == 1
	}, time.Second, 10*time.Millisecond)
	protocol.mu.Lock()
	require.Equal(t, 2, protocol.attachCalls["page-a"])
	protocol.mu.Unlock()
}

func TestTrackerReattachesPageAfterSessionDetach(t *testing.T) {
	protocol := newFakeProtocol()
	tracker := New(protocol)
	require.NoError(t, tracker.Start(context.Background()))
	require.Eventually(t, func() bool { return tracker.SessionExists("session-a") }, time.Second, 10*time.Millisecond)

	protocol.emitTarget("Target.detachedFromTarget", map[string]any{"sessionId": "session-a"})
	require.Eventually(t, func() bool { return !tracker.SessionExists("session-a") }, time.Second, 10*time.Millisecond)
	require.NoError(t, tracker.RefreshTargets(context.Background()))
	require.Eventually(t, func() bool { return tracker.SessionExists("session-a") }, time.Second, 10*time.Millisecond)
	protocol.mu.Lock()
	require.Equal(t, 2, protocol.attachCalls["page-a"])
	protocol.mu.Unlock()
}

func TestTrackerDoesNotRetainTabClosedImmediatelyAfterCreation(t *testing.T) {
	protocol := newFakeProtocol()
	tracker := New(protocol)
	events, cancel := tracker.Subscribe()
	defer cancel()
	require.NoError(t, tracker.Start(context.Background()))

	protocol.emitTarget("Target.targetCreated", map[string]any{"targetInfo": map[string]any{
		"targetId": "short-lived", "type": "page", "title": "Short", "url": "https://short.example/",
	}})
	protocol.emitTarget("Target.targetDestroyed", map[string]any{"targetId": "short-lived"})
	require.Eventually(t, func() bool {
		select {
		case event := <-events:
			return event.Kind == EventProtocol && event.Message.Method == "Target.targetDestroyed"
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond)
	for _, tab := range tracker.Snapshot().Tabs {
		require.NotEqual(t, "https://short.example/", tab.PageURL)
	}
}

func TestTrackerBindsPageSessionThatArrivesBeforeTarget(t *testing.T) {
	protocol := newFakeProtocol()
	protocol.windowIDs["late-page"] = 30
	tracker := New(protocol)
	require.NoError(t, tracker.Start(context.Background()))

	protocol.emitAttachedFrom("session-a", "late-session", "late-page", "Late", "https://late.example/")
	protocol.emitTarget("Target.targetCreated", map[string]any{"targetInfo": map[string]any{
		"targetId": "late-page", "type": "page", "title": "Late", "url": "https://late.example/",
	}})
	require.Eventually(t, func() bool {
		location, ok := tracker.Resolve("late-session", "root-late")
		return ok && location.TabID == 3 && location.PageURL == "https://late.example/"
	}, time.Second, 10*time.Millisecond)

	protocol.emitTarget("Target.detachedFromTarget", map[string]any{"sessionId": "session-a"})
	require.Eventually(t, func() bool { return !tracker.SessionExists("session-a") }, time.Second, 10*time.Millisecond)
	location, ok := tracker.Resolve("late-session", "root-late")
	require.True(t, ok)
	require.Equal(t, 3, location.TabID)
}

func TestTrackerNeverReusesWindowTabOrFrameIDs(t *testing.T) {
	protocol := newFakeProtocol()
	tracker := New(protocol)
	require.NoError(t, tracker.Start(context.Background()))
	require.Eventually(t, func() bool {
		_, ok := tracker.Resolve("session-a", "inner")
		return ok
	}, time.Second, 10*time.Millisecond)

	protocol.emitTarget("Target.targetDestroyed", map[string]any{"targetId": "page-a"})
	require.Eventually(t, func() bool { return !tracker.SessionExists("session-a") }, time.Second, 10*time.Millisecond)
	protocol.emitTarget("Target.targetCreated", map[string]any{"targetInfo": map[string]any{
		"targetId": "page-a", "type": "page", "title": "Store Again", "url": "https://store.example/again",
	}})

	require.Eventually(t, func() bool {
		location, ok := tracker.Resolve("session-a", "inner")
		return ok && location.TabID == 3
	}, time.Second, 10*time.Millisecond)
	location, ok := tracker.Resolve("session-a", "inner")
	require.True(t, ok)
	require.Equal(t, 3, location.WindowID)
	require.Equal(t, 3, location.TabID)
	require.Equal(t, 4, location.Frame.ID)
}
