// Package fillfence serializes vault fills at the process that owns Chromium.
// A Fence belongs to exactly one Chrome generation. It must never be reused for
// a replacement browser or constructed against an inherited, unfenced browser.
package fillfence

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const Protocol = "kernel.vault-fill.v1"
const Address = "127.0.0.1:9226"

type message struct {
	ID        int64           `json:"id"`
	Method    string          `json:"method,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
}

// Fence is fail-closed: only the explicit end message following acknowledged
// commands releases admission. Disconnect, timeout and protocol failure leave
// this generation quarantined, even if an outstanding command later completes.
// There is no timer, reconnect, or administrative unlock that clears quarantine.
type Fence struct {
	upstream string
	identity func() string
	bornAs   string
	mu       sync.Mutex
	occupied bool
}

func New(upstream string, identity func() string) *Fence {
	return &Fence{upstream: upstream, identity: identity, bornAs: identity()}
}

func (f *Fence) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	if f.occupied {
		f.mu.Unlock()
		http.Error(w, "fill unavailable", http.StatusConflict)
		return
	}
	f.occupied = true
	f.mu.Unlock()
	claimed := false
	defer func() {
		if !claimed {
			f.mu.Lock()
			f.occupied = false
			f.mu.Unlock()
		}
	}()
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	client, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
	if err != nil {
		return
	}
	defer client.CloseNow()
	client.SetReadLimit(8 << 20)
	handshake, stop := context.WithTimeout(ctx, 5*time.Second)
	_, raw, err := client.Read(handshake)
	stop()
	var begin message
	var params struct{ Protocol, Nonce, Instance string }
	if err != nil || json.Unmarshal(raw, &begin) != nil || begin.ID <= 0 || begin.Method != "Kernel.vaultFill.begin" || begin.SessionID != "" || json.Unmarshal(begin.Params, &params) != nil || params.Protocol != Protocol || len(params.Nonce) != 32 {
		return
	}
	if f.bornAs == "" || f.identity() != f.bornAs || params.Instance != f.bornAs {
		return
	}
	claimed = true

	// Never redial this connection, including on browser or proxy restarts.
	upstream, _, err := websocket.Dial(ctx, f.upstream, nil)
	if err != nil {
		return
	}
	defer upstream.CloseNow()
	upstream.SetReadLimit(8 << 20)
	if !reply(ctx, client, begin.ID, map[string]string{"protocol": Protocol, "nonce": params.Nonce}) {
		return
	}
	lastID := begin.ID
	for {
		_, raw, err := client.Read(ctx)
		var command message
		if err != nil || json.Unmarshal(raw, &command) != nil || command.ID <= lastID || f.identity() != f.bornAs {
			return
		}
		lastID = command.ID
		if command.Method == "Kernel.vaultFill.end" && command.SessionID == "" {
			// No command is outstanding here. Close the only command channel before
			// releasing; queued data on the old client can never reach Chrome again.
			upstream.CloseNow()
			f.mu.Lock()
			f.occupied = false
			f.mu.Unlock()
			reply(ctx, client, command.ID, struct{}{})
			return
		}
		if !allowed(command.Method) {
			return
		}
		if upstream.Write(ctx, websocket.MessageText, raw) != nil {
			return
		}
		for {
			_, data, err := upstream.Read(ctx)
			var response message
			if err != nil || json.Unmarshal(data, &response) != nil {
				return
			}
			if response.ID != 0 && (response.ID != command.ID || response.SessionID != command.SessionID) {
				return
			}
			if client.Write(ctx, websocket.MessageText, data) != nil {
				return
			}
			if response.ID == command.ID {
				break
			}
		}
	}
}

func reply(ctx context.Context, conn *websocket.Conn, id int64, result any) bool {
	data, err := json.Marshal(struct {
		ID     int64 `json:"id"`
		Result any   `json:"result"`
	}{id, result})
	return err == nil && conn.Write(ctx, websocket.MessageText, data) == nil
}

// This path cannot create a second command channel, reset Chrome, or enqueue
// detached protocol work. Runtime calls are synchronous (the API supplies the
// guarded scripts); only their response confirms completion of that command.
func allowed(method string) bool {
	switch method {
	case "Target.getTargets", "Target.attachToTarget", "Page.enable", "Page.getFrameTree", "Page.createIsolatedWorld", "DOM.describeNode", "DOM.resolveNode", "Runtime.callFunctionOn", "Runtime.releaseObject", "Runtime.evaluate", "Runtime.getProperties":
		return true
	}
	return false
}
