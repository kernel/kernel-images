package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	transport "github.com/kernel/kernel-images/server/experiments/agenttransport"
)

func TestContentPeer(t *testing.T) {
	path := os.Getenv("ACP_CONTENT_PEER")
	if path == "" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), 8<<20)
	encoder := json.NewEncoder(os.Stdout)
	send := func(value any) {
		if err := encoder.Encode(value); err != nil {
			os.Exit(1)
		}
	}
	for scanner.Scan() {
		var msg message
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			os.Exit(1)
		}
		var result any
		switch msg.Method {
		case "initialize":
			result = json.RawMessage(fmt.Sprintf(`{"protocolVersion":1,"agentCapabilities":{"promptCapabilities":%s}}`, os.Getenv("ACP_CONTENT_CAPS")))
		case "session/new":
			result = map[string]string{"sessionId": "native-session"}
		case "session/prompt":
			var params struct {
				SessionID string            `json:"sessionId"`
				Prompt    []json.RawMessage `json:"prompt"`
			}
			if json.Unmarshal(msg.Params, &params) != nil || params.SessionID != "native-session" {
				os.Exit(1)
			}
			if err := os.WriteFile(path, msg.Params, 0600); err != nil {
				os.Exit(1)
			}
			for _, block := range params.Prompt {
				send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": params.SessionID, "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": block}, "_meta": map[string]string{"fixture": "preserved"}}})
			}
			result = json.RawMessage(`{"stopReason":"end_turn","_meta":{"fixtureUsage":9007199254740993}}`)
		default:
			os.Exit(1)
		}
		send(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
	}
	os.Exit(0)
}

func contentClient(t *testing.T, caps string) (*Client, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "received.json")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := Start(ctx, Config{Command: os.Args[0], Args: []string{"-test.run=^TestContentPeer$"}, Cwd: filepath.Dir(path), Env: []string{"ACP_CONTENT_PEER=" + path, "ACP_CONTENT_CAPS=" + caps}}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	return client, path
}

func equalJSON(t *testing.T, got, want json.RawMessage) {
	t.Helper()
	decode := func(data []byte) any {
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	if !reflect.DeepEqual(decode(got), decode(want)) {
		t.Fatal("ACP JSON changed in transit")
	}
}

func TestContentThroughHTTPACPAndJournal(t *testing.T) {
	client, receivedPath := contentClient(t, `{"image":true,"audio":true,"embeddedContext":true}`)
	path := filepath.Join(t.TempDir(), "events.jsonl")
	store, err := transport.OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := transport.NewRuntime(client, store)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	server := httptest.NewServer(runtime)
	t.Cleanup(func() { runtime.Close(); server.Close() })
	prompt := json.RawMessage(fmt.Sprintf(`[
		{"type":"text","text":"inspect these inputs","annotations":{"audience":["assistant"]},"_meta":{"n":9007199254740993}},
		{"type":"image","data":"AAEC","mimeType":"image/png","uri":"file:///image.png"},
		{"type":"audio","data":"AAEC","mimeType":"audio/wav"},
		{"type":"resource","resource":{"uri":"file:///large.txt","text":"%s","mimeType":"text/plain"}},
		{"type":"resource","resource":{"uri":"file:///data.bin","blob":"AAEC","mimeType":"application/octet-stream"}},
		{"type":"resource_link","uri":"file:///source.go","name":"source.go","description":"source","size":123,"_meta":{"custom":true}}
	]`, strings.Repeat("x", 70<<10)))
	command := transport.Command{ID: "mixed-content", Prompt: prompt}
	body, err := json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	httpClient := &http.Client{Timeout: 10 * time.Second}
	response, err := httpClient.Post(server.URL+"/commands", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 202 {
		t.Fatalf("submit: %d", response.StatusCode)
	}
	response, err = httpClient.Get(server.URL + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 4096), 16<<20)
	var replay []transport.Event
	for scanner.Scan() {
		data, ok := strings.CutPrefix(scanner.Text(), "data: ")
		if !ok {
			continue
		}
		var event transport.Event
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			t.Fatal(err)
		}
		replay = append(replay, event)
		if event.Kind == "completed" || event.Kind == "failed" || event.Kind == "uncertain" {
			break
		}
	}
	response.Body.Close()
	if len(replay) != 9 || replay[len(replay)-1].Kind != "completed" {
		t.Fatalf("incomplete content replay: %d events, scanner=%v", len(replay), scanner.Err())
	}
	equalJSON(t, replay[0].Command.Prompt, prompt)
	data, err := os.ReadFile(receivedPath)
	if err != nil {
		t.Fatal(err)
	}
	var received struct {
		Prompt json.RawMessage `json:"prompt"`
	}
	if err := json.Unmarshal(data, &received); err != nil {
		t.Fatal(err)
	}
	equalJSON(t, received.Prompt, prompt)
	var blocks []json.RawMessage
	if err := json.Unmarshal(prompt, &blocks); err != nil {
		t.Fatal(err)
	}
	for i, block := range blocks {
		var update struct {
			Params struct {
				Update struct {
					Content json.RawMessage `json:"content"`
				} `json:"update"`
				Meta map[string]string `json:"_meta"`
			} `json:"params"`
		}
		if err := json.Unmarshal(replay[i+1].Payload, &update); err != nil {
			t.Fatal(err)
		}
		equalJSON(t, update.Params.Update.Content, block)
		if update.Params.Meta["fixture"] != "preserved" {
			t.Fatal("update metadata dropped")
		}
	}
	var completion message
	if err := json.Unmarshal(replay[7].Payload, &completion); err != nil {
		t.Fatal(err)
	}
	equalJSON(t, completion.Result, json.RawMessage(`{"stopReason":"end_turn","_meta":{"fixtureUsage":9007199254740993}}`))
	runtime.Close()
	client.Close()
	store, err = transport.OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := transport.NewRuntime(client, store)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	defer restored.Close()
	op, err := restored.Submit(command)
	if err != nil || op.State != "completed" || client.Dispatches() != 1 {
		t.Fatalf("completed content retry redispatched: %+v %v", op, err)
	}
	equalJSON(t, op.Command.Prompt, prompt)
	events, err := store.Load()
	if err != nil || !reflect.DeepEqual(events, replay) {
		t.Fatalf("journal recovery changed content: %v", err)
	}
}

func TestUnsupportedContentDoesNotDispatch(t *testing.T) {
	client, _ := contentClient(t, `{}`)
	runtime := transport.NewReference(client)
	defer runtime.Close()
	for _, kind := range []string{"image", "audio", "resource", "unknown"} {
		_, err := runtime.Submit(transport.Command{ID: kind, Prompt: json.RawMessage(fmt.Sprintf(`[{"type":%q}]`, kind))})
		if err == nil {
			t.Fatalf("unsupported %s accepted", kind)
		}
	}
	for _, prompt := range []string{`[{"type":"text","text":"ok"}]`, `[{"type":"resource_link","name":"source","uri":"file:///source"}]`} {
		if err := client.ValidatePrompt(json.RawMessage(prompt)); err != nil {
			t.Fatalf("baseline content rejected: %v", err)
		}
	}
	if client.Dispatches() != 0 {
		t.Fatal("unsupported content dispatched")
	}
}
