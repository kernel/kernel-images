package webmcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

const (
	settleDelay             = 200 * time.Millisecond
	settleLimit             = 2 * time.Second
	maxToolsPerSession      = 256
	maxCompletedInvocations = 256
	maxAbandonedInvocations = 256
)

var errCommandOutcomeUnknown = errors.New("CDP command outcome is unknown")

type connection struct {
	conn   *websocket.Conn
	ctx    context.Context
	cancel context.CancelFunc

	nextID  atomic.Int64
	writeMu sync.Mutex

	pendingMu sync.Mutex
	pending   map[int64]chan commandResult

	targetMu sync.Mutex

	stateMu              sync.RWMutex
	rootTargetID         string
	rootSessionID        string
	sessions             map[string]session
	frames               map[string]map[string]frameInfo
	tools                map[string]*registeredTool
	toolRefs             map[string]string
	toolLimitWarned      map[string]bool
	invocations          map[invocationKey]invocationResponse
	waitingInvocations   map[invocationKey]struct{}
	abandonedInvocations map[invocationKey]time.Time
	stateChangedCh       chan struct{}
	logger               *slog.Logger

	closed    chan struct{}
	closeOnce sync.Once
}

func dial(ctx context.Context, url string) (*connection, error) {
	ws, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return nil, fmt.Errorf("WebMCP: dial Chromium DevTools: %w", err)
	}
	ws.SetReadLimit(8 * 1024 * 1024)
	clientCtx, cancel := context.WithCancel(context.Background())
	c := &connection{
		conn:                 ws,
		ctx:                  clientCtx,
		cancel:               cancel,
		pending:              make(map[int64]chan commandResult),
		sessions:             make(map[string]session),
		frames:               make(map[string]map[string]frameInfo),
		tools:                make(map[string]*registeredTool),
		toolRefs:             make(map[string]string),
		toolLimitWarned:      make(map[string]bool),
		invocations:          make(map[invocationKey]invocationResponse),
		waitingInvocations:   make(map[invocationKey]struct{}),
		abandonedInvocations: make(map[invocationKey]time.Time),
		stateChangedCh:       make(chan struct{}, 1),
		logger:               slog.Default(),
		closed:               make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

func (c *connection) close() error {
	c.closeOnce.Do(func() {
		c.cancel()
		c.conn.CloseNow()
		close(c.closed)
		c.failPending(errCommandOutcomeUnknown)
	})
	return nil
}

func (c *connection) isClosed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

func (c *connection) readLoop() {
	for {
		_, payload, err := c.conn.Read(c.ctx)
		if err != nil {
			_ = c.close()
			return
		}
		var message cdpMessage
		if json.Unmarshal(payload, &message) != nil {
			continue
		}
		if message.ID != 0 {
			c.pendingMu.Lock()
			responseCh := c.pending[message.ID]
			delete(c.pending, message.ID)
			c.pendingMu.Unlock()
			if responseCh != nil {
				if message.Error != nil {
					responseCh <- commandResult{err: message.Error}
				} else {
					responseCh <- commandResult{result: message.Result}
				}
			}
			continue
		}
		c.handleEvent(message)
	}
}

func (c *connection) failPending(err error) {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	for id, responseCh := range c.pending {
		responseCh <- commandResult{err: err}
		delete(c.pending, id)
	}
}

func (c *connection) send(ctx context.Context, method string, params any, sessionID string) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	var rawParams json.RawMessage
	if params != nil {
		encoded, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("WebMCP: marshal %s parameters: %w", method, err)
		}
		rawParams = encoded
	}
	payload, err := json.Marshal(cdpRequest{ID: id, Method: method, Params: rawParams, SessionID: sessionID})
	if err != nil {
		return nil, fmt.Errorf("WebMCP: marshal %s request: %w", method, err)
	}
	responseCh := make(chan commandResult, 1)
	c.pendingMu.Lock()
	c.pending[id] = responseCh
	c.pendingMu.Unlock()

	c.writeMu.Lock()
	err = c.conn.Write(ctx, websocket.MessageText, payload)
	c.writeMu.Unlock()
	if err != nil {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return nil, fmt.Errorf("WebMCP: write %s: %w", method, err)
	}

	select {
	case response := <-responseCh:
		if response.err != nil {
			return nil, fmt.Errorf("WebMCP: %s: %w", method, response.err)
		}
		return response.result, nil
	case <-ctx.Done():
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return nil, fmt.Errorf("WebMCP: %s: %w", method, ctx.Err())
	case <-c.closed:
		return nil, fmt.Errorf("WebMCP: %s: %w", method, errCommandOutcomeUnknown)
	}
}

func (c *connection) selectTarget(ctx context.Context, requestedTargetID string) error {
	c.targetMu.Lock()
	defer c.targetMu.Unlock()

	raw, err := c.send(ctx, "Target.getTargets", nil, "")
	if err != nil {
		return err
	}
	var result struct {
		TargetInfos []targetInfo `json:"targetInfos"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("WebMCP: decode Target.getTargets: %w", err)
	}
	c.stateMu.RLock()
	currentTargetID := c.rootTargetID
	_, currentSessionAlive := c.sessions[c.rootSessionID]
	c.stateMu.RUnlock()
	var selected targetInfo
	if requestedTargetID == "" && currentSessionAlive {
		for _, target := range result.TargetInfos {
			if target.Type == "page" && target.TargetID == currentTargetID {
				selected = target
				break
			}
		}
	}
	if selected.TargetID == "" {
		for _, target := range result.TargetInfos {
			if target.Type != "page" {
				continue
			}
			if requestedTargetID == "" || requestedTargetID == target.TargetID {
				selected = target
				break
			}
		}
	}
	if selected.TargetID == "" {
		return ErrNoPageTarget
	}

	c.stateMu.RLock()
	alreadySelected := c.rootTargetID == selected.TargetID && c.sessions[c.rootSessionID].id != ""
	previousTargetID := c.rootTargetID
	c.stateMu.RUnlock()
	if alreadySelected {
		return nil
	}
	if previousTargetID != "" {
		_ = c.close()
		return errors.New("WebMCP: selected page target changed")
	}

	c.stateMu.Lock()
	c.clearStateLocked()
	c.rootTargetID = selected.TargetID
	c.stateMu.Unlock()
	if _, err := c.send(ctx, "Target.autoAttachRelated", map[string]any{
		"targetId":               selected.TargetID,
		"waitForDebuggerOnStart": false,
		"filter": []map[string]any{
			{"type": "page"},
			{"type": "iframe"},
		},
	}, ""); err != nil {
		return err
	}

	c.stateMu.Lock()
	for id, sess := range c.sessions {
		if sess.target.TargetID == selected.TargetID {
			c.rootSessionID = id
			break
		}
	}
	rootSessionID := c.rootSessionID
	c.stateMu.Unlock()
	if rootSessionID == "" {
		return fmt.Errorf("WebMCP: Chromium did not attach the selected page target")
	}
	return c.initializeSession(rootSessionID, selected.Type)
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
	_, sessionAlive := c.sessions[sessionID]
	c.stateMu.RUnlock()
	if !sessionAlive {
		return InvocationResult{}, ErrToolNotFound
	}

	raw, err := c.send(ctx, "WebMCP.invokeTool", map[string]any{
		"frameId":  frameID,
		"toolName": name,
		"input":    input,
	}, sessionID)
	if err != nil {
		unknownOutcome := errors.Is(err, context.Canceled) ||
			errors.Is(err, context.DeadlineExceeded) ||
			errors.Is(err, errCommandOutcomeUnknown) ||
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
	c.waitingInvocations[key] = struct{}{}
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
		_, alive := c.sessions[sessionID]
		c.stateMu.Unlock()
		if !alive || c.isClosed() {
			c.stateMu.Lock()
			c.abandonInvocationLocked(key)
			c.stateMu.Unlock()
			return InvocationResult{InvocationID: started.InvocationID}, ErrOutcomeUnknown
		}
		select {
		case <-c.stateChangedCh:
		case <-ticker.C:
		case <-ctx.Done():
			c.stateMu.Lock()
			c.abandonInvocationLocked(key)
			c.stateMu.Unlock()
			return InvocationResult{InvocationID: started.InvocationID}, ErrOutcomeUnknown
		case <-c.closed:
			c.stateMu.Lock()
			c.abandonInvocationLocked(key)
			c.stateMu.Unlock()
			return InvocationResult{InvocationID: started.InvocationID}, ErrOutcomeUnknown
		}
	}
}

func (c *connection) sessionExists(sessionID string) bool {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	_, ok := c.sessions[sessionID]
	return ok
}
