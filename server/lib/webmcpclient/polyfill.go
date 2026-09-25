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
	"github.com/kernel/kernel-images/server/lib/cdpclient"
	"github.com/nrednav/cuid2"
)

// Sites that ship their own WebMCP tools before the browser exposes the
// native registry install a polyfill on navigator.modelContext. Chromium
// dropped that alias in favor of document.modelContext, so nothing such a
// polyfill registers reaches the CDP WebMCP domain. polyfill_bridge.js copies
// those tools into the frame's native registry, after which they are listed,
// invoked, and removed like any other page tool.
//
//go:embed polyfill_bridge.js
var polyfillBridgeSource string

const (
	polyfillSyncTimeout     = 2 * time.Second
	polyfillSyncConcurrency = 8
	polyfillReleaseTimeout  = time.Second
)

// polyfillBridge is a remote object handle for one frame's bridge. The handle
// lives in the frame's execution context and becomes invalid when it goes away.
type polyfillBridge struct {
	objectID string
	group    string
}

func polyfillBridgeKey(sessionID, frameID string) string {
	return sessionID + "\x00" + frameID
}

// polyfillFrameURL excludes browser-internal documents; every web document,
// including about:blank and srcdoc frames that inherit their parent's origin,
// can carry a polyfill.
func polyfillFrameURL(url string) bool {
	return !strings.HasPrefix(url, "chrome") && !strings.HasPrefix(url, "devtools://")
}

// syncPolyfillTools brings every tracked frame's bridged registrations up to
// date with its polyfill and reports whether any native registry changed.
func (c *connection) syncPolyfillTools(ctx context.Context) bool {
	// One bridge per document: concurrent listings would otherwise create two
	// and leave the first one's registrations unmanaged.
	c.polyfillSyncMu.Lock()
	defer c.polyfillSyncMu.Unlock()
	frames := c.surface.SessionFrames()
	live := make(map[string]bool, len(frames))
	for _, frame := range frames {
		live[polyfillBridgeKey(frame.SessionID, frame.FrameID)] = true
	}
	c.stateMu.Lock()
	for key := range c.polyfillBridges {
		if !live[key] {
			delete(c.polyfillBridges, key)
		}
	}
	c.stateMu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, polyfillSyncTimeout)
	defer cancel()
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		changed bool
	)
	semaphore := make(chan struct{}, polyfillSyncConcurrency)
	for _, frame := range frames {
		if !polyfillFrameURL(frame.URL) {
			continue
		}
		wg.Add(1)
		go func(frame browsersurface.SessionFrame) {
			defer wg.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()
			if c.syncPolyfillFrame(ctx, frame) {
				mu.Lock()
				changed = true
				mu.Unlock()
			}
		}(frame)
	}
	wg.Wait()
	return changed
}

func (c *connection) syncPolyfillFrame(ctx context.Context, frame browsersurface.SessionFrame) bool {
	// A handle from a replaced document fails once; the retry bridges the
	// frame's current document in the same listing.
	for attempt := 0; attempt < 2; attempt++ {
		changed, stale := c.syncPolyfillBridge(ctx, frame)
		if !stale {
			return changed
		}
	}
	return false
}

// syncPolyfillBridge syncs the frame's bridge, creating it when needed, and
// reports whether the bridge's handle was stale.
func (c *connection) syncPolyfillBridge(ctx context.Context, frame browsersurface.SessionFrame) (changed, stale bool) {
	key := polyfillBridgeKey(frame.SessionID, frame.FrameID)
	c.stateMu.RLock()
	bridge := c.polyfillBridges[key]
	c.stateMu.RUnlock()
	if bridge == nil {
		var err error
		if bridge, err = c.createPolyfillBridge(ctx, frame); err != nil {
			return false, false
		}
		c.stateMu.Lock()
		c.polyfillBridges[key] = bridge
		c.stateMu.Unlock()
	}

	raw, err := c.surface.Send(ctx, "Runtime.callFunctionOn", map[string]any{
		"objectId":            bridge.objectID,
		"functionDeclaration": "function() { return this.sync(); }",
		"awaitPromise":        true,
		"returnByValue":       true,
	}, frame.SessionID)
	if err == nil {
		var result evaluationResult
		if err = json.Unmarshal(raw, &result); err == nil && result.ExceptionDetails != nil {
			err = errors.New("WebMCP: polyfill bridge threw")
		}
		if err == nil {
			return string(result.Result.Value) == "true", false
		}
	}
	// A protocol error means the handle's document is gone. Timeouts keep the
	// handle, which may still own registrations.
	var protocolErr *cdpclient.Error
	if !errors.As(err, &protocolErr) {
		return false, false
	}
	c.stateMu.Lock()
	if c.polyfillBridges[key] == bridge {
		delete(c.polyfillBridges, key)
	}
	c.stateMu.Unlock()
	go c.releaseObjectGroup(frame.SessionID, bridge.group)
	return false, true
}

type evaluationResult struct {
	Result struct {
		ObjectID string          `json:"objectId"`
		Value    json.RawMessage `json:"value"`
	} `json:"result"`
	ExceptionDetails *json.RawMessage `json:"exceptionDetails"`
}

func (c *connection) createPolyfillBridge(ctx context.Context, frame browsersurface.SessionFrame) (*polyfillBridge, error) {
	group := "kernel-webmcp-polyfill-" + cuid2.Generate()
	windowID, err := c.frameWindow(ctx, frame, group)
	if err == nil {
		var raw json.RawMessage
		raw, err = c.surface.Send(ctx, "Runtime.callFunctionOn", map[string]any{
			"objectId":            windowID,
			"functionDeclaration": polyfillBridgeSource,
			"objectGroup":         group,
		}, frame.SessionID)
		if err == nil {
			var result evaluationResult
			if err = json.Unmarshal(raw, &result); err == nil {
				if result.ExceptionDetails != nil || result.Result.ObjectID == "" {
					err = errors.New("WebMCP: polyfill bridge is unavailable")
				} else {
					return &polyfillBridge{objectID: result.Result.ObjectID, group: group}, nil
				}
			}
		}
	}
	go c.releaseObjectGroup(frame.SessionID, group)
	return nil, err
}

func (c *connection) releaseObjectGroup(sessionID, group string) {
	ctx, cancel := context.WithTimeout(context.Background(), polyfillReleaseTimeout)
	defer cancel()
	_, _ = c.surface.Send(ctx, "Runtime.releaseObjectGroup", map[string]any{"objectGroup": group}, sessionID)
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
		var result evaluationResult
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
	var window evaluationResult
	if err := json.Unmarshal(raw, &window); err != nil || window.Result.ObjectID == "" {
		return "", fmt.Errorf("WebMCP: frame window is unavailable")
	}
	return window.Result.ObjectID, nil
}
