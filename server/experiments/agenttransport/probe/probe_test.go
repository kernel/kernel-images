package probe

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"
)

func TestMCPToolBarrier(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	input, writer := io.Pipe()
	output, reader := io.Pipe()
	t.Cleanup(func() { cancel(); writer.Close(); input.Close(); reader.Close(); output.Close() })
	go func() { <-ctx.Done(); input.Close(); output.Close() }()
	p := Probe{Dir: t.TempDir()}
	done := make(chan error, 1)
	go func() { done <- ServeMCP(ctx, input, reader, p) }()
	encoder := json.NewEncoder(writer)
	decoder := json.NewDecoder(bufio.NewReader(output))
	send := func(id int, method string, params any) {
		t.Helper()
		if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
			t.Fatal(err)
		}
	}
	receive := func() map[string]json.RawMessage {
		t.Helper()
		var result map[string]json.RawMessage
		if err := decoder.Decode(&result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	send(1, "initialize", map[string]string{"protocolVersion": "2025-03-26"})
	var initialized struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(receive()["result"], &initialized); err != nil || initialized.ProtocolVersion != "2025-03-26" {
		t.Fatal("MCP version negotiation failed")
	}
	send(2, "tools/list", map[string]any{})
	var listed struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(receive()["result"], &listed); err != nil || len(listed.Tools) != 1 || listed.Tools[0].Name != "checkpoint" {
		t.Fatal("missing checkpoint tool")
	}
	send(3, "tools/call", map[string]any{"name": "checkpoint", "arguments": map[string]any{}})
	if err := p.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	count, err := p.Count()
	if err != nil || count != 1 {
		t.Fatalf("tool count %d: %v", count, err)
	}
	if err := p.Release(); err != nil {
		t.Fatal(err)
	}
	if len(receive()["error"]) != 0 {
		t.Fatal("tool invocation failed")
	}
	writer.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
