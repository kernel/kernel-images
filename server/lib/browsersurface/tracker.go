package browsersurface

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	sessionInitTimeout    = 5 * time.Second
	sessionInitRetryDelay = 50 * time.Millisecond
)

type subscriber struct {
	events chan Event
	done   chan struct{}
}

// Tracker maps CDP targets, sessions, and frames into stable browser windows,
// tabs, and embedded frames. IDs are monotonically increasing for the lifetime
// of the Chromium process and are never reused.
type Tracker struct {
	protocol Protocol
	ctx      context.Context
	cancel   context.CancelFunc
	logger   *slog.Logger

	startMu  sync.Mutex
	started  bool
	startErr error

	stateMu             sync.RWMutex
	nextWindowID        int
	nextTabID           int
	nextFrameID         int
	windows             map[int64]window
	tabs                map[int]*tab
	tabsByTarget        map[string]int
	frames              map[string]*frame
	sessions            map[string]*session
	trackingTarget      map[string]bool
	trackingFrameTarget map[string]bool
	stateChanged        chan struct{}

	subMu       sync.Mutex
	nextSubID   int
	subscribers map[int]*subscriber
	closed      chan struct{}
	closeOnce   sync.Once
}

func New(protocol Protocol) *Tracker {
	ctx, cancel := context.WithCancel(context.Background())
	return &Tracker{
		protocol:            protocol,
		ctx:                 ctx,
		cancel:              cancel,
		logger:              slog.Default(),
		windows:             make(map[int64]window),
		tabs:                make(map[int]*tab),
		tabsByTarget:        make(map[string]int),
		frames:              make(map[string]*frame),
		sessions:            make(map[string]*session),
		trackingTarget:      make(map[string]bool),
		trackingFrameTarget: make(map[string]bool),
		stateChanged:        make(chan struct{}, 1),
		subscribers:         make(map[int]*subscriber),
		closed:              make(chan struct{}),
	}
}

func (t *Tracker) Start(ctx context.Context) error {
	t.startMu.Lock()
	defer t.startMu.Unlock()
	if t.started {
		return t.startErr
	}
	t.started = true
	if t.protocol.Events() == nil {
		t.startErr = fmt.Errorf("start browser surface discovery: protocol events are unavailable")
		return t.startErr
	}
	go t.eventLoop()

	if _, err := t.protocol.Send(ctx, "Target.setDiscoverTargets", map[string]any{
		"discover": true,
		"filter": []map[string]any{
			{"type": "page"},
			{"type": "iframe"},
		},
	}, ""); err != nil {
		t.startErr = fmt.Errorf("start browser surface discovery: %w", err)
		return t.startErr
	}
	raw, err := t.protocol.Send(ctx, "Target.getTargets", nil, "")
	if err != nil {
		t.startErr = fmt.Errorf("list browser tabs: %w", err)
		return t.startErr
	}
	var result struct {
		TargetInfos []targetInfo `json:"targetInfos"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.startErr = fmt.Errorf("decode browser tabs: %w", err)
		return t.startErr
	}
	for _, target := range result.TargetInfos {
		if target.Type == "page" {
			t.trackPage(target, false)
		}
	}
	for _, target := range result.TargetInfos {
		if target.Type == "iframe" {
			t.trackFrameTarget(target)
		}
	}
	return nil
}

func (t *Tracker) Subscribe() (<-chan Event, func()) {
	t.subMu.Lock()
	defer t.subMu.Unlock()
	t.nextSubID++
	id := t.nextSubID
	sub := &subscriber{events: make(chan Event, 256), done: make(chan struct{})}
	t.subscribers[id] = sub
	var once sync.Once
	cancel := func() {
		once.Do(func() {
			close(sub.done)
			t.subMu.Lock()
			if _, ok := t.subscribers[id]; ok {
				delete(t.subscribers, id)
				close(sub.events)
			}
			t.subMu.Unlock()
		})
	}
	return sub.events, cancel
}

func (t *Tracker) Send(ctx context.Context, method string, params any, sessionID string) (json.RawMessage, error) {
	return t.protocol.Send(ctx, method, params, sessionID)
}

func (t *Tracker) Done() <-chan struct{} {
	return t.closed
}

func (t *Tracker) IsClosed() bool {
	select {
	case <-t.closed:
		return true
	default:
		return false
	}
}

func (t *Tracker) HasTabs() bool {
	t.stateMu.RLock()
	defer t.stateMu.RUnlock()
	return len(t.tabs) != 0
}

func (t *Tracker) RefreshWindows(ctx context.Context) {
	t.stateMu.RLock()
	tabs := make([]tab, 0, len(t.tabs))
	for _, tracked := range t.tabs {
		tabs = append(tabs, *tracked)
	}
	t.stateMu.RUnlock()
	sort.Slice(tabs, func(i, j int) bool { return tabs[i].id < tabs[j].id })
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	for _, tracked := range tabs {
		raw, err := t.protocol.Send(ctx, "Browser.getWindowForTarget", map[string]any{"targetId": tracked.targetID}, "")
		if err != nil {
			continue
		}
		var result struct {
			WindowID int64 `json:"windowId"`
		}
		if json.Unmarshal(raw, &result) == nil {
			t.assignWindow(tracked.targetID, result.WindowID)
		}
	}
}

func (t *Tracker) Snapshot() Snapshot {
	t.stateMu.RLock()
	defer t.stateMu.RUnlock()
	snapshot := Snapshot{
		Windows: make([]WindowInfo, 0, len(t.windows)),
		Tabs:    make([]TabInfo, 0, len(t.tabs)),
		Frames:  make([]FrameInfo, 0, len(t.frames)),
	}
	for _, tracked := range t.windows {
		snapshot.Windows = append(snapshot.Windows, WindowInfo{ID: tracked.id})
	}
	for _, tracked := range t.tabs {
		snapshot.Tabs = append(snapshot.Tabs, TabInfo{
			ID: tracked.id, WindowID: tracked.windowID,
			PageTitle: tracked.title, PageURL: stripFragment(tracked.url),
		})
	}
	for _, tracked := range t.frames {
		if tracked.id == 0 {
			continue
		}
		parentFrameID := 0
		if parent := t.frames[tracked.parentID]; parent != nil {
			parentFrameID = parent.id
		}
		snapshot.Frames = append(snapshot.Frames, FrameInfo{
			ID: tracked.id, TabID: tracked.tabID, ParentFrameID: parentFrameID,
			URL: stripFragment(tracked.url),
		})
	}
	sort.Slice(snapshot.Windows, func(i, j int) bool { return snapshot.Windows[i].ID < snapshot.Windows[j].ID })
	sort.Slice(snapshot.Tabs, func(i, j int) bool { return snapshot.Tabs[i].ID < snapshot.Tabs[j].ID })
	sort.Slice(snapshot.Frames, func(i, j int) bool { return snapshot.Frames[i].ID < snapshot.Frames[j].ID })
	return snapshot
}

func (t *Tracker) SessionExists(sessionID string) bool {
	t.stateMu.RLock()
	defer t.stateMu.RUnlock()
	_, ok := t.sessions[sessionID]
	return ok
}

func (t *Tracker) Resolve(sessionID, frameID string) (Location, bool) {
	t.stateMu.RLock()
	defer t.stateMu.RUnlock()
	sess, ok := t.sessions[sessionID]
	if !ok || sess.tabID == 0 {
		return Location{}, false
	}
	tab, ok := t.tabs[sess.tabID]
	if !ok {
		return Location{}, false
	}
	location := Location{
		WindowID:  tab.windowID,
		TabID:     tab.id,
		PageTitle: tab.title,
		PageURL:   stripFragment(tab.url),
	}
	trackedFrame, ok := t.frames[frameID]
	if !ok || trackedFrame.tabID != tab.id {
		return Location{}, false
	}
	if trackedFrame.parentID != "" {
		location.Frame = &FrameLocation{ID: trackedFrame.id, URL: stripFragment(trackedFrame.url)}
	}
	return location, true
}

func (t *Tracker) WaitForSettled(ctx context.Context) {
	limit := time.NewTimer(2 * time.Second)
	defer limit.Stop()
	quiet := time.NewTimer(200 * time.Millisecond)
	defer quiet.Stop()
	for {
		select {
		case <-t.stateChanged:
			if !quiet.Stop() {
				select {
				case <-quiet.C:
				default:
				}
			}
			quiet.Reset(200 * time.Millisecond)
		case <-quiet.C:
			return
		case <-limit.C:
			return
		case <-ctx.Done():
			return
		case <-t.closed:
			return
		}
	}
}

func (t *Tracker) eventLoop() {
	defer t.stop()
	for message := range t.protocol.Events() {
		t.handleProtocolEvent(message)
	}
}

func (t *Tracker) publish(event Event) {
	t.subMu.Lock()
	defer t.subMu.Unlock()
	for _, sub := range t.subscribers {
		select {
		case sub.events <- event:
		case <-sub.done:
		case <-t.closed:
			return
		}
	}
}

func (t *Tracker) signalChanged() {
	select {
	case t.stateChanged <- struct{}{}:
	default:
	}
}

func (t *Tracker) stop() {
	t.closeOnce.Do(func() {
		t.cancel()
		close(t.closed)
		t.subMu.Lock()
		for id, sub := range t.subscribers {
			close(sub.events)
			delete(t.subscribers, id)
		}
		t.subMu.Unlock()
	})
}

func stripFragment(value string) string {
	withoutFragment, _, _ := strings.Cut(value, "#")
	return withoutFragment
}
