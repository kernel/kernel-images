package webmcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kernel/kernel-images/server/lib/browsersurface"
	"github.com/kernel/kernel-images/server/lib/cdpclient"
	"github.com/nrednav/cuid2"
)

const (
	customToolNamePrefix    = "custom."
	customToolIDLength      = len("ct_") + 24
	settleDelay             = 200 * time.Millisecond
	settleLimit             = 2 * time.Second
	maxToolsPerSession      = 256
	maxCompletedInvocations = 256
	maxAbandonedInvocations = 256
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
	polyfillWatches      map[*polyfillWatch]struct{}
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
		polyfillWatches:      make(map[*polyfillWatch]struct{}),
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
		case browsersurface.EventDocumentChanged:
			c.stateMu.Lock()
			c.removeFrameToolsLocked(event.SessionID, event.FrameID)
			c.releasePolyfillWatchesLocked(func(watch *polyfillWatch) bool {
				return watch.sessionID == event.SessionID && watch.frameID == event.FrameID
			})
			c.stateMu.Unlock()
			c.signalStateChanged()
		case browsersurface.EventFrameInvalidated:
			c.stateMu.Lock()
			c.removeFrameToolsAcrossSessionsLocked(event.FrameID)
			c.releasePolyfillWatchesLocked(func(watch *polyfillWatch) bool { return watch.frameID == event.FrameID })
			c.stateMu.Unlock()
			c.signalStateChanged()
		case browsersurface.EventFrameRemoved:
			c.stateMu.Lock()
			c.abandonFrameInvocationsAcrossSessionsLocked(event.FrameID)
			c.removeFrameToolsAcrossSessionsLocked(event.FrameID)
			c.releasePolyfillWatchesLocked(func(watch *polyfillWatch) bool { return watch.frameID == event.FrameID })
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

// customToolIdentity decodes the hidden name used for a custom registration.
// The browser registers custom.<ct_CUID2>.<name> to avoid collisions with page
// tools, while discovery exposes the original name and the generated ID separately.
// Names that do not match this reserved format are left intact.
func customToolIdentity(name string) (string, string) {
	if !strings.HasPrefix(name, customToolNamePrefix) {
		return "", name
	}
	remainder := strings.TrimPrefix(name, customToolNamePrefix)
	if len(remainder) <= customToolIDLength || remainder[customToolIDLength] != '.' {
		return "", name
	}
	id := remainder[:customToolIDLength]
	if !strings.HasPrefix(id, "ct_") {
		return "", name
	}
	if id[len("ct_")] < 'a' || id[len("ct_")] > 'z' {
		return "", name
	}
	for _, char := range id[len("ct_")+1:] {
		if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyz0123456789", char) {
			return "", name
		}
	}
	return id, remainder[customToolIDLength+1:]
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
		customID, name := customToolIdentity(tool.Name)
		c.tools[ref] = &registeredTool{
			ref:            ref,
			sessionID:      sessionID,
			name:           name,
			registeredName: tool.Name,
			description:    tool.Description,
			inputSchema:    tool.InputSchema,
			annotations:    tool.Annotations,
			customID:       customID,
			frameID:        tool.FrameID,
			declarative:    tool.BackendNodeID != nil,
		}
	}
	c.signalStateChanged()
}

func (c *connection) toolsSnapshot() []Tool {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	// A name registered both natively and through a polyfill lists once, as
	// the native tool.
	native := make(map[string]bool)
	for _, tool := range c.tools {
		if !tool.polyfill && tool.customID == "" {
			native[tool.key()] = true
		}
	}
	result := make([]Tool, 0, len(c.tools))
	for _, tool := range c.tools {
		if tool.polyfill && native[toolKey(tool.sessionID, tool.frameID, tool.name)] {
			continue
		}
		location, ok := c.surface.Resolve(tool.sessionID, tool.frameID)
		if !ok {
			continue
		}
		source := ToolSource{
			WindowID:  location.WindowID,
			TabID:     location.TabID,
			TargetID:  location.TargetID,
			PageTitle: location.PageTitle,
			PageURL:   location.PageURL,
		}
		if location.Frame != nil {
			source.Frame = &ToolFrame{FrameID: location.Frame.ID, URL: location.Frame.URL}
		}
		result = append(result, Tool{
			Ref:          tool.ref,
			Name:         tool.name,
			Description:  tool.description,
			InputSchema:  tool.inputSchema,
			Annotations:  tool.annotations,
			CustomID:     tool.customID,
			Polyfill:     tool.polyfill,
			Title:        tool.title,
			OutputSchema: tool.outputSchema,
			Hints:        tool.hints,
			Source:       source,
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
	if tool.polyfill {
		polyfillTool := *tool
		c.stateMu.RUnlock()
		if !c.surface.SessionExists(polyfillTool.sessionID) {
			return InvocationResult{}, ErrToolNotFound
		}
		return c.invokePolyfill(ctx, &polyfillTool, input)
	}
	sessionID, frameID, name := tool.sessionID, tool.frameID, tool.registeredName
	awaitingSubmission := tool.declarative && (tool.annotations == nil || !tool.annotations.Autosubmit)
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
	if awaitingSubmission {
		c.stateMu.Lock()
		if response, completed := c.invocations[key]; completed {
			delete(c.invocations, key)
			c.stateMu.Unlock()
			return InvocationResult{
				InvocationID: response.InvocationID,
				Status:       response.Status,
				Output:       response.Output,
				ErrorText:    response.ErrorText,
			}, nil
		}
		c.forceAbandonInvocationLocked(key)
		c.stateMu.Unlock()
		return InvocationResult{
			InvocationID: started.InvocationID,
			Status:       "awaiting_submission",
			Output: map[string]any{
				"form_populated": true,
				"submitted":      false,
			},
		}, nil
	}
	c.stateMu.Lock()
	c.waitingInvocations[key] = frameID
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
	c.releasePolyfillWatchesLocked(func(watch *polyfillWatch) bool { return watch.sessionID == sessionID })
	for key := range c.waitingInvocations {
		if key.sessionID == sessionID {
			c.abandonInvocationLocked(key)
		}
	}
	for ref, tool := range c.tools {
		if tool.sessionID == sessionID {
			delete(c.toolRefs, tool.key())
			delete(c.tools, ref)
		}
	}
	c.signalStateChanged()
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
			delete(c.toolRefs, tool.key())
			delete(c.tools, ref)
		}
	}
}

func (c *connection) removeFrameToolsAcrossSessionsLocked(frameID string) {
	for ref, tool := range c.tools {
		if tool.frameID == frameID {
			delete(c.toolRefs, tool.key())
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
