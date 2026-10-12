package webauthnhook

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kernel/kernel-images/server/lib/cdpmonitor"
	"github.com/stretchr/testify/require"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

type command struct {
	method string
	params map[string]any
}

// fakeSession records commands and answers the few whose results the hook reads.
type fakeSession struct {
	mu        sync.Mutex
	commands  []command
	replies   []map[string]any
	forbidden map[string]bool // permissions-policy features reported as not allowed
}

func (f *fakeSession) Send(_ context.Context, method string, params any) (json.RawMessage, error) {
	var decoded map[string]any
	if params != nil {
		data, _ := json.Marshal(params)
		_ = json.Unmarshal(data, &decoded)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, command{method, decoded})
	switch method {
	case "Page.addScriptToEvaluateOnNewDocument":
		return json.RawMessage(`{"identifier":"script-1"}`), nil
	case "Page.getPermissionsPolicyState":
		states := []map[string]any{}
		for _, feature := range []string{"publickey-credentials-get", "publickey-credentials-create"} {
			states = append(states, map[string]any{"feature": feature, "allowed": !f.forbidden[feature]})
		}
		return json.Marshal(map[string]any{"states": states})
	case "Runtime.evaluate":
		expression := decoded["expression"].(string)
		argument := strings.TrimSuffix(strings.TrimPrefix(expression, replyFunction+"("), ")")
		var reply map[string]any
		if err := json.Unmarshal([]byte(argument), &reply); err != nil {
			return nil, err
		}
		reply["context"] = decoded["uniqueContextId"]
		f.replies = append(f.replies, reply)
	}
	return json.RawMessage(`{}`), nil
}

func (f *fakeSession) methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	methods := make([]string, 0, len(f.commands))
	for _, c := range f.commands {
		methods = append(methods, c.method)
	}
	return methods
}

func (f *fakeSession) finalReply(t *testing.T, id string) map[string]any {
	t.Helper()
	var found map[string]any
	require.Eventually(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, reply := range f.replies {
			if reply["id"] == id && reply["ack"] == nil {
				found = reply
				return true
			}
		}
		return false
	}, 5*time.Second, 10*time.Millisecond)
	return found
}

func (f *fakeSession) countReplies(id string, ack bool) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, reply := range f.replies {
		if reply["id"] == id && (reply["ack"] == true) == ack {
			n++
		}
	}
	return n
}

type recordedRequest struct {
	op  string
	req Request
}

type fakeSigner struct {
	mu       sync.Mutex
	requests []recordedRequest
	answer   Answer
	release  chan struct{}
}

func (f *fakeSigner) Answer(_ context.Context, op string, req Request) Answer {
	f.mu.Lock()
	f.requests = append(f.requests, recordedRequest{op, req})
	release := f.release
	f.mu.Unlock()
	if release != nil {
		<-release
	}
	return f.answer
}

func (f *fakeSigner) calls() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedRequest(nil), f.requests...)
}

type harness struct {
	hook     *Hook
	signer   *fakeSigner
	path     string
	retained int
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessAt(t, filepath.Join(t.TempDir(), "webauthn-hook.json"))
}

func newHarnessAt(t *testing.T, path string) *harness {
	t.Helper()
	h := &harness{path: path, signer: &fakeSigner{answer: Answer{Result: "credential", Credential: json.RawMessage(`{"id":"cred"}`)}}}
	h.hook = New(Config{StatePath: path, Signer: h.signer, Logger: discard}, func() { h.retained++ })
	return h
}

func (h *harness) attach(t *testing.T, id, targetType, parentFrameID string) *fakeSession {
	t.Helper()
	fake := &fakeSession{forbidden: map[string]bool{}}
	h.hook.reconcile(h.hook.track(&session{id: id, targetID: id, targetType: targetType, parentFrameID: parentFrameID, cdp: fake}))
	return fake
}

func (h *harness) context(sessionID string, id int, origin, frameID string) {
	params, _ := json.Marshal(map[string]any{"context": map[string]any{
		"id": id, "uniqueId": sessionID + "-" + origin, "origin": origin,
		"auxData": map[string]any{"frameId": frameID, "isDefault": true},
	}})
	h.hook.ProtocolEvent(sessionID, "Runtime.executionContextCreated", params)
}

func (h *harness) call(sessionID string, contextID int, payload map[string]any) {
	data, _ := json.Marshal(payload)
	params, _ := json.Marshal(map[string]any{"name": bindingName, "payload": string(data), "executionContextId": contextID})
	h.hook.ProtocolEvent(sessionID, "Runtime.bindingCalled", params)
}

func getCall(id string) map[string]any {
	return map[string]any{"id": id, "op": "get", "publicKey": map[string]any{"challenge": "AAAA"}}
}

func TestSetInstallsAndRemovesHook(t *testing.T) {
	h := newHarness(t)
	page := h.attach(t, "page", "page", "")
	require.Empty(t, page.methods(), "an inactive hook must not touch sessions")
	h.hook.SessionAttached(cdpmonitor.Session{ID: "worker", TargetType: "worker"})
	require.NotContains(t, h.hook.sessions, "worker", "only page and iframe documents can call WebAuthn")

	require.NoError(t, h.hook.Set(context.Background(), true))
	require.Equal(t, 1, h.retained)
	require.Equal(t, []string{"Runtime.enable", "Page.enable", "Runtime.addBinding", "Page.addScriptToEvaluateOnNewDocument"}, page.methods())
	require.Equal(t, bindingName, page.commands[2].params["name"])
	require.Equal(t, true, page.commands[3].params["runImmediately"])
	require.Equal(t, hookSource, page.commands[3].params["source"])

	require.NoError(t, h.hook.Set(context.Background(), true))
	require.Len(t, page.methods(), 4, "enabling twice must not register the script twice")

	require.NoError(t, h.hook.Set(context.Background(), false))
	require.Equal(t, "Page.removeScriptToEvaluateOnNewDocument", page.methods()[4])
	require.Equal(t, "script-1", page.commands[4].params["identifier"])
	require.Len(t, page.methods(), 5, "disabling keeps the domains and binding for hooked documents")

	require.NoError(t, h.hook.Set(context.Background(), true))
	require.Equal(t, []string{"Page.addScriptToEvaluateOnNewDocument"}, page.methods()[5:])
	require.Equal(t, 1, h.retained)

	later := h.attach(t, "later", "iframe", "page")
	require.Equal(t, []string{"Runtime.enable", "Page.enable", "Runtime.addBinding", "Page.addScriptToEvaluateOnNewDocument"}, later.methods())
}

func TestEnabledFlagSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "webauthn-hook.json")
	first := newHarnessAt(t, path)
	require.NoError(t, first.hook.Set(context.Background(), true))

	restarted := newHarnessAt(t, path)
	require.Equal(t, 1, restarted.retained, "a restored hook retains page domains before the monitor starts")
	page := restarted.attach(t, "page", "page", "")
	require.Contains(t, page.methods(), "Page.addScriptToEvaluateOnNewDocument")

	require.NoError(t, restarted.hook.Set(context.Background(), false))
	again := newHarnessAt(t, path)
	require.Zero(t, again.retained)
	require.Empty(t, again.attach(t, "page", "page", "").methods())
}

func TestUnreadableStateLeavesHookOff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "webauthn-hook.json")
	require.NoError(t, os.WriteFile(path, []byte("{"), 0o644))
	h := newHarnessAt(t, path)
	require.Empty(t, h.attach(t, "page", "page", "").methods())
}

func TestCallForwardsChromiumOriginAndRepliesInContext(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, h.hook.Set(context.Background(), true))
	page := h.attach(t, "page", "page", "")
	h.context("page", 1, "https://example.com", "page")

	payload := getCall("r1")
	payload["mediation"] = "conditional"
	payload["origin"] = "https://evil.example"
	h.call("page", 1, payload)
	reply := page.finalReply(t, "r1")
	require.Equal(t, "credential", reply["result"])
	require.Equal(t, map[string]any{"id": "cred"}, reply["credential"])
	require.Equal(t, "page-https://example.com", reply["context"])
	require.Equal(t, 1, page.countReplies("r1", true))

	calls := h.signer.calls()
	require.Len(t, calls, 1)
	require.Equal(t, "get", calls[0].op)
	require.Equal(t, "https://example.com", calls[0].req.Origin)
	require.Equal(t, "https://example.com", calls[0].req.TopOrigin)
	require.Equal(t, "conditional", calls[0].req.Mediation)
	require.JSONEq(t, `{"challenge":"AAAA"}`, string(calls[0].req.PublicKey))

	// A resend after the answer re-delivers it without asking again.
	h.call("page", 1, getCall("r1"))
	require.Eventually(t, func() bool {
		page.mu.Lock()
		defer page.mu.Unlock()
		return len(page.replies) == 3
	}, 5*time.Second, 10*time.Millisecond)
	require.Len(t, h.signer.calls(), 1)
}

func TestResendWhilePendingIsAcknowledged(t *testing.T) {
	h := newHarness(t)
	h.signer.release = make(chan struct{})
	require.NoError(t, h.hook.Set(context.Background(), true))
	page := h.attach(t, "page", "page", "")
	h.context("page", 1, "https://example.com", "page")

	h.call("page", 1, getCall("r1"))
	require.Eventually(t, func() bool { return len(h.signer.calls()) == 1 }, 5*time.Second, 10*time.Millisecond)
	h.call("page", 1, getCall("r1"))
	require.Eventually(t, func() bool { return page.countReplies("r1", true) == 2 }, 5*time.Second, 10*time.Millisecond)
	close(h.signer.release)
	require.Equal(t, "credential", page.finalReply(t, "r1")["result"])
	require.Len(t, h.signer.calls(), 1)

	// The same request ID from another document is a different request.
	h.signer.release = nil
	h.context("page", 2, "https://other.example.com", "page")
	h.call("page", 2, getCall("r1"))
	require.Eventually(t, func() bool { return len(h.signer.calls()) == 2 }, 5*time.Second, 10*time.Millisecond)
}

func TestCrossOriginIframeGetsTopOrigin(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, h.hook.Set(context.Background(), true))
	h.attach(t, "page", "page", "")
	h.context("page", 1, "https://shop.example", "page")
	h.context("page", 2, "https://shop.example", "child-frame")
	h.attach(t, "outer", "iframe", "child-frame")
	h.context("outer", 1, "https://widgets.example", "outer")
	inner := h.attach(t, "inner", "iframe", "outer")
	h.context("inner", 1, "https://login.example.com", "inner")

	h.call("inner", 1, getCall("r1"))
	require.Equal(t, "credential", inner.finalReply(t, "r1")["result"])
	calls := h.signer.calls()
	require.Len(t, calls, 1)
	require.Equal(t, "https://login.example.com", calls[0].req.Origin)
	require.Equal(t, "https://shop.example", calls[0].req.TopOrigin)
	require.Empty(t, calls[0].req.Mediation)
}

func TestRequestsLeftToBrowser(t *testing.T) {
	for name, tc := range map[string]struct {
		origin    string
		forbidden string
		parent    string
	}{
		"opaque origin":             {origin: "://"},
		"null origin":               {origin: "null"},
		"insecure origin":           {origin: "http://example.com"},
		"permissions policy denies": {origin: "https://example.com", forbidden: "publickey-credentials-get"},
		"unknown top frame":         {origin: "https://example.com", parent: "missing"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			require.NoError(t, h.hook.Set(context.Background(), true))
			h.attach(t, "page", "page", "")
			h.context("page", 1, "https://example.com", "page")
			parent := "page"
			if tc.parent != "" {
				parent = tc.parent
			}
			frame := h.attach(t, "frame", "iframe", parent)
			frame.forbidden[tc.forbidden] = true
			h.context("frame", 1, tc.origin, "frame")
			h.call("frame", 1, getCall("r1"))
			require.Equal(t, "not_handled", frame.finalReply(t, "r1")["result"])
			require.Empty(t, h.signer.calls())
		})
	}
}

func TestLocalhostIsServed(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, h.hook.Set(context.Background(), true))
	page := h.attach(t, "page", "page", "")
	h.context("page", 1, "http://localhost:8080", "page")
	h.call("page", 1, getCall("r1"))
	require.Equal(t, "credential", page.finalReply(t, "r1")["result"])
}

func TestDisabledHookAnswersNotHandled(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, h.hook.Set(context.Background(), true))
	page := h.attach(t, "page", "page", "")
	h.context("page", 1, "https://example.com", "page")
	require.NoError(t, h.hook.Set(context.Background(), false))

	h.call("page", 1, getCall("r1"))
	require.Equal(t, "not_handled", page.finalReply(t, "r1")["result"])
	require.Empty(t, h.signer.calls())
}

func TestIgnoredCalls(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, h.hook.Set(context.Background(), true))
	page := h.attach(t, "page", "page", "")
	h.context("page", 1, "https://example.com", "page")

	h.call("page", 9, getCall("unknown-context"))
	h.call("page", 1, map[string]any{"id": "bad-op", "op": "store"})
	h.call("page", 1, map[string]any{"op": "get"})
	h.call("page", 1, map[string]any{"id": strings.Repeat("x", maxRequestIDLen+1), "op": "get"})
	other, _ := json.Marshal(map[string]any{"name": "__kernelEvent", "payload": `{"id":"x","op":"get"}`, "executionContextId": 1})
	h.hook.ProtocolEvent("page", "Runtime.bindingCalled", other)

	destroyed, _ := json.Marshal(map[string]any{"executionContextId": 1})
	h.hook.ProtocolEvent("page", "Runtime.executionContextDestroyed", destroyed)
	h.call("page", 1, getCall("after-destroy"))

	time.Sleep(50 * time.Millisecond)
	page.mu.Lock()
	defer page.mu.Unlock()
	require.Empty(t, page.replies)
	require.Empty(t, h.signer.calls())
}

func TestSessionCallsAreBounded(t *testing.T) {
	h := newHarness(t)
	h.signer.release = make(chan struct{})
	defer close(h.signer.release)
	require.NoError(t, h.hook.Set(context.Background(), true))
	page := h.attach(t, "page", "page", "")
	h.context("page", 1, "https://example.com", "page")
	for i := range maxSessionCalls {
		h.call("page", 1, getCall("r"+string(rune('a'+i))))
	}
	h.call("page", 1, getCall("overflow"))
	require.Equal(t, "not_handled", page.finalReply(t, "overflow")["result"])
}
