package webmcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/kernel/kernel-images/server/lib/browsersurface"
	"github.com/kernel/kernel-images/server/lib/cdpclient"
	"github.com/nrednav/cuid2"
)

const (
	settleDelay             = 200 * time.Millisecond
	settleLimit             = 2 * time.Second
	maxToolsPerSession      = 256
	maxCompletedInvocations = 256
	maxAbandonedInvocations = 256
	maxDocumentChanges      = 1024
)

type connection struct {
	protocol *cdpclient.Client
	surface  *browsersurface.Tracker

	startMu sync.Mutex
	started bool

	stateMu              sync.RWMutex
	enabledSessions      map[string]bool
	tools                map[string]*registeredTool
	toolRefs             map[string]string
	toolLimitWarned      map[string]bool
	invocations          map[invocationKey]invocationResponse
	waitingInvocations   map[invocationKey]string
	abandonedInvocations map[invocationKey]time.Time
	documentChanges      map[documentKey]uint64
	eventSequence        uint64
	stateChangedCh       chan struct{}
	logger               *slog.Logger

	eventsCancel func()
	eventsDone   chan struct{}
	closed       chan struct{}
	closeOnce    sync.Once
}

func newConnection(protocol *cdpclient.Client) *connection {
	surface := browsersurface.New(protocol)
	events, cancel := surface.Subscribe()
	client := &connection{
		protocol:             protocol,
		surface:              surface,
		enabledSessions:      make(map[string]bool),
		tools:                make(map[string]*registeredTool),
		toolRefs:             make(map[string]string),
		toolLimitWarned:      make(map[string]bool),
		invocations:          make(map[invocationKey]invocationResponse),
		waitingInvocations:   make(map[invocationKey]string),
		abandonedInvocations: make(map[invocationKey]time.Time),
		documentChanges:      make(map[documentKey]uint64),
		stateChangedCh:       make(chan struct{}, 1),
		logger:               slog.Default(),
		eventsCancel:         cancel,
		eventsDone:           make(chan struct{}),
		closed:               make(chan struct{}),
	}
	go client.eventLoop(events)
	return client
}

func (c *connection) start(ctx context.Context) error {
	c.startMu.Lock()
	defer c.startMu.Unlock()
	if c.started {
		return nil
	}
	if err := c.surface.Start(ctx); err != nil {
		return err
	}
	c.started = true
	return nil
}

func (c *connection) close() error {
	c.closeOnce.Do(func() {
		c.eventsCancel()
		_ = c.protocol.Close()
		close(c.closed)
	})
	return nil
}

func (c *connection) isClosed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return c.protocol.IsClosed()
	}
}

func (c *connection) eventLoop(events <-chan browsersurface.Event) {
	defer close(c.eventsDone)
	for event := range events {
		switch event.Kind {
		case browsersurface.EventSessionReady:
			go c.enableSession(event.SessionID)
		case browsersurface.EventSessionRemoved:
			c.removeSession(event.SessionID)
		case browsersurface.EventDocumentInvalidated:
			c.stateMu.Lock()
			c.markDocumentChangedLocked(documentKey{sessionID: event.SessionID, frameID: event.FrameID})
			c.abandonFrameInvocationsLocked(event.SessionID, event.FrameID)
			c.stateMu.Unlock()
			c.signalStateChanged()
		case browsersurface.EventDocumentChanged:
			c.stateMu.Lock()
			c.removeFrameToolsLocked(event.SessionID, event.FrameID)
			c.stateMu.Unlock()
			c.signalStateChanged()
		case browsersurface.EventFrameRemoved:
			c.stateMu.Lock()
			for _, tool := range c.tools {
				if tool.frameID == event.FrameID {
					c.markDocumentChangedLocked(documentKey{sessionID: tool.sessionID, frameID: event.FrameID})
				}
			}
			c.abandonFrameInvocationsAcrossSessionsLocked(event.FrameID)
			c.removeFrameToolsAcrossSessionsLocked(event.FrameID)
			c.stateMu.Unlock()
			c.signalStateChanged()
		case browsersurface.EventProtocol:
			c.handleProtocolEvent(event.Message)
		}
	}
}

func (c *connection) enableSession(sessionID string) {
	c.stateMu.Lock()
	if c.enabledSessions[sessionID] {
		c.stateMu.Unlock()
		return
	}
	c.enabledSessions[sessionID] = true
	c.stateMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := c.surface.Send(ctx, "WebMCP.enable", nil, sessionID); err != nil {
		c.stateMu.Lock()
		delete(c.enabledSessions, sessionID)
		c.stateMu.Unlock()
		if !c.isClosed() && c.surface.SessionExists(sessionID) {
			c.logger.Warn("failed to enable WebMCP session", "session_id", sessionID, "err", err)
		}
		return
	}
	c.signalStateChanged()
}

func (c *connection) handleProtocolEvent(message cdpclient.Message) {
	switch message.Method {
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
			c.eventSequence++
			response.observedAt = c.eventSequence
			c.pruneAbandonedInvocationsLocked()
			if _, abandoned := c.abandonedInvocations[key]; !abandoned {
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

func (c *connection) addTools(sessionID string, tools []toolEvent) {
	if !c.surface.SessionExists(sessionID) {
		return
	}
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
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
		location, ok := c.surface.Resolve(tool.sessionID, tool.frameID)
		if !ok {
			continue
		}
		source := ToolSource{
			WindowID:  location.WindowID,
			TabID:     location.TabID,
			PageTitle: location.PageTitle,
			PageURL:   location.PageURL,
		}
		if location.Frame != nil {
			source.Frame = &ToolFrame{FrameID: location.Frame.ID, URL: location.Frame.URL}
		}
		result = append(result, Tool{
			Ref:         tool.ref,
			Name:        tool.name,
			Description: tool.description,
			InputSchema: tool.inputSchema,
			Annotations: tool.annotations,
			Source:      source,
		})
	}
	sort.Slice(result, func(i, j int) bool {
		left, right := result[i], result[j]
		if left.Source.WindowID != right.Source.WindowID {
			return left.Source.WindowID < right.Source.WindowID
		}
		if left.Source.TabID != right.Source.TabID {
			return left.Source.TabID < right.Source.TabID
		}
		leftFrame, rightFrame := 0, 0
		if left.Source.Frame != nil {
			leftFrame = left.Source.Frame.FrameID
		}
		if right.Source.Frame != nil {
			rightFrame = right.Source.Frame.FrameID
		}
		if leftFrame != rightFrame {
			return leftFrame < rightFrame
		}
		return left.Name < right.Name
	})
	return result
}

func (c *connection) waitForSettled(ctx context.Context) {
	limit := time.NewTimer(settleLimit)
	defer limit.Stop()
	quiet := time.NewTimer(settleDelay)
	defer quiet.Stop()
	for {
		select {
		case <-c.stateChangedCh:
			if !quiet.Stop() {
				select {
				case <-quiet.C:
				default:
				}
			}
			quiet.Reset(settleDelay)
		case <-quiet.C:
			return
		case <-limit.C:
			return
		case <-ctx.Done():
			return
		case <-c.closed:
			return
		}
	}
}

func (c *connection) invoke(ctx context.Context, toolRef string, input map[string]any) (InvocationResult, error) {
	c.stateMu.RLock()
	tool, ok := c.tools[toolRef]
	if !ok {
		c.stateMu.RUnlock()
		return InvocationResult{}, ErrToolNotFound
	}
	sessionID, frameID, name := tool.sessionID, tool.frameID, tool.name
	document := documentKey{sessionID: sessionID, frameID: frameID}
	startedAfterChange := c.documentChanges[document]
	c.stateMu.RUnlock()
	if !c.sessionExists(sessionID) {
		return InvocationResult{}, ErrToolNotFound
	}

	raw, err := c.surface.Send(ctx, "WebMCP.invokeTool", map[string]any{
		"frameId":  frameID,
		"toolName": name,
		"input":    input,
	}, sessionID)
	if err != nil {
		unknownOutcome := errors.Is(err, context.Canceled) ||
			errors.Is(err, context.DeadlineExceeded) ||
			errors.Is(err, cdpclient.ErrOutcomeUnknown) ||
			!c.sessionExists(sessionID)
		if unknownOutcome {
			return InvocationResult{}, ErrOutcomeUnknown
		}
		return InvocationResult{}, err
	}
	var started struct {
		InvocationID string `json:"invocationId"`
	}
	if err := json.Unmarshal(raw, &started); err != nil || started.InvocationID == "" {
		return InvocationResult{}, fmt.Errorf("WebMCP: invalid invokeTool response")
	}

	key := invocationKey{sessionID: sessionID, invocationID: started.InvocationID}
	c.stateMu.Lock()
	c.waitingInvocations[key] = frameID
	if changedAt := c.documentChanges[document]; changedAt > startedAfterChange {
		response, completed := c.invocations[key]
		if !completed || response.observedAt >= changedAt {
			delete(c.invocations, key)
			c.forceAbandonInvocationLocked(key)
			c.stateMu.Unlock()
			return InvocationResult{InvocationID: started.InvocationID}, ErrOutcomeUnknown
		}
	}
	c.stateMu.Unlock()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		c.stateMu.Lock()
		if response, ok := c.invocations[key]; ok {
			delete(c.invocations, key)
			delete(c.waitingInvocations, key)
			c.stateMu.Unlock()
			return InvocationResult{
				InvocationID: response.InvocationID,
				Status:       response.Status,
				Output:       response.Output,
				ErrorText:    response.ErrorText,
			}, nil
		}
		if _, abandoned := c.abandonedInvocations[key]; abandoned {
			c.stateMu.Unlock()
			return InvocationResult{InvocationID: started.InvocationID}, ErrOutcomeUnknown
		}
		c.stateMu.Unlock()
		if !c.sessionExists(sessionID) || c.executionClosed() {
			c.stateMu.Lock()
			abandoned := c.abandonInvocationLocked(key)
			c.stateMu.Unlock()
			if !abandoned {
				continue
			}
			return InvocationResult{InvocationID: started.InvocationID}, ErrOutcomeUnknown
		}
		select {
		case <-c.stateChangedCh:
		case <-ticker.C:
		case <-ctx.Done():
			c.stateMu.Lock()
			abandoned := c.abandonInvocationLocked(key)
			c.stateMu.Unlock()
			if !abandoned {
				continue
			}
			return InvocationResult{InvocationID: started.InvocationID}, ErrOutcomeUnknown
		case <-c.closed:
			c.stateMu.Lock()
			abandoned := c.abandonInvocationLocked(key)
			c.stateMu.Unlock()
			if !abandoned {
				continue
			}
			return InvocationResult{InvocationID: started.InvocationID}, ErrOutcomeUnknown
		}
	}
}

func (c *connection) executionClosed() bool {
	select {
	case <-c.closed:
		return true
	case <-c.eventsDone:
		return true
	default:
		return false
	}
}

func (c *connection) sessionExists(sessionID string) bool {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	return c.enabledSessions[sessionID]
}

func (c *connection) removeSession(sessionID string) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	delete(c.enabledSessions, sessionID)
	delete(c.toolLimitWarned, sessionID)
	for document := range c.documentChanges {
		if document.sessionID == sessionID {
			delete(c.documentChanges, document)
		}
	}
	for key := range c.waitingInvocations {
		if key.sessionID == sessionID {
			c.abandonInvocationLocked(key)
		}
	}
	for ref, tool := range c.tools {
		if tool.sessionID == sessionID {
			delete(c.toolRefs, toolKey(tool.sessionID, tool.frameID, tool.name))
			delete(c.tools, ref)
		}
	}
	c.signalStateChanged()
}

func (c *connection) markDocumentChangedLocked(document documentKey) {
	c.eventSequence++
	if _, exists := c.documentChanges[document]; !exists && len(c.documentChanges) >= maxDocumentChanges {
		var oldest documentKey
		oldestSequence := ^uint64(0)
		for candidate, sequence := range c.documentChanges {
			if sequence < oldestSequence {
				oldest = candidate
				oldestSequence = sequence
			}
		}
		delete(c.documentChanges, oldest)
	}
	c.documentChanges[document] = c.eventSequence
}

func (c *connection) abandonFrameInvocationsLocked(sessionID, frameID string) {
	for key, invocationFrameID := range c.waitingInvocations {
		if key.sessionID == sessionID && invocationFrameID == frameID {
			c.abandonInvocationLocked(key)
		}
	}
}

func (c *connection) abandonFrameInvocationsAcrossSessionsLocked(frameID string) {
	for key, invocationFrameID := range c.waitingInvocations {
		if invocationFrameID == frameID {
			c.abandonInvocationLocked(key)
		}
	}
}

func (c *connection) removeFrameToolsLocked(sessionID, frameID string) {
	for ref, tool := range c.tools {
		if tool.sessionID == sessionID && tool.frameID == frameID {
			delete(c.toolRefs, toolKey(tool.sessionID, tool.frameID, tool.name))
			delete(c.tools, ref)
		}
	}
}

func (c *connection) removeFrameToolsAcrossSessionsLocked(frameID string) {
	for ref, tool := range c.tools {
		if tool.frameID == frameID {
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
	c.forceAbandonInvocationLocked(key)
	return true
}

func (c *connection) forceAbandonInvocationLocked(key invocationKey) {
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

func toolKey(sessionID, frameID, name string) string {
	return sessionID + "\x00" + frameID + "\x00" + name
}
