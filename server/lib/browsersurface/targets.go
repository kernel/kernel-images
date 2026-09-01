package browsersurface

import (
	"context"
	"encoding/json"
	"time"
)

func (t *Tracker) trackPage(target targetInfo, async bool) {
	tabID, attach := t.registerPage(target)
	if !attach {
		return
	}
	if async {
		go t.attachPage(tabID, target)
		return
	}
	t.attachPage(tabID, target)
}

func (t *Tracker) registerPage(target targetInfo) (int, bool) {
	t.stateMu.Lock()
	if tabID := t.tabsByTarget[target.TargetID]; tabID != 0 {
		t.updateTabTargetLocked(target)
		attach := !t.trackingTarget[target.TargetID]
		t.trackingTarget[target.TargetID] = true
		t.bindSessionsLocked()
		t.stateMu.Unlock()
		t.signalChanged()
		return tabID, attach
	}
	t.nextTabID++
	tabID := t.nextTabID
	t.tabs[tabID] = &tab{id: tabID, targetID: target.TargetID, title: target.Title, url: target.URL}
	t.tabsByTarget[target.TargetID] = tabID
	t.trackingTarget[target.TargetID] = true
	t.bindSessionsLocked()
	t.stateMu.Unlock()
	t.signalChanged()
	return tabID, true
}

func (t *Tracker) attachPage(tabID int, target targetInfo) {
	windowCtx, cancel := context.WithTimeout(t.ctx, 2*time.Second)
	raw, err := t.protocol.Send(windowCtx, "Browser.getWindowForTarget", map[string]any{"targetId": target.TargetID}, "")
	cancel()
	windowAssigned := false
	if err == nil {
		var result struct {
			WindowID int64 `json:"windowId"`
		}
		if json.Unmarshal(raw, &result) == nil {
			t.assignWindow(target.TargetID, result.WindowID)
			windowAssigned = true
		}
	}
	if !windowAssigned {
		t.assignFallbackWindow(target.TargetID, tabID)
	}
	attachCtx, cancel := context.WithTimeout(t.ctx, 5*time.Second)
	_, err = t.protocol.Send(attachCtx, "Target.autoAttachRelated", map[string]any{
		"targetId":               target.TargetID,
		"waitForDebuggerOnStart": false,
		"filter": []map[string]any{
			{"type": "page"},
			{"type": "iframe"},
		},
	}, "")
	cancel()
	if err == nil {
		return
	}
	t.stateMu.Lock()
	if t.tabsByTarget[target.TargetID] == tabID {
		t.trackingTarget[target.TargetID] = false
	}
	t.stateMu.Unlock()
	t.signalChanged()
	if !t.protocol.IsClosed() {
		t.logger.Warn("failed to attach browser tab", "tab_id", tabID, "err", err)
	}
}

func (t *Tracker) RefreshTargets(ctx context.Context) error {
	t.stateMu.RLock()
	known := make([]string, 0, len(t.tabsByTarget))
	for targetID := range t.tabsByTarget {
		known = append(known, targetID)
	}
	t.stateMu.RUnlock()

	raw, err := t.protocol.Send(ctx, "Target.getTargets", nil, "")
	if err != nil {
		return err
	}
	var result struct {
		TargetInfos []targetInfo `json:"targetInfos"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return err
	}
	active := make(map[string]bool)
	for _, target := range result.TargetInfos {
		if target.Type != "page" {
			continue
		}
		active[target.TargetID] = true
		t.trackPage(target, true)
	}
	for _, targetID := range known {
		if active[targetID] {
			continue
		}
		t.removeTarget(targetID)
	}
	return nil
}

func (t *Tracker) assignFallbackWindow(targetID string, tabID int) {
	t.stateMu.RLock()
	tracked := t.tabs[t.tabsByTarget[targetID]]
	assigned := tracked != nil && tracked.windowID != 0
	t.stateMu.RUnlock()
	if !assigned {
		t.assignWindow(targetID, -int64(tabID))
	}
}

func (t *Tracker) assignWindow(targetID string, rawWindowID int64) {
	t.stateMu.Lock()
	defer t.stateMu.Unlock()
	tabID := t.tabsByTarget[targetID]
	trackedTab := t.tabs[tabID]
	if trackedTab == nil {
		return
	}
	oldRawWindowID := trackedTab.rawWindowID
	trackedWindow, ok := t.windows[rawWindowID]
	if !ok {
		t.nextWindowID++
		trackedWindow = window{id: t.nextWindowID, rawID: rawWindowID}
		t.windows[rawWindowID] = trackedWindow
	}
	trackedTab.windowID = trackedWindow.id
	trackedTab.rawWindowID = rawWindowID
	if oldRawWindowID != rawWindowID {
		t.removeEmptyWindowLocked(oldRawWindowID)
	}
	t.signalChanged()
}

func (t *Tracker) updateTarget(target targetInfo) {
	t.stateMu.Lock()
	_, knownTab := t.tabsByTarget[target.TargetID]
	t.updateTabTargetLocked(target)
	for _, sess := range t.sessions {
		if sess.target.TargetID == target.TargetID {
			sess.target = target
		}
	}
	t.stateMu.Unlock()
	t.signalChanged()
	if target.Type == "page" && !knownTab {
		t.trackPage(target, true)
		return
	}
	if target.Type == "page" {
		go func() {
			ctx, cancel := context.WithTimeout(t.ctx, 2*time.Second)
			defer cancel()
			raw, err := t.protocol.Send(ctx, "Browser.getWindowForTarget", map[string]any{"targetId": target.TargetID}, "")
			if err != nil {
				return
			}
			var result struct {
				WindowID int64 `json:"windowId"`
			}
			if json.Unmarshal(raw, &result) == nil {
				t.assignWindow(target.TargetID, result.WindowID)
			}
		}()
	}
}

func (t *Tracker) updateTabTargetLocked(target targetInfo) {
	if tracked := t.tabs[t.tabsByTarget[target.TargetID]]; tracked != nil {
		tracked.title = target.Title
		tracked.url = target.URL
	}
}

func (t *Tracker) removeTarget(targetID string) {
	t.stateMu.Lock()
	var removedSessions []string
	if tabID := t.tabsByTarget[targetID]; tabID != 0 {
		removedSessions = t.removeTabLocked(tabID)
	} else {
		for id, sess := range t.sessions {
			if sess.target.TargetID == targetID {
				removedSessions = append(removedSessions, t.removeSessionLocked(id)...)
			}
		}
	}
	t.stateMu.Unlock()
	for _, sessionID := range unique(removedSessions) {
		t.publish(Event{Kind: EventSessionRemoved, SessionID: sessionID})
	}
	t.signalChanged()
}

func (t *Tracker) removeTabLocked(tabID int) []string {
	trackedTab := t.tabs[tabID]
	if trackedTab == nil {
		return nil
	}
	var removed []string
	for id, sess := range t.sessions {
		if sess.tabID == tabID {
			removed = append(removed, t.removeSessionLocked(id)...)
		}
	}
	for rawID, trackedFrame := range t.frames {
		if trackedFrame.tabID == tabID {
			delete(t.frames, rawID)
		}
	}
	delete(t.tabsByTarget, trackedTab.targetID)
	delete(t.trackingTarget, trackedTab.targetID)
	delete(t.tabs, tabID)
	t.removeEmptyWindowLocked(trackedTab.rawWindowID)
	return removed
}

func (t *Tracker) removeEmptyWindowLocked(rawWindowID int64) {
	for _, trackedTab := range t.tabs {
		if trackedTab.rawWindowID == rawWindowID {
			return
		}
	}
	delete(t.windows, rawWindowID)
}

func unique(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}
