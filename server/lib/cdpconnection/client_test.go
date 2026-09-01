package cdpconnection

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

func TestClientCorrelatesConcurrentCommandsAndPreservesEvents(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		for {
			_, payload, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			var request request
			if json.Unmarshal(payload, &request) != nil {
				continue
			}
			event, _ := json.Marshal(map[string]any{
				"method": "Test.event", "sessionId": request.SessionID,
				"params": map[string]any{"command": request.Method},
			})
			_ = conn.Write(r.Context(), websocket.MessageText, event)
			response, _ := json.Marshal(map[string]any{
				"id": request.ID, "result": map[string]any{"command": request.Method},
			})
			_ = conn.Write(r.Context(), websocket.MessageText, response)
		}
	}))
	defer server.Close()

	client, err := Dial(context.Background(), "ws"+strings.TrimPrefix(server.URL, "http"))
	require.NoError(t, err)
	defer client.Close()

	commands := []string{"Browser.getVersion", "Target.getTargets"}
	type commandResult struct {
		command string
		err     error
	}
	results := make(chan commandResult, len(commands))
	var wg sync.WaitGroup
	for _, command := range commands {
		wg.Add(1)
		go func() {
			defer wg.Done()
			raw, sendErr := client.Send(context.Background(), command, nil, "session-1")
			if sendErr != nil {
				results <- commandResult{err: sendErr}
				return
			}
			var result struct {
				Command string `json:"command"`
			}
			if unmarshalErr := json.Unmarshal(raw, &result); unmarshalErr != nil {
				results <- commandResult{err: unmarshalErr}
				return
			}
			results <- commandResult{command: result.Command}
		}()
	}
	wg.Wait()
	close(results)
	seenResults := make(map[string]bool)
	for result := range results {
		require.NoError(t, result.err)
		seenResults[result.command] = true
	}
	require.True(t, seenResults[commands[0]])
	require.True(t, seenResults[commands[1]])

	seenEvents := make(map[string]bool)
	for range commands {
		event := <-client.Events()
		require.Equal(t, "Test.event", event.Method)
		var params struct {
			Command string `json:"command"`
		}
		require.NoError(t, json.Unmarshal(event.Params, &params))
		seenEvents[params.Command] = true
	}
	require.True(t, seenEvents[commands[0]])
	require.True(t, seenEvents[commands[1]])
}
