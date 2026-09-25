package webmcpclient

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/kernel/kernel-images/server/lib/browsersurface"
	"github.com/nrednav/cuid2"
)

// Sites that ship their own WebMCP tools before the browser exposes the
// native registry install a polyfill on navigator.modelContext. Chromium
// dropped that alias in favor of document.modelContext, so nothing such a
// polyfill registers reaches the CDP WebMCP domain. polyfill_page.js reads and
// invokes those tools from the page's main world without leaving anything
// behind on the page.
//
//go:embed polyfill_page.js
var polyfillPageSource string

const (
	polyfillSyncTimeout     = 2 * time.Second
	polyfillSyncConcurrency = 8
	polyfillReleaseTimeout  = time.Second
)

type polyfillToolMetadata struct {
	Name         string          `json:"name"`
	Title        string          `json:"title"`
	Description  string          `json:"description"`
	InputSchema  map[string]any  `json:"inputSchema"`
	OutputSchema map[string]any  `json:"outputSchema"`
	Annotations  map[string]bool `json:"annotations"`
}

type polyfillInvokeResult struct {
	OK     bool    `json:"ok"`
	Output *string `json:"output"`
	Error  string  `json:"error"`
}

type polyfillFrameTools struct {
	frame browsersurface.SessionFrame
	tools []polyfillToolMetadata
	read  bool
}

func polyfillToolKey(sessionID, frameID, name string) string {
	return toolKey(sessionID, frameID, name) + "\x00polyfill"
}

// polyfillFrameURL excludes browser-internal documents; every web document,
// including about:blank and srcdoc frames that inherit their parent's origin,
// can carry a polyfill.
func polyfillFrameURL(url string) bool {
	return !strings.HasPrefix(url, "chrome") && !strings.HasPrefix(url, "devtools://")
}

// syncPolyfillTools reads polyfill-registered tools from every tracked frame
// and reconciles them with the registry. A frame that cannot be read keeps its
// previous entries; surface events remove them when the document goes away.
func (c *connection) syncPolyfillTools(ctx context.Context) {
	frames := c.surface.SessionFrames()
	if len(frames) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, polyfillSyncTimeout)
	defer cancel()

	results := make([]polyfillFrameTools, len(frames))
	var wg sync.WaitGroup
	semaphore := make(chan struct{}, polyfillSyncConcurrency)
	for i, frame := range frames {
		if !polyfillFrameURL(frame.URL) {
			continue
		}
		wg.Add(1)
		go func(i int, frame browsersurface.SessionFrame) {
			defer wg.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()
			tools, err := c.readPolyfillTools(ctx, frame)
			if err != nil {
				return
			}
			results[i] = polyfillFrameTools{frame: frame, tools: tools, read: true}
		}(i, frame)
	}
	wg.Wait()

	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	for _, result := range results {
		if result.read {
			c.applyPolyfillToolsLocked(result.frame, result.tools)
		}
	}
}

func (c *connection) readPolyfillTools(ctx context.Context, frame browsersurface.SessionFrame) ([]polyfillToolMetadata, error) {
	raw, err := c.callPolyfill(ctx, frame, "list", nil, false)
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, nil
	}
	var result struct {
		Tools []polyfillToolMetadata `json:"tools"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("WebMCP: decode polyfill tools: %w", err)
	}
	return result.Tools, nil
}

func (c *connection) applyPolyfillToolsLocked(frame browsersurface.SessionFrame, tools []polyfillToolMetadata) {
	if !c.surface.SessionExists(frame.SessionID) {
		return
	}
	native := make(map[string]bool)
	tracked := 0
	for _, existing := range c.tools {
		if existing.sessionID != frame.SessionID {
			continue
		}
		tracked++
		if !existing.polyfill && existing.customID == "" && existing.frameID == frame.FrameID {
			native[existing.name] = true
		}
	}
	desired := make(map[string]polyfillToolMetadata, len(tools))
	for _, tool := range tools {
		if !native[tool.Name] {
			desired[tool.Name] = tool
		}
	}

	for ref, existing := range c.tools {
		if !existing.polyfill || existing.sessionID != frame.SessionID || existing.frameID != frame.FrameID {
			continue
		}
		if _, keep := desired[existing.name]; keep {
			continue
		}
		delete(c.toolRefs, existing.key())
		delete(c.tools, ref)
		tracked--
	}
	for _, tool := range tools {
		if _, keep := desired[tool.Name]; !keep {
			continue
		}
		key := polyfillToolKey(frame.SessionID, frame.FrameID, tool.Name)
		ref := c.toolRefs[key]
		if ref == "" {
			if tracked >= maxToolsPerSession {
				if !c.toolLimitWarned[frame.SessionID] {
					c.toolLimitWarned[frame.SessionID] = true
					c.logger.Warn("WebMCP tool limit reached", "session_id", frame.SessionID, "limit", maxToolsPerSession)
				}
				continue
			}
			ref = "wmcp_" + cuid2.Generate()
			c.toolRefs[key] = ref
			tracked++
		}
		c.tools[ref] = &registeredTool{
			ref:            ref,
			sessionID:      frame.SessionID,
			name:           tool.Name,
			registeredName: tool.Name,
			description:    tool.Description,
			inputSchema:    tool.InputSchema,
			frameID:        frame.FrameID,
			polyfill:       true,
			rootFrame:      frame.Root,
			title:          tool.Title,
			outputSchema:   tool.OutputSchema,
			hints:          tool.Annotations,
		}
	}
}

// polyfillWatch is released when the document that is running a polyfill
// invocation goes away.
type polyfillWatch struct {
	sessionID string
	frameID   string
	released  chan struct{}
}

func (c *connection) watchPolyfillDocument(sessionID, frameID string) (*polyfillWatch, func()) {
	watch := &polyfillWatch{sessionID: sessionID, frameID: frameID, released: make(chan struct{})}
	c.stateMu.Lock()
	c.polyfillWatches[watch] = struct{}{}
	c.stateMu.Unlock()
	return watch, func() {
		c.stateMu.Lock()
		delete(c.polyfillWatches, watch)
		c.stateMu.Unlock()
	}
}

func (c *connection) releasePolyfillWatchesLocked(matches func(*polyfillWatch) bool) {
	for watch := range c.polyfillWatches {
		if matches(watch) {
			close(watch.released)
			delete(c.polyfillWatches, watch)
		}
	}
}

func (c *connection) invokePolyfill(ctx context.Context, tool *registeredTool, input map[string]any) (InvocationResult, error) {
	frame := browsersurface.SessionFrame{SessionID: tool.sessionID, FrameID: tool.frameID, Root: tool.rootFrame}
	invocation := InvocationResult{InvocationID: cuid2.Generate()}

	// Chromium never answers a call whose document navigates before the
	// returned promise settles. The native WebMCP domain reports such an
	// invocation as completed with empty output, so watch the document and do
	// the same.
	watch, stopWatching := c.watchPolyfillDocument(tool.sessionID, tool.frameID)
	defer stopWatching()
	callCtx, cancelCall := context.WithCancel(ctx)
	defer cancelCall()
	type callResult struct {
		raw json.RawMessage
		err error
	}
	results := make(chan callResult, 1)
	go func() {
		raw, err := c.callPolyfill(callCtx, frame, "invoke", []any{tool.name, input}, true)
		results <- callResult{raw: raw, err: err}
	}()
	var raw json.RawMessage
	select {
	case result := <-results:
		raw = result.raw
		if result.err != nil {
			var notDispatched *polyfillNotDispatchedError
			if errors.As(result.err, &notDispatched) && ctx.Err() == nil {
				return InvocationResult{}, ErrToolNotFound
			}
			return invocation, ErrOutcomeUnknown
		}
	case <-watch.released:
		cancelCall()
		result := <-results
		if result.err != nil {
			invocation.Status = "completed"
			invocation.Output = []any{}
			return invocation, nil
		}
		raw = result.raw
	}
	var result polyfillInvokeResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return invocation, fmt.Errorf("WebMCP: decode polyfill invocation: %w", err)
	}
	if !result.OK {
		invocation.Status = "error"
		invocation.ErrorText = result.Error
		return invocation, nil
	}
	if result.Output != nil {
		if err := json.Unmarshal([]byte(*result.Output), &invocation.Output); err != nil {
			return invocation, fmt.Errorf("WebMCP: decode polyfill output: %w", err)
		}
	}
	invocation.Status = "completed"
	return invocation, nil
}

// polyfillNotDispatchedError reports a failure before the page function ran,
// so the caller knows the tool did not execute.
type polyfillNotDispatchedError struct {
	cause error
}

func (e *polyfillNotDispatchedError) Error() string { return e.cause.Error() }
func (e *polyfillNotDispatchedError) Unwrap() error { return e.cause }

// callPolyfill runs one entry point of polyfill_page.js in the frame's main
// world and returns the JSON value it produced, or nil for null.
func (c *connection) callPolyfill(ctx context.Context, frame browsersurface.SessionFrame, method string, args []any, userGesture bool) (json.RawMessage, error) {
	group := "kernel-webmcp-polyfill-" + cuid2.Generate()
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), polyfillReleaseTimeout)
		defer cancel()
		_, _ = c.surface.Send(releaseCtx, "Runtime.releaseObjectGroup", map[string]any{"objectGroup": group}, frame.SessionID)
	}()

	windowID, err := c.frameWindow(ctx, frame, group)
	if err != nil {
		return nil, &polyfillNotDispatchedError{cause: err}
	}
	arguments := make([]map[string]any, 0, len(args))
	for _, arg := range args {
		arguments = append(arguments, map[string]any{"value": arg})
	}
	raw, err := c.surface.Send(ctx, "Runtime.callFunctionOn", map[string]any{
		"objectId":            windowID,
		"functionDeclaration": fmt.Sprintf("function(...args) { return (%s).%s(this, ...args); }", polyfillPageSource, method),
		"arguments":           arguments,
		"awaitPromise":        true,
		"returnByValue":       true,
		"userGesture":         userGesture,
		"objectGroup":         group,
	}, frame.SessionID)
	if err != nil {
		return nil, err
	}
	var result struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text      string `json:"text"`
			Exception *struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("WebMCP: decode polyfill %s response: %w", method, err)
	}
	if details := result.ExceptionDetails; details != nil {
		description := details.Text
		if details.Exception != nil && details.Exception.Description != "" {
			description = details.Exception.Description
		}
		return nil, fmt.Errorf("WebMCP: polyfill %s threw: %s", method, description)
	}
	if len(result.Result.Value) == 0 || string(result.Result.Value) == "null" {
		return nil, nil
	}
	return result.Result.Value, nil
}

// frameWindow returns a remote object handle for the frame's Window. Root
// frames evaluate directly in their session; same-process child frames are
// reached through their owner element's contentWindow, which only succeeds
// for same-origin documents.
func (c *connection) frameWindow(ctx context.Context, frame browsersurface.SessionFrame, group string) (string, error) {
	if frame.Root {
		raw, err := c.surface.Send(ctx, "Runtime.evaluate", map[string]any{
			"expression":  "window",
			"objectGroup": group,
		}, frame.SessionID)
		if err != nil {
			return "", err
		}
		var result struct {
			Result struct {
				ObjectID string `json:"objectId"`
			} `json:"result"`
		}
		if err := json.Unmarshal(raw, &result); err != nil || result.Result.ObjectID == "" {
			return "", fmt.Errorf("WebMCP: frame window is unavailable")
		}
		return result.Result.ObjectID, nil
	}

	raw, err := c.surface.Send(ctx, "DOM.getFrameOwner", map[string]any{"frameId": frame.FrameID}, frame.SessionID)
	if err != nil {
		return "", err
	}
	var owner struct {
		BackendNodeID int `json:"backendNodeId"`
	}
	if err := json.Unmarshal(raw, &owner); err != nil || owner.BackendNodeID == 0 {
		return "", fmt.Errorf("WebMCP: frame owner is unavailable")
	}
	raw, err = c.surface.Send(ctx, "DOM.resolveNode", map[string]any{
		"backendNodeId": owner.BackendNodeID,
		"objectGroup":   group,
	}, frame.SessionID)
	if err != nil {
		return "", err
	}
	var node struct {
		Object struct {
			ObjectID string `json:"objectId"`
		} `json:"object"`
	}
	if err := json.Unmarshal(raw, &node); err != nil || node.Object.ObjectID == "" {
		return "", fmt.Errorf("WebMCP: frame owner is unavailable")
	}
	raw, err = c.surface.Send(ctx, "Runtime.callFunctionOn", map[string]any{
		"objectId":            node.Object.ObjectID,
		"functionDeclaration": "function() { return this.contentWindow; }",
		"objectGroup":         group,
	}, frame.SessionID)
	if err != nil {
		return "", err
	}
	var window struct {
		Result struct {
			ObjectID string `json:"objectId"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &window); err != nil || window.Result.ObjectID == "" {
		return "", fmt.Errorf("WebMCP: frame window is unavailable")
	}
	return window.Result.ObjectID, nil
}
