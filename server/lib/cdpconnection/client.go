package cdpconnection

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/coder/websocket"
)

var ErrOutcomeUnknown = errors.New("CDP command outcome is unknown")

type Message struct {
	ID        int64           `json:"id,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *Error          `json:"error,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
}

type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("CDP error %d: %s", e.Code, e.Message)
}

type request struct {
	ID        int64           `json:"id"`
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
}

type commandResult struct {
	result json.RawMessage
	err    error
}

// Client maintains one event-aware browser-level CDP connection. Send may be
// called concurrently; Events delivers protocol events in receive order.
type Client struct {
	conn   *websocket.Conn
	ctx    context.Context
	cancel context.CancelFunc

	nextID  atomic.Int64
	writeMu sync.Mutex

	pendingMu sync.Mutex
	pending   map[int64]chan commandResult
	events    chan Message

	closed    chan struct{}
	closeOnce sync.Once
}

func Dial(ctx context.Context, url string) (*Client, error) {
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return nil, fmt.Errorf("dial Chromium DevTools: %w", err)
	}
	conn.SetReadLimit(8 * 1024 * 1024)
	clientCtx, cancel := context.WithCancel(context.Background())
	client := &Client{
		conn:    conn,
		ctx:     clientCtx,
		cancel:  cancel,
		pending: make(map[int64]chan commandResult),
		events:  make(chan Message, 256),
		closed:  make(chan struct{}),
	}
	go client.readLoop()
	return client, nil
}

func (c *Client) Send(ctx context.Context, method string, params any, sessionID string) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	var rawParams json.RawMessage
	if params != nil {
		encoded, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("marshal %s parameters: %w", method, err)
		}
		rawParams = encoded
	}
	payload, err := json.Marshal(request{ID: id, Method: method, Params: rawParams, SessionID: sessionID})
	if err != nil {
		return nil, fmt.Errorf("marshal %s request: %w", method, err)
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
		return nil, fmt.Errorf("write %s: %w", method, err)
	}

	select {
	case response := <-responseCh:
		if response.err != nil {
			return nil, fmt.Errorf("%s: %w", method, response.err)
		}
		return response.result, nil
	case <-ctx.Done():
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return nil, fmt.Errorf("%s: %w", method, ctx.Err())
	case <-c.closed:
		return nil, fmt.Errorf("%s: %w", method, ErrOutcomeUnknown)
	}
}

func (c *Client) Events() <-chan Message {
	return c.events
}

func (c *Client) Done() <-chan struct{} {
	return c.closed
}

func (c *Client) IsClosed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		c.cancel()
		c.conn.CloseNow()
		close(c.closed)
		c.failPending(ErrOutcomeUnknown)
	})
	return nil
}

func (c *Client) readLoop() {
	defer close(c.events)
	for {
		_, payload, err := c.conn.Read(c.ctx)
		if err != nil {
			_ = c.Close()
			return
		}
		var message Message
		if json.Unmarshal(payload, &message) != nil {
			continue
		}
		if message.ID == 0 {
			select {
			case c.events <- message:
			case <-c.closed:
				return
			}
			continue
		}
		c.pendingMu.Lock()
		responseCh := c.pending[message.ID]
		delete(c.pending, message.ID)
		c.pendingMu.Unlock()
		if responseCh == nil {
			continue
		}
		if message.Error != nil {
			responseCh <- commandResult{err: message.Error}
		} else {
			responseCh <- commandResult{result: message.Result}
		}
	}
}

func (c *Client) failPending(err error) {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	for id, responseCh := range c.pending {
		responseCh <- commandResult{err: err}
		delete(c.pending, id)
	}
}
