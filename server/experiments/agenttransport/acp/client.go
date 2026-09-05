// Package acp provides a deliberately small ACP v1 stdio test driver. It keeps
// the agent process alive across HTTP detach/attach; it is not a general SDK.
package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	transport "github.com/kernel/kernel-images/server/experiments/agenttransport"
)

type Config struct {
	Command    string            `json:"command"`
	Args       []string          `json:"args"`
	Cwd        string            `json:"cwd"`
	Env        []string          `json:"env"`
	InheritEnv []string          `json:"inheritEnv,omitempty"`
	MCPServers []json.RawMessage `json:"mcpServers"`
	AuthMethod string            `json:"authMethod,omitempty"`
	Setup      []SetupCall       `json:"setup,omitempty"`
}
type SetupCall struct {
	Method string         `json:"method"`
	Params map[string]any `json:"params"`
}
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}
type RPCError struct{ Detail string }

func (e *RPCError) Error() string { return "ACP request failed: " + e.Detail }

type received struct {
	raw json.RawMessage
	msg message
	err error
}
type Client struct {
	command       *exec.Cmd
	stdin         *os.File
	stdout        *os.File
	incoming      chan received
	stopped       chan struct{}
	exited        chan struct{}
	processCtx    context.Context
	processFailed context.CancelCauseFunc
	once          sync.Once
	sendMu        sync.Mutex
	callMu        sync.Mutex
	next          int
	sessionID     string
	Capabilities  json.RawMessage
	dispatches    atomic.Int32
	forcedStops   atomic.Int32
}

func Start(ctx context.Context, config Config, stderr io.Writer) (*Client, error) {
	if !filepath.IsAbs(config.Cwd) {
		return nil, errors.New("absolute workspace required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	command := exec.Command(config.Command, config.Args...)
	command.Dir = config.Cwd
	for _, name := range []string{"PATH", "HOME", "USER", "LANG", "TMPDIR", "TERM", "SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if value, ok := os.LookupEnv(name); ok {
			command.Env = append(command.Env, name+"="+value)
		}
	}
	for _, name := range config.InheritEnv {
		value, ok := os.LookupEnv(name)
		if !ok {
			return nil, fmt.Errorf("missing inherited environment variable %s", name)
		}
		command.Env = append(command.Env, name+"="+value)
	}
	command.Env = append(command.Env, config.Env...)
	command.Stderr = stderr
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	inputRead, inputWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outputRead, outputWrite, err := os.Pipe()
	if err != nil {
		inputRead.Close()
		inputWrite.Close()
		return nil, err
	}
	command.Stdin = inputRead
	command.Stdout = outputWrite
	if err := command.Start(); err != nil {
		inputRead.Close()
		inputWrite.Close()
		outputRead.Close()
		outputWrite.Close()
		return nil, err
	}
	inputRead.Close()
	outputWrite.Close()
	processCtx, processFailed := context.WithCancelCause(context.Background())
	c := &Client{command: command, stdin: inputWrite, stdout: outputRead, incoming: make(chan received, 64), stopped: make(chan struct{}), exited: make(chan struct{}), processCtx: processCtx, processFailed: processFailed}
	go func() {
		_ = command.Wait()
		c.processFailed(fmt.Errorf("%w: agent process exited", transport.ErrUncertain))
		close(c.exited)
	}()
	go c.read()
	fail := func(err error) (*Client, error) { c.Close(); return nil, err }
	result, err := c.call(ctx, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}, "clientInfo": map[string]string{"name": "transport-acceptance", "version": "1"}}, nil)
	if err != nil {
		return fail(err)
	}
	var initialized struct {
		ProtocolVersion int `json:"protocolVersion"`
	}
	if err := json.Unmarshal(result, &initialized); err != nil {
		return fail(err)
	}
	if initialized.ProtocolVersion != 1 {
		return fail(errors.New("test driver requires ACP v1"))
	}
	c.Capabilities = append(json.RawMessage(nil), result...)
	if config.AuthMethod != "" {
		if _, err := c.call(ctx, "authenticate", map[string]string{"methodId": config.AuthMethod}, nil); err != nil {
			return fail(err)
		}
	}
	servers := config.MCPServers
	if servers == nil {
		servers = make([]json.RawMessage, 0)
	}
	result, err = c.call(ctx, "session/new", map[string]any{"cwd": config.Cwd, "mcpServers": servers}, nil)
	if err != nil {
		return fail(err)
	}
	var session struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(result, &session); err != nil || session.SessionID == "" {
		return fail(errors.New("session/new did not return a session ID"))
	}
	c.sessionID = session.SessionID
	for _, setup := range config.Setup {
		switch setup.Method {
		case "session/set_config_option", "session/set_mode", "session/set_model":
		default:
			return fail(fmt.Errorf("unsupported setup method %q", setup.Method))
		}
		params := make(map[string]any)
		for key, value := range setup.Params {
			params[key] = value
		}
		params["sessionId"] = c.sessionID
		if _, err := c.call(ctx, setup.Method, params, nil); err != nil {
			return fail(err)
		}
	}
	return c, nil
}
func (c *Client) PID() int         { return c.command.Process.Pid }
func (c *Client) Dispatches() int  { return int(c.dispatches.Load()) }
func (c *Client) ForcedStops() int { return int(c.forcedStops.Load()) }
func (c *Client) Kill()            { _ = syscall.Kill(-c.PID(), syscall.SIGKILL) }
func (c *Client) Close() {
	c.once.Do(func() {
		c.processFailed(fmt.Errorf("%w: ACP client closed", transport.ErrUncertain))
		close(c.stopped)
		c.Kill()
		c.stdin.Close()
		c.stdout.Close()
		<-c.exited
	})
}

// Split only on LF. JSON strings can contain Unicode separators that must not
// be interpreted as framing, and partial final records are protocol errors.
func split(data []byte, atEOF bool) (int, []byte, error) {
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF && len(data) > 0 {
		return 0, nil, io.ErrUnexpectedEOF
	}
	return 0, nil, nil
}
func (c *Client) read() {
	defer c.processFailed(fmt.Errorf("%w: ACP stream ended", transport.ErrUncertain))
	scanner := bufio.NewScanner(c.stdout)
	scanner.Split(split)
	scanner.Buffer(make([]byte, 4096), 8<<20)
	for scanner.Scan() {
		raw := append(json.RawMessage(nil), scanner.Bytes()...)
		var msg message
		err := json.Unmarshal(raw, &msg)
		if err == nil && msg.JSONRPC != "2.0" {
			err = errors.New("invalid JSON-RPC version")
		}
		select {
		case c.incoming <- received{raw, msg, err}:
		case <-c.stopped:
			return
		}
		if err != nil {
			return
		}
	}
	err := scanner.Err()
	if err == nil {
		err = io.EOF
	}
	select {
	case c.incoming <- received{err: err}:
	case <-c.stopped:
	}
}
func (c *Client) send(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if err := c.stdin.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	n, err := c.stdin.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	return err
}
func (c *Client) request(method string, params any) (json.RawMessage, error) {
	c.next++
	id := json.RawMessage(fmt.Sprint(c.next))
	err := c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	return id, err
}
func (c *Client) call(ctx context.Context, method string, params any, turn *transport.Turn) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id, err := c.request(method, params)
	if err != nil {
		return nil, err
	}
	return c.receive(ctx, id, turn)
}
func (c *Client) receive(ctx context.Context, id json.RawMessage, turn *transport.Turn) (json.RawMessage, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-c.stopped:
			return nil, errors.New("ACP process closed")
		case event := <-c.incoming:
			if event.err != nil {
				return nil, event.err
			}
			msg := event.msg
			if msg.Method == "" {
				if !bytes.Equal(msg.ID, id) {
					return nil, errors.New("unexpected ACP response ID")
				}
				if len(msg.Error) > 0 {
					return nil, &RPCError{Detail: string(msg.Error)}
				}
				return msg.Result, nil
			}
			if msg.Method == "session/update" {
				var params struct {
					SessionID string `json:"sessionId"`
				}
				if err := json.Unmarshal(msg.Params, &params); err != nil {
					return nil, err
				}
				if c.sessionID != "" && params.SessionID != c.sessionID {
					return nil, errors.New("update for another ACP session")
				}
				if turn != nil {
					if err := turn.Emit("acp", event.raw); err != nil && ctx.Err() == nil {
						return nil, err
					}
				}
				continue
			}
			if len(msg.ID) == 0 {
				continue
			}
			if msg.Method != "session/request_permission" {
				if err := c.send(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "error": map[string]any{"code": -32601, "message": "test client does not implement this capability"}}); err != nil {
					return nil, err
				}
				continue
			}
			var params struct {
				SessionID string `json:"sessionId"`
			}
			if err := json.Unmarshal(msg.Params, &params); err != nil || params.SessionID != c.sessionID {
				return nil, errors.New("permission for another ACP session")
			}
			outcome := map[string]string{"outcome": "cancelled"}
			if turn != nil {
				permissionCtx, cancel := context.WithCancelCause(ctx)
				stop := context.AfterFunc(c.processCtx, func() { cancel(context.Cause(c.processCtx)) })
				option, err := turn.Permission(permissionCtx, string(msg.ID), msg.Params)
				stop()
				cancel(nil)
				if err == nil {
					outcome = map[string]string{"outcome": "selected", "optionId": option}
				} else if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
					return nil, err
				}
			}
			if err := c.send(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": map[string]any{"outcome": outcome}}); err != nil {
				return nil, err
			}
		}
	}
}
func (c *Client) Run(ctx context.Context, prompt string, turn *transport.Turn) error {
	c.callMu.Lock()
	defer c.callMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	c.dispatches.Add(1)
	id, err := c.request("session/prompt", map[string]any{"sessionId": c.sessionID, "prompt": []map[string]string{{"type": "text", "text": prompt}}})
	if err != nil {
		c.Close()
		return fmt.Errorf("%w: %v", transport.ErrUncertain, err)
	}
	result, err := c.receive(ctx, id, turn)
	if ctx.Err() != nil {
		_ = c.send(map[string]any{"jsonrpc": "2.0", "method": "session/cancel", "params": map[string]string{"sessionId": c.sessionID}})
		// Do not allow another prompt until the agent has stopped or been killed.
		drain, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if result == nil {
			if _, err := c.receive(drain, id, turn); err != nil {
				c.forcedStops.Add(1)
				c.Close()
			}
		}
		return ctx.Err()
	}
	if err != nil {
		var rpcError *RPCError
		if errors.As(err, &rpcError) {
			return err
		}
		c.Close()
		return fmt.Errorf("%w: %v", transport.ErrUncertain, err)
	}
	var completion struct {
		StopReason string `json:"stopReason"`
	}
	if err := json.Unmarshal(result, &completion); err != nil {
		return err
	}
	if completion.StopReason != "end_turn" {
		return fmt.Errorf("agent stopped with %q", completion.StopReason)
	}
	return nil
}
