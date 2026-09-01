package browsersurface

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/kernel/kernel-images/server/lib/cdpconnection"
	"github.com/stretchr/testify/require"
)

type fakeProtocol struct {
	mu        sync.Mutex
	windowIDs map[string]int
	events    chan cdpconnection.Message
	closed    chan struct{}
}

func newFakeProtocol() *fakeProtocol {
	return &fakeProtocol{
		windowIDs: map[string]int{"page-a": 10, "page-b": 20},
		events:    make(chan cdpconnection.Message, 64),
		closed:    make(chan struct{}),
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
	case "Target.autoAttachRelated":
		targetID := params.(map[string]any)["targetId"].(string)
		if targetID == "page-a" {
			f.emitAttached("session-a", "page-a", "Store", "https://store.example/")
		} else {
			f.emitAttached("session-b", "page-b", "Travel", "https://travel.example/")
		}
		return marshal(map[string]any{}), nil
	case "Page.enable":
		return marshal(map[string]any{}), nil
	case "Page.getFrameTree":
		if sessionID == "oopif-session" {
			return marshal(map[string]any{"frameTree": map[string]any{
				"frame": map[string]any{"id": "inner", "url": "https://bank.example/"},
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

func (f *fakeProtocol) Events() <-chan cdpconnection.Message { return f.events }
func (f *fakeProtocol) Done() <-chan struct{}                { return f.closed }
func (f *fakeProtocol) IsClosed() bool                       { return false }

func (f *fakeProtocol) emitAttached(sessionID, targetID, title, url string) {
	params, _ := json.Marshal(map[string]any{
		"sessionId": sessionID,
		"targetInfo": map[string]any{
			"targetId": targetID, "type": "page", "title": title, "url": url,
		},
	})
	f.events <- cdpconnection.Message{Method: "Target.attachedToTarget", Params: params}
}

func (f *fakeProtocol) emitTarget(method string, params any) {
	raw, _ := json.Marshal(params)
	f.events <- cdpconnection.Message{Method: method, Params: raw}
}

func (f *fakeProtocol) setWindow(targetID string, windowID int) {
	f.mu.Lock()
	f.windowIDs[targetID] = windowID
	f.mu.Unlock()
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

	protocol.emitTarget("Target.attachedToTarget", map[string]any{
		"sessionId": "oopif-session",
		"targetInfo": map[string]any{
			"targetId": "inner", "type": "iframe", "url": "https://bank.example/",
		},
	})
	require.Eventually(t, func() bool {
		location, resolved := tracker.Resolve("oopif-session", "inner")
		return resolved && location.Frame != nil && location.Frame.ID == 2
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
