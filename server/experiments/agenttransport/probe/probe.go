// Package probe supplies a controlled MCP tool and deterministic ACP peer for
// the acceptance tests. Neither mode is a production agent or MCP server.
package probe

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Probe struct{ Dir string }

func (p Probe) Count() (int, error) {
	data, err := os.ReadFile(filepath.Join(p.Dir, "calls"))
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return strings.Count(string(data), "called\n"), nil
}
func (p Probe) Release() error {
	return os.WriteFile(filepath.Join(p.Dir, "release"), []byte("release"), 0600)
}
func (p Probe) Wait(ctx context.Context) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		count, err := p.Count()
		if err != nil {
			return err
		}
		if count > 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
func (p Probe) Checkpoint(ctx context.Context) error {
	file, err := os.OpenFile(filepath.Join(p.Dir, "calls"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	_, err = file.WriteString("called\n")
	if err == nil {
		err = file.Sync()
	}
	file.Close()
	if err != nil {
		return err
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		_, err := os.Stat(filepath.Join(p.Dir, "release"))
		if err == nil {
			return nil
		}
		if !os.IsNotExist(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

type request struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
}

func reply(out io.Writer, id json.RawMessage, result any) error {
	return json.NewEncoder(out).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}
func failure(out io.Writer, id json.RawMessage, text string) error {
	return json.NewEncoder(out).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32601, "message": text}})
}

// ServeMCP implements only the tool surface needed by the tests. The checkpoint
// records each invocation, then waits for the test controller's release file.
func ServeMCP(ctx context.Context, in io.Reader, out io.Writer, p Probe) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		var r request
		if err := json.Unmarshal(scanner.Bytes(), &r); err != nil {
			return err
		}
		if len(r.ID) == 0 {
			continue
		}
		var result any
		switch r.Method {
		case "initialize":
			var params struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			if err := json.Unmarshal(r.Params, &params); err != nil {
				return err
			}
			result = map[string]any{"protocolVersion": params.ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "acceptance-probe", "version": "1"}}
		case "ping":
			result = map[string]any{}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "checkpoint", "description": "Acceptance-test checkpoint. Call exactly once when instructed; waits for the test controller to release it.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}}}}
		case "tools/call":
			var params struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(r.Params, &params); err != nil {
				return err
			}
			if params.Name != "checkpoint" {
				if err := failure(out, r.ID, "unknown tool"); err != nil {
					return err
				}
				continue
			}
			if err := p.Checkpoint(ctx); err != nil {
				return err
			}
			result = map[string]any{"content": []any{map[string]string{"type": "text", "text": "checkpoint released; reply ACCEPTANCE_COMPLETE"}}, "isError": false}
		default:
			if err := failure(out, r.ID, "unsupported MCP method"); err != nil {
				return err
			}
			continue
		}
		if err := reply(out, r.ID, result); err != nil {
			return err
		}
	}
	return scanner.Err()
}
func update(out io.Writer, text string) error {
	return json.NewEncoder(out).Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "fixture-session", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": text}}}})
}

// ServeACP is a deterministic subprocess peer, not a model. It exercises the
// same stdio client used for real harnesses, including complete ACP payloads.
func ServeACP(ctx context.Context, in io.Reader, out io.Writer, p Probe) error {
	return serveACP(ctx, in, out, p, false)
}
func ServePermissionACP(ctx context.Context, in io.Reader, out io.Writer, p Probe) error {
	return serveACP(ctx, in, out, p, true)
}
func serveACP(ctx context.Context, in io.Reader, out io.Writer, p Probe, permission bool) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		var r request
		if err := json.Unmarshal(scanner.Bytes(), &r); err != nil {
			return err
		}
		switch r.Method {
		case "initialize":
			if err := reply(out, r.ID, map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{}, "agentInfo": map[string]string{"name": "deterministic-fixture", "version": "1"}}); err != nil {
				return err
			}
		case "session/new":
			if err := reply(out, r.ID, map[string]string{"sessionId": "fixture-session"}); err != nil {
				return err
			}
		case "session/prompt":
			if permission {
				permissionRequest := map[string]any{"jsonrpc": "2.0", "id": "permission-1", "method": "session/request_permission", "params": map[string]any{"sessionId": "fixture-session", "toolCall": map[string]any{"toolCallId": "checkpoint-1", "title": "checkpoint", "status": "pending"}, "options": []any{map[string]string{"optionId": "allow", "kind": "allow_once", "name": "Allow once"}}}}
				if err := json.NewEncoder(out).Encode(permissionRequest); err != nil {
					return err
				}
				if !scanner.Scan() {
					return io.ErrUnexpectedEOF
				}
				var decision request
				if err := json.Unmarshal(scanner.Bytes(), &decision); err != nil {
					return err
				}
				var result struct {
					Outcome struct {
						Outcome  string `json:"outcome"`
						OptionID string `json:"optionId"`
					} `json:"outcome"`
				}
				_ = json.Unmarshal(decision.Result, &result)
				if decision.Method == "session/cancel" || result.Outcome.Outcome == "cancelled" {
					if err := update(out, "cancelled checkpoint"); err != nil {
						return err
					}
					if err := reply(out, r.ID, map[string]string{"stopReason": "cancelled"}); err != nil {
						return err
					}
					continue
				}
				if string(decision.ID) != `"permission-1"` || result.Outcome.OptionID != "allow" {
					return errors.New("invalid permission response")
				}
			}
			if err := update(out, "before checkpoint"); err != nil {
				return err
			}
			if err := p.Checkpoint(ctx); err != nil {
				return err
			}
			if err := update(out, "ACCEPTANCE_COMPLETE"); err != nil {
				return err
			}
			if err := reply(out, r.ID, map[string]string{"stopReason": "end_turn"}); err != nil {
				return err
			}
		case "session/cancel":
			// A permission response may have completed cancellation already.
			continue
		default:
			if len(r.ID) > 0 {
				if err := failure(out, r.ID, fmt.Sprintf("unsupported fixture method %s", r.Method)); err != nil {
					return err
				}
			}
		}
	}
	return scanner.Err()
}
