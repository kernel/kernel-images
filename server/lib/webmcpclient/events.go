package webmcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/nrednav/cuid2"
)

func (c *connection) handleEvent(message cdpMessage) {
	switch message.Method {
	case "Target.attachedToTarget":
		var event struct {
			SessionID  string     `json:"sessionId"`
			TargetInfo targetInfo `json:"targetInfo"`
		}
		if json.Unmarshal(message.Params, &event) != nil {
			return
		}
		c.stateMu.Lock()
		if message.SessionID != "" {
			if _, ok := c.sessions[message.SessionID]; !ok {
				c.stateMu.Unlock()
				return
			}
		}
		c.sessions[event.SessionID] = session{id: event.SessionID, parentID: message.SessionID, target: event.TargetInfo}
		c.stateMu.Unlock()
		c.signalStateChanged()
		c.stateMu.RLock()
		rootTargetID := c.rootTargetID
		c.stateMu.RUnlock()
		if event.TargetInfo.TargetID != rootTargetID {
			go func() {
				if err := c.initializeSession(event.SessionID, event.TargetInfo.Type); err != nil && !c.isClosed() {
					c.logger.Warn("failed to initialize WebMCP target",
						"target_id", event.TargetInfo.TargetID,
						"target_type", event.TargetInfo.Type,
						"err", err)
				}
			}()
		}
	case "Target.detachedFromTarget":
		var event struct {
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal(message.Params, &event) == nil {
			c.removeSession(event.SessionID)
		}
	case "Target.targetInfoChanged":
		var event struct {
			TargetInfo targetInfo `json:"targetInfo"`
		}
		if json.Unmarshal(message.Params, &event) == nil {
			c.stateMu.Lock()
			for id, sess := range c.sessions {
				if sess.target.TargetID == event.TargetInfo.TargetID {
					sess.target = event.TargetInfo
					c.sessions[id] = sess
				}
			}
			c.stateMu.Unlock()
			c.signalStateChanged()
		}
	case "Page.frameNavigated":
		var event struct {
			Frame frameInfo `json:"frame"`
		}
		if json.Unmarshal(message.Params, &event) == nil {
			c.stateMu.Lock()
			if _, ok := c.sessions[message.SessionID]; !ok {
				c.stateMu.Unlock()
				return
			}
			if c.frames[message.SessionID] == nil {
				c.frames[message.SessionID] = make(map[string]frameInfo)
			}
			c.frames[message.SessionID][event.Frame.ID] = event.Frame
			if sess, ok := c.sessions[message.SessionID]; ok && sess.target.TargetID == event.Frame.ID {
				sess.target.URL = event.Frame.URL
				c.sessions[message.SessionID] = sess
			}
			c.removeFrameToolsLocked(message.SessionID, event.Frame.ID)
			c.stateMu.Unlock()
			c.signalStateChanged()
		}
	case "Page.frameDetached":
		var event struct {
			FrameID string `json:"frameId"`
		}
		if json.Unmarshal(message.Params, &event) == nil {
			c.stateMu.Lock()
			delete(c.frames[message.SessionID], event.FrameID)
			c.removeFrameToolsLocked(message.SessionID, event.FrameID)
			c.stateMu.Unlock()
			c.signalStateChanged()
		}
	case "WebMCP.toolsAdded":
		var event struct {
			Tools []toolEvent `json:"tools"`
		}
		if json.Unmarshal(message.Params, &event) == nil {
			c.addTools(message.SessionID, event.Tools)
		}
	case "WebMCP.toolsRemoved":
		var event struct {
			Tools []struct {
				Name    string `json:"name"`
				FrameID string `json:"frameId"`
			} `json:"tools"`
		}
		if json.Unmarshal(message.Params, &event) == nil {
			c.stateMu.Lock()
			for _, tool := range event.Tools {
				c.removeToolLocked(toolKey(message.SessionID, tool.FrameID, tool.Name))
			}
			c.stateMu.Unlock()
			c.signalStateChanged()
		}
	case "WebMCP.toolResponded":
		var response invocationResponse
		if json.Unmarshal(message.Params, &response) == nil {
			key := invocationKey{sessionID: message.SessionID, invocationID: response.InvocationID}
			c.stateMu.Lock()
			c.pruneAbandonedInvocationsLocked()
			if _, abandoned := c.abandonedInvocations[key]; abandoned {
				delete(c.abandonedInvocations, key)
			} else {
				if len(c.invocations) >= maxCompletedInvocations {
					for existing := range c.invocations {
						if _, waiting := c.waitingInvocations[existing]; !waiting {
							delete(c.invocations, existing)
							break
						}
					}
				}
				c.invocations[key] = response
			}
			c.stateMu.Unlock()
			c.signalStateChanged()
		}
	}
}

func (c *connection) initializeSession(sessionID, targetType string) error {
	if targetType != "page" && targetType != "iframe" {
		return nil
	}
	ctx, cancel := context.WithTimeout(c.ctx, 5*time.Second)
	defer cancel()
	if _, err := c.send(ctx, "Page.enable", nil, sessionID); err != nil {
		return err
	}
	raw, err := c.send(ctx, "Page.getFrameTree", nil, sessionID)
	if err != nil {
		return err
	}
	var result struct {
		FrameTree frameTree `json:"frameTree"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("WebMCP: decode Page.getFrameTree: %w", err)
	}
	c.stateMu.Lock()
	if c.frames[sessionID] == nil {
		c.frames[sessionID] = make(map[string]frameInfo)
	}
	addFrameTree(c.frames[sessionID], result.FrameTree)
	c.stateMu.Unlock()
	if _, err := c.send(ctx, "WebMCP.enable", nil, sessionID); err != nil {
		return err
	}
	c.signalStateChanged()
	return nil
}

func addFrameTree(frames map[string]frameInfo, tree frameTree) {
	frames[tree.Frame.ID] = tree.Frame
	for _, child := range tree.ChildFrames {
		addFrameTree(frames, child)
	}
}

func (c *connection) addTools(sessionID string, tools []toolEvent) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if _, ok := c.sessions[sessionID]; !ok {
		return
	}
	tracked := 0
	for _, existing := range c.tools {
		if existing.sessionID == sessionID {
			tracked++
		}
	}
	for _, tool := range tools {
		key := toolKey(sessionID, tool.FrameID, tool.Name)
		ref := c.toolRefs[key]
		if ref == "" {
			if tracked >= maxToolsPerSession {
				if !c.toolLimitWarned[sessionID] {
					c.toolLimitWarned[sessionID] = true
					c.logger.Warn("WebMCP tool limit reached", "session_id", sessionID, "limit", maxToolsPerSession)
				}
				continue
			}
			ref = "wmcp_" + cuid2.Generate()
			c.toolRefs[key] = ref
			tracked++
		}
		c.tools[ref] = &registeredTool{
			ref:         ref,
			sessionID:   sessionID,
			name:        tool.Name,
			description: tool.Description,
			inputSchema: tool.InputSchema,
			annotations: tool.Annotations,
			frameID:     tool.FrameID,
		}
	}
	c.signalStateChanged()
}

func (c *connection) toolsSnapshot() []Tool {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	result := make([]Tool, 0, len(c.tools))
	for _, tool := range c.tools {
		sess, ok := c.sessions[tool.sessionID]
		if !ok {
			continue
		}
		frame := c.frames[tool.sessionID][tool.frameID]
		documentRef := ""
		if frame.LoaderID != "" {
			documentRef = sess.target.TargetID + ":" + frame.LoaderID
		}
		result = append(result, Tool{
			Ref:           tool.ref,
			Name:          tool.name,
			Description:   tool.description,
			InputSchema:   tool.inputSchema,
			Annotations:   tool.annotations,
			PageTargetID:  c.rootTargetID,
			TargetID:      sess.target.TargetID,
			TargetType:    sess.target.Type,
			TargetURL:     stripURLFragment(sess.target.URL),
			FrameID:       tool.frameID,
			FrameURL:      stripURLFragment(frame.URL),
			ParentFrameID: frame.ParentID,
			DocumentRef:   documentRef,
		})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].FrameID != result[j].FrameID {
			return result[i].FrameID < result[j].FrameID
		}
		return result[i].Name < result[j].Name
	})
	return result
}

func (c *connection) removeSession(sessionID string) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	toRemove := map[string]bool{sessionID: true}
	for changed := true; changed; {
		changed = false
		for id, sess := range c.sessions {
			if toRemove[sess.parentID] && !toRemove[id] {
				toRemove[id] = true
				changed = true
			}
		}
	}
	for id := range toRemove {
		delete(c.sessions, id)
		delete(c.frames, id)
		delete(c.toolLimitWarned, id)
		for key := range c.waitingInvocations {
			if key.sessionID == id {
				c.abandonInvocationLocked(key)
			}
		}
		for ref, tool := range c.tools {
			if tool.sessionID == id {
				delete(c.toolRefs, toolKey(tool.sessionID, tool.frameID, tool.name))
				delete(c.tools, ref)
			}
		}
	}
	c.signalStateChanged()
}

func (c *connection) clearStateLocked() {
	c.rootTargetID = ""
	c.rootSessionID = ""
	c.sessions = make(map[string]session)
	c.frames = make(map[string]map[string]frameInfo)
	c.tools = make(map[string]*registeredTool)
	c.toolRefs = make(map[string]string)
	c.toolLimitWarned = make(map[string]bool)
	c.invocations = make(map[invocationKey]invocationResponse)
	c.waitingInvocations = make(map[invocationKey]struct{})
	c.abandonedInvocations = make(map[invocationKey]time.Time)
}

func (c *connection) removeFrameToolsLocked(sessionID, frameID string) {
	for ref, tool := range c.tools {
		if tool.sessionID == sessionID && tool.frameID == frameID {
			delete(c.toolRefs, toolKey(tool.sessionID, tool.frameID, tool.name))
			delete(c.tools, ref)
		}
	}
}

func (c *connection) removeToolLocked(key string) {
	ref := c.toolRefs[key]
	delete(c.toolRefs, key)
	delete(c.tools, ref)
}

func (c *connection) abandonInvocationLocked(key invocationKey) bool {
	if _, completed := c.invocations[key]; completed {
		return false
	}
	delete(c.waitingInvocations, key)
	c.pruneAbandonedInvocationsLocked()
	if len(c.abandonedInvocations) >= maxAbandonedInvocations {
		var oldest invocationKey
		var oldestAt time.Time
		for candidate, abandonedAt := range c.abandonedInvocations {
			if oldestAt.IsZero() || abandonedAt.Before(oldestAt) {
				oldest = candidate
				oldestAt = abandonedAt
			}
		}
		delete(c.abandonedInvocations, oldest)
	}
	c.abandonedInvocations[key] = time.Now()
	return true
}

func (c *connection) pruneAbandonedInvocationsLocked() {
	cutoff := time.Now().Add(-10 * time.Minute)
	for invocationID, abandonedAt := range c.abandonedInvocations {
		if abandonedAt.Before(cutoff) {
			delete(c.abandonedInvocations, invocationID)
		}
	}
}

func (c *connection) signalStateChanged() {
	select {
	case c.stateChangedCh <- struct{}{}:
	default:
	}
}

func stripURLFragment(value string) string {
	withoutFragment, _, _ := strings.Cut(value, "#")
	return withoutFragment
}

func toolKey(sessionID, frameID, name string) string {
	return sessionID + "\x00" + frameID + "\x00" + name
}
