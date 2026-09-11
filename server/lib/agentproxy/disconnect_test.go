package agentproxy

import (
	"syscall"
	"testing"
)

func TestAbruptDisconnect(t *testing.T) {
	for _, prompt := range []string{"block", "permission"} {
		t.Run(prompt, func(t *testing.T) {
			handler, address, dir, _ := fixture(t, 1)
			conn := connect(t, address)
			pid := initialize(t, conn)
			session := newSession(t, conn, 2, dir)
			send(t, conn, map[string]any{"jsonrpc": "2.0", "id": 3, "method": "session/prompt", "params": textPrompt(session, prompt)})
			message := read(t, conn)
			expected := "session/update"
			if prompt == "permission" {
				expected = "session/request_permission"
			}
			if message.Method != expected {
				t.Fatalf("agent did not reach %s", prompt)
			}
			conn.CloseNow() // No WebSocket close handshake or ACP cancellation.
			eventually(t, func() bool { return syscall.Kill(pid, 0) == syscall.ESRCH && len(handler.slots) == 0 })
		})
	}
}
