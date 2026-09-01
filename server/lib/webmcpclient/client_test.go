package webmcpclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

type staticUpstream struct {
	url string
}

func (u staticUpstream) Current() string { return u.url }

type fakeCDP struct {
	server *httptest.Server
	url    string

	mu              sync.Mutex
	connections     int
	relatedAttaches int
	enabledSessions map[string]int
	omitResponse    bool
}

func newFakeCDP(t *testing.T, omitResponse bool) *fakeCDP {
	t.Helper()
	fake := &fakeCDP{enabledSessions: make(map[string]int), omitResponse: omitResponse}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.serve))
	fake.url = "ws" + strings.TrimPrefix(fake.server.URL, "http")
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *fakeCDP) serve(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	f.mu.Lock()
	f.connections++
	f.mu.Unlock()

	var writeMu sync.Mutex
	write := func(value any) {
		payload, err := json.Marshal(value)
		if err != nil {
			return
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = conn.Write(r.Context(), websocket.MessageText, payload)
	}
	for {
		_, payload, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		var request cdpRequest
		if json.Unmarshal(payload, &request) != nil {
			continue
		}
		respond := func(result any) {
			write(map[string]any{"id": request.ID, "result": result})
		}
		switch request.Method {
		case "Target.getTargets":
			respond(map[string]any{"targetInfos": []map[string]any{{
				"targetId": "page-target", "type": "page", "url": "https://merchant.example/",
			}}})
		case "Target.autoAttachRelated":
			f.mu.Lock()
			f.relatedAttaches++
			f.mu.Unlock()
			write(map[string]any{
				"method": "Target.attachedToTarget",
				"params": map[string]any{
					"sessionId": "page-session",
					"targetInfo": map[string]any{
						"targetId": "page-target", "type": "page", "url": "https://merchant.example/",
					},
				},
			})
			write(map[string]any{
				"method": "Target.attachedToTarget",
				"params": map[string]any{
					"sessionId": "iframe-session",
					"targetInfo": map[string]any{
						"targetId": "iframe-target", "type": "iframe", "url": "https://payments.example/element#private-state",
						"parentFrameId": "page-frame",
					},
				},
			})
			respond(map[string]any{})
		case "Page.enable":
			respond(map[string]any{})
		case "Page.getFrameTree":
			frame := map[string]any{
				"id": "page-frame", "loaderId": "page-loader", "url": "https://merchant.example/",
			}
			if request.SessionID == "iframe-session" {
				frame = map[string]any{
					"id": "iframe-frame", "loaderId": "iframe-loader", "url": "https://payments.example/element#private-state",
					"parentId": "page-frame",
				}
			}
			respond(map[string]any{"frameTree": map[string]any{"frame": frame}})
		case "WebMCP.enable":
			f.mu.Lock()
			f.enabledSessions[request.SessionID]++
			f.mu.Unlock()
			respond(map[string]any{})
			name, frameID := "merchant_tool", "page-frame"
			if request.SessionID == "iframe-session" {
				name, frameID = "payment_tool", "iframe-frame"
			}
			write(map[string]any{
				"method":    "WebMCP.toolsAdded",
				"sessionId": request.SessionID,
				"params": map[string]any{"tools": []map[string]any{{
					"name": name, "description": name + " description", "frameId": frameID,
					"inputSchema": map[string]any{"type": "object"},
				}}},
			})
		case "WebMCP.invokeTool":
			respond(map[string]any{"invocationId": "invocation-1"})
			if !f.omitResponse {
				write(map[string]any{
					"method":    "Page.frameNavigated",
					"sessionId": request.SessionID,
					"params": map[string]any{"frame": map[string]any{
						"id": "iframe-frame", "loaderId": "next-loader", "url": "https://payments.example/success",
					}},
				})
				write(map[string]any{
					"method":    "WebMCP.toolResponded",
					"sessionId": request.SessionID,
					"params": map[string]any{
						"invocationId": "invocation-1", "status": "Completed",
						"output": map[string]any{"content": []map[string]any{{"type": "text", "text": "paid"}}},
					},
				})
			}
		default:
			write(map[string]any{"id": request.ID, "error": map[string]any{"code": -32601, "message": "unknown method"}})
		}
	}
}

func TestManagerDiscoversTopPageAndOOPIFToolsOverOneConnection(t *testing.T) {
	fake := newFakeCDP(t, false)
	manager := NewManager(staticUpstream{url: fake.url})
	t.Cleanup(func() { _ = manager.Close() })

	tools, err := manager.Tools(context.Background(), "")
	require.NoError(t, err)
	require.Len(t, tools, 2)

	byName := make(map[string]Tool, len(tools))
	for _, tool := range tools {
		byName[tool.Name] = tool
	}
	require.Equal(t, "page-target", byName["merchant_tool"].PageTargetID)
	require.Equal(t, "page-target", byName["merchant_tool"].TargetID)
	require.Equal(t, "page-target:page-loader", byName["merchant_tool"].DocumentRef)
	require.Equal(t, "page-target", byName["payment_tool"].PageTargetID)
	require.Equal(t, "iframe-target", byName["payment_tool"].TargetID)
	require.Equal(t, "iframe-frame", byName["payment_tool"].FrameID)
	require.Equal(t, "https://payments.example/element", byName["payment_tool"].TargetURL)
	require.Equal(t, "https://payments.example/element", byName["payment_tool"].FrameURL)
	require.Equal(t, "iframe-target:iframe-loader", byName["payment_tool"].DocumentRef)

	fake.mu.Lock()
	defer fake.mu.Unlock()
	require.Equal(t, 1, fake.connections)
	require.Equal(t, 1, fake.relatedAttaches)
	require.GreaterOrEqual(t, fake.enabledSessions["page-session"], 1)
	require.GreaterOrEqual(t, fake.enabledSessions["iframe-session"], 1)
}

func TestManagerReusesConnectionAndToolReferences(t *testing.T) {
	fake := newFakeCDP(t, false)
	manager := NewManager(staticUpstream{url: fake.url})
	t.Cleanup(func() { _ = manager.Close() })

	first, err := manager.Tools(context.Background(), "")
	require.NoError(t, err)
	second, err := manager.Tools(context.Background(), "")
	require.NoError(t, err)
	require.Equal(t, first, second)

	fake.mu.Lock()
	defer fake.mu.Unlock()
	require.Equal(t, 1, fake.connections)
	require.Equal(t, 1, fake.relatedAttaches)
}

func TestInvocationCanCompleteAfterFrameNavigation(t *testing.T) {
	fake := newFakeCDP(t, false)
	manager := NewManager(staticUpstream{url: fake.url})
	t.Cleanup(func() { _ = manager.Close() })
	tools, err := manager.Tools(context.Background(), "")
	require.NoError(t, err)
	var paymentTool Tool
	for _, tool := range tools {
		if tool.Name == "payment_tool" {
			paymentTool = tool
		}
	}
	require.NotEmpty(t, paymentTool.Ref)

	result, err := manager.Invoke(context.Background(), paymentTool.Ref, map[string]any{"amount": 2900})
	require.NoError(t, err)
	require.Equal(t, "invocation-1", result.InvocationID)
	require.Equal(t, "Completed", result.Status)
	require.Equal(t, "paid", result.Output.(map[string]any)["content"].([]any)[0].(map[string]any)["text"])

	_, err = manager.Invoke(context.Background(), paymentTool.Ref, map[string]any{})
	require.ErrorIs(t, err, ErrToolNotFound)
}

func TestInvocationTimeoutHasUnknownOutcome(t *testing.T) {
	fake := newFakeCDP(t, true)
	manager := NewManager(staticUpstream{url: fake.url})
	t.Cleanup(func() { _ = manager.Close() })
	tools, err := manager.Tools(context.Background(), "")
	require.NoError(t, err)
	var toolRef string
	for _, tool := range tools {
		if tool.Name == "payment_tool" {
			toolRef = tool.Ref
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	result, err := manager.Invoke(ctx, toolRef, map[string]any{})
	require.ErrorIs(t, err, ErrOutcomeUnknown)
	require.Equal(t, "invocation-1", result.InvocationID)
}
