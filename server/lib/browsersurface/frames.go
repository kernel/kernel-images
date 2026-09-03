package browsersurface

import (
	"context"
	"encoding/json"
	"sort"
	"time"
)

func (t *Tracker) addSession(sessionID, parentSessionID string, target targetInfo) {
	if target.Type != "page" && target.Type != "iframe" {
		return
	}
	t.stateMu.Lock()
	if _, exists := t.sessions[sessionID]; exists {
		t.stateMu.Unlock()
		return
	}
	tabID := t.tabsByTarget[target.TargetID]
	if tabID == 0 && target.Type == "iframe" && parentSessionID != "" {
		if parent := t.sessions[parentSessionID]; parent != nil {
			tabID = parent.tabID
		}
	}
	if tabID == 0 && target.Type == "iframe" && target.ParentFrameID != "" {
		if parentFrame := t.frames[target.ParentFrameID]; parentFrame != nil {
			tabID = parentFrame.tabID
		}
	}
	if tabID == 0 && target.Type == "iframe" {
		if ownFrame := t.frames[target.TargetID]; ownFrame != nil {
			tabID = ownFrame.tabID
		}
	}
	ownedParentID := ""
	if target.Type == "iframe" {
		ownedParentID = parentSessionID
	}
	t.sessions[sessionID] = &session{id: sessionID, parentID: ownedParentID, target: target, tabID: tabID}
	t.bindSessionsLocked()
	t.stateMu.Unlock()
	t.signalChanged()
	go t.initializeSession(sessionID)
}

func (t *Tracker) initializeSession(sessionID string) {
	deadline := time.Now().Add(sessionInitTimeout)
	var sess session
	for {
		t.stateMu.Lock()
		t.bindSessionsLocked()
		tracked := t.sessions[sessionID]
		if tracked == nil || tracked.initialized || tracked.initializing {
			t.stateMu.Unlock()
			return
		}
		parentReady := true
		if parent := t.sessions[tracked.parentID]; parent != nil {
			parentReady = parent.initialized
		}
		if tracked.tabID != 0 && parentReady {
			tracked.initializing = true
			sess = *tracked
			t.stateMu.Unlock()
			break
		}
		t.stateMu.Unlock()
		if time.Now().After(deadline) {
			t.removeSession(sessionID)
			return
		}
		if t.protocol.IsClosed() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(t.ctx, sessionInitTimeout)
	defer cancel()
	var result struct {
		FrameTree frameTree `json:"frameTree"`
	}
	var initErr error
	for {
		if !t.SessionExists(sessionID) {
			return
		}
		result.FrameTree = frameTree{}
		if _, initErr = t.protocol.Send(ctx, "Page.enable", nil, sessionID); initErr == nil {
			var raw json.RawMessage
			raw, initErr = t.protocol.Send(ctx, "Page.getFrameTree", nil, sessionID)
			if initErr == nil {
				initErr = json.Unmarshal(raw, &result)
			}
		}
		if initErr == nil {
			break
		}
		if !t.SessionExists(sessionID) {
			return
		}

		timer := time.NewTimer(sessionInitRetryDelay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			t.failSessionInitialization(sessionID, initErr)
			return
		case <-t.closed:
			timer.Stop()
			t.failSessionInitialization(sessionID, initErr)
			return
		}
	}

	t.stateMu.Lock()
	tracked := t.sessions[sessionID]
	if tracked == nil {
		t.stateMu.Unlock()
		return
	}
	if result.FrameTree.Frame.ParentID == "" && sess.target.Type == "iframe" {
		result.FrameTree.Frame.ParentID = sess.target.ParentFrameID
		if existing := t.frames[result.FrameTree.Frame.ID]; existing != nil && existing.parentID != "" {
			result.FrameTree.Frame.ParentID = existing.parentID
		}
	}
	t.addFrameTreeLocked(sess.tabID, result.FrameTree)
	if sess.target.Type == "page" {
		if trackedTab := t.tabs[sess.tabID]; trackedTab != nil {
			trackedTab.rootFrameID = result.FrameTree.Frame.ID
		}
	}
	tracked.initializing = false
	tracked.initialized = true
	t.bindSessionsLocked()
	t.stateMu.Unlock()
	t.signalChanged()
	t.publish(Event{Kind: EventSessionReady, SessionID: sessionID})
}

func (t *Tracker) failSessionInitialization(sessionID string, err error) {
	t.removeSession(sessionID)
	if !t.protocol.IsClosed() {
		ctx, cancel := context.WithTimeout(t.ctx, time.Second)
		_, _ = t.protocol.Send(ctx, "Target.detachFromTarget", map[string]any{"sessionId": sessionID}, "")
		cancel()
		t.logger.Warn("failed to initialize browser surface session", "session_id", sessionID, "err", err)
	}
}

func (t *Tracker) addFrameTreeLocked(tabID int, tree frameTree) {
	t.upsertFrameLocked(tabID, tree.Frame)
	for _, child := range tree.ChildFrames {
		t.addFrameTreeLocked(tabID, child)
	}
}

func (t *Tracker) upsertFrameLocked(tabID int, info frameInfo) {
	tracked := t.frames[info.ID]
	if tracked == nil {
		publicID := 0
		if info.ParentID != "" {
			t.nextFrameID++
			publicID = t.nextFrameID
		}
		tracked = &frame{id: publicID, rawID: info.ID, tabID: tabID}
		t.frames[info.ID] = tracked
	} else if tracked.id == 0 && info.ParentID != "" {
		t.nextFrameID++
		tracked.id = t.nextFrameID
	}
	tracked.parentID = info.ParentID
	tracked.tabID = tabID
	tracked.url = info.URL
}

func (t *Tracker) bindSessionsLocked() {
	for _, sess := range t.sessions {
		if sess.tabID != 0 {
			continue
		}
		if tabID := t.tabsByTarget[sess.target.TargetID]; tabID != 0 {
			sess.tabID = tabID
			continue
		}
		if sess.target.Type == "iframe" {
			if parent := t.sessions[sess.parentID]; parent != nil && parent.tabID != 0 {
				sess.tabID = parent.tabID
				continue
			}
			if parentFrame := t.frames[sess.target.ParentFrameID]; parentFrame != nil {
				sess.tabID = parentFrame.tabID
				continue
			}
			if ownFrame := t.frames[sess.target.TargetID]; ownFrame != nil {
				sess.tabID = ownFrame.tabID
			}
		}
	}
}

func (t *Tracker) attachFrame(sessionID, frameID, parentFrameID string) {
	t.stateMu.Lock()
	if sess := t.sessions[sessionID]; sess != nil && sess.tabID != 0 {
		t.upsertFrameLocked(sess.tabID, frameInfo{ID: frameID, ParentID: parentFrameID})
		t.bindSessionsLocked()
	}
	t.stateMu.Unlock()
	t.signalChanged()
}

func (t *Tracker) navigateFrame(sessionID string, info frameInfo) {
	var removed []string
	t.stateMu.Lock()
	if sess := t.sessions[sessionID]; sess != nil && sess.tabID != 0 {
		removed = t.removeFrameChildrenLocked(info.ID)
		if info.ParentID == "" && sess.target.Type == "iframe" {
			if existing := t.frames[info.ID]; existing != nil {
				info.ParentID = existing.parentID
			}
		}
		t.upsertFrameLocked(sess.tabID, info)
		if trackedTab := t.tabs[sess.tabID]; trackedTab != nil && trackedTab.rootFrameID == info.ID {
			trackedTab.url = info.URL
		}
	}
	t.stateMu.Unlock()
	for _, frameID := range removed {
		t.publish(Event{Kind: EventFrameInvalidated, FrameID: frameID})
	}
	t.publish(Event{Kind: EventDocumentChanged, SessionID: sessionID, FrameID: info.ID})
	t.signalChanged()
}

func (t *Tracker) removeFrame(frameID string) {
	t.stateMu.Lock()
	removed := t.removeFrameChildrenLocked(frameID)
	if _, exists := t.frames[frameID]; exists {
		removed = append(removed, frameID)
		delete(t.frames, frameID)
	}
	t.stateMu.Unlock()
	for _, removedID := range removed {
		t.publish(Event{Kind: EventFrameRemoved, FrameID: removedID})
	}
	t.signalChanged()
}

func (t *Tracker) removeFrameChildrenLocked(parentID string) []string {
	var removed []string
	for changed := true; changed; {
		changed = false
		for rawID, tracked := range t.frames {
			if tracked.parentID == parentID || contains(removed, tracked.parentID) {
				removed = append(removed, rawID)
				delete(t.frames, rawID)
				changed = true
			}
		}
	}
	return removed
}

func (t *Tracker) removeSession(sessionID string) {
	t.stateMu.Lock()
	removed := t.removeSessionLocked(sessionID)
	t.stateMu.Unlock()
	for _, id := range removed {
		t.publish(Event{Kind: EventSessionRemoved, SessionID: id})
	}
	t.signalChanged()
}

func (t *Tracker) removeSessionLocked(sessionID string) []string {
	toRemove := map[string]bool{sessionID: true}
	for changed := true; changed; {
		changed = false
		for id, sess := range t.sessions {
			if toRemove[sess.parentID] && !toRemove[id] {
				toRemove[id] = true
				changed = true
			}
		}
	}
	removed := make([]string, 0, len(toRemove))
	for id := range toRemove {
		if sess := t.sessions[id]; sess != nil {
			switch sess.target.Type {
			case "page":
				t.trackingTarget[sess.target.TargetID] = false
			case "iframe":
				delete(t.trackingFrameTarget, sess.target.TargetID)
			}
			delete(t.sessions, id)
			removed = append(removed, id)
		}
	}
	sort.Strings(removed)
	return removed
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
