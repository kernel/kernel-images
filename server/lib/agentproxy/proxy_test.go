package agentproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type rpcMessage struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

func fixture(t *testing.T, limit int) (*Handler, string, string, context.CancelFunc) {
	t.Helper()
	remote := os.Getenv("AGENT_PROXY_TEST_ACPREMOTE")
	if remote == "" {
		t.Skip("set AGENT_PROXY_TEST_ACPREMOTE to the pinned acpremote venv executable")
	}
	peer, err := filepath.Abs("testdata/agent.py")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	config := Config{ACPRemote: remote, MaxConnections: limit, Harnesses: map[string]Harness{
		"pi": {Command: filepath.Join(filepath.Dir(remote), "python"), Args: []string{peer}, Cwd: dir, Env: map[string]string{"AGENT_PROXY_TEST_DIR": dir}},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	handler, err := New(ctx, config, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(func() { cancel(); server.Close(); eventually(t, func() bool { return len(handler.slots) == 0 }) })
	return handler, "ws" + strings.TrimPrefix(server.URL, "http") + "/agent/v1/acp?harness=pi", dir, cancel
}

func connect(t *testing.T, address string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, address, &websocket.DialOptions{Subprotocols: []string{"acp.v1"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	if conn.Subprotocol() != "acp.v1" {
		t.Fatal("client subprotocol not negotiated")
	}
	return conn
}

func send(t *testing.T, conn *websocket.Conn, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// ACP UI sends newline-terminated JSON in its WebSocket text frames.
	if err := conn.Write(ctx, websocket.MessageText, append(data, '\n')); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, conn *websocket.Conn) rpcMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var message rpcMessage
	if err := json.Unmarshal(data, &message); err != nil {
		t.Fatal(err)
	}
	if len(message.Error) != 0 {
		t.Fatalf("RPC error: %s", message.Error)
	}
	return message
}

func call(t *testing.T, conn *websocket.Conn, id int, method string, params any) (json.RawMessage, []json.RawMessage) {
	t.Helper()
	send(t, conn, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	return result(t, conn, id)
}

func result(t *testing.T, conn *websocket.Conn, id int) (json.RawMessage, []json.RawMessage) {
	t.Helper()
	var updates []json.RawMessage
	for {
		message := read(t, conn)
		if string(message.ID) == strconv.Itoa(id) {
			return message.Result, updates
		}
		if message.Method != "session/update" {
			t.Fatalf("unexpected message: %+v", message)
		}
		updates = append(updates, message.Params)
	}
}

func initialize(t *testing.T, conn *websocket.Conn) int {
	t.Helper()
	data, _ := call(t, conn, 1, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}})
	var initialized struct {
		Meta struct {
			PID int `json:"pid"`
		} `json:"_meta"`
	}
	if err := json.Unmarshal(data, &initialized); err != nil || initialized.Meta.PID == 0 {
		t.Fatalf("initialize failed: %s", data)
	}
	return initialized.Meta.PID
}

func newSession(t *testing.T, conn *websocket.Conn, id int, dir string) string {
	t.Helper()
	data, _ := call(t, conn, id, "session/new", map[string]any{"cwd": dir, "mcpServers": []any{}})
	var session struct {
		ID string `json:"sessionId"`
	}
	if err := json.Unmarshal(data, &session); err != nil || session.ID == "" {
		t.Fatalf("new session failed: %s", data)
	}
	return session.ID
}

func textPrompt(sessionID, text string) map[string]any {
	return map[string]any{"sessionId": sessionID, "prompt": []map[string]string{{"type": "text", "text": text}}}
}

func eventually(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestConnectionLifecycle(t *testing.T) {
	handler, address, dir, _ := fixture(t, 2)
	first := connect(t, address)
	firstPID := initialize(t, first)
	firstSession := newSession(t, first, 2, dir)
	if other := newSession(t, first, 3, dir); other == firstSession {
		t.Fatal("ACP sessions were restricted to one per connection")
	}
	_, updates := call(t, first, 4, "session/prompt", textPrompt(firstSession, "remember this"))
	if len(updates) != 1 || !bytes.Contains(updates[0], []byte("remember this")) {
		t.Fatal("missing output")
	}
	second := connect(t, address)
	secondPID := initialize(t, second)
	if firstPID == secondPID {
		t.Fatal("connections shared an agent process")
	}
	secondSession := newSession(t, second, 2, dir)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if conn, response, err := websocket.Dial(ctx, address, nil); err == nil {
		conn.CloseNow()
		t.Fatal("connection limit not enforced")
	} else if response == nil || response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %+v: %v", response, err)
	}
	// Disconnect an in-flight turn: the agent must terminate, not detach.
	send(t, first, map[string]any{"jsonrpc": "2.0", "id": 5, "method": "session/prompt", "params": textPrompt(firstSession, "block")})
	if message := read(t, first); message.Method != "session/update" {
		t.Fatal("blocking turn did not start")
	}
	first.Close(websocket.StatusNormalClosure, "")
	eventually(t, func() bool { return syscall.Kill(firstPID, 0) == syscall.ESRCH && len(handler.slots) == 1 })
	call(t, second, 3, "session/prompt", textPrompt(secondSession, "still alive"))
	reconnected := connect(t, address)
	if pid := initialize(t, reconnected); pid == firstPID || pid == secondPID {
		t.Fatal("reconnect did not start a fresh agent")
	}
	_, restored := call(t, reconnected, 2, "session/load", map[string]any{"sessionId": firstSession, "cwd": dir, "mcpServers": []any{}})
	if len(restored) != 1 || !bytes.Contains(restored[0], []byte("remember this")) {
		t.Fatal("exact native session history was not restored")
	}
	second.Close(websocket.StatusNormalClosure, "")
	reconnected.Close(websocket.StatusNormalClosure, "")
}

func TestACPContentAndPermissions(t *testing.T) {
	_, address, dir, _ := fixture(t, 1)
	conn := connect(t, address)
	initialize(t, conn)
	session := newSession(t, conn, 2, dir)
	content := json.RawMessage(`[{"type":"image","data":"AAEC","mimeType":"image/png","_meta":{"n":9007199254740993}},{"type":"resource_link","uri":"file:///source","name":"source"}]`)
	completion, updates := call(t, conn, 3, "session/prompt", map[string]any{"sessionId": session, "prompt": content})
	if len(updates) != 2 || !bytes.Contains(updates[0], []byte("9007199254740993")) || !bytes.Contains(completion, []byte("preserved")) {
		t.Fatal("opaque ACP content or completion metadata changed")
	}
	send(t, conn, map[string]any{"jsonrpc": "2.0", "id": 4, "method": "session/prompt", "params": textPrompt(session, "permission")})
	permission := read(t, conn)
	if permission.Method != "session/request_permission" || string(permission.ID) != `"approval"` {
		t.Fatalf("permission request changed: %+v", permission)
	}
	send(t, conn, map[string]any{"jsonrpc": "2.0", "id": permission.ID, "result": map[string]any{"outcome": map[string]string{"outcome": "selected", "optionId": "allow"}}})
	_, updates = result(t, conn, 4)
	if len(updates) != 1 || !bytes.Contains(updates[0], []byte("selected")) {
		t.Fatal("permission response did not reach agent")
	}
	conn.Close(websocket.StatusNormalClosure, "")
}

func TestShutdownClosesAgent(t *testing.T) {
	handler, address, _, stop := fixture(t, 1)
	conn := connect(t, address)
	pid := initialize(t, conn)
	stop()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, _, err := conn.Read(ctx); err == nil {
		t.Fatal("shutdown did not close connection")
	}
	eventually(t, func() bool { return syscall.Kill(pid, 0) == syscall.ESRCH && len(handler.slots) == 0 })
}

func TestExistingACPMirrorClient(t *testing.T) {
	_, address, dir, _ := fixture(t, 1)
	remote := os.Getenv("AGENT_PROXY_TEST_ACPREMOTE")
	client, err := filepath.Abs("testdata/client.py")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, filepath.Join(filepath.Dir(remote), "python"), client, remote, address, dir)
	output, err := command.CombinedOutput()
	if err != nil || !bytes.Contains(output, []byte(`"ok": true`)) {
		t.Fatalf("existing client failed: %v\n%s", err, output)
	}
	t.Log(strings.TrimSpace(string(output)))
}
