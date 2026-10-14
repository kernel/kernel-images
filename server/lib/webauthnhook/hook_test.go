package webauthnhook

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/kernel/kernel-images/server/lib/browsersurface"
	"github.com/kernel/kernel-images/server/lib/cdpclient"
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
	suspended []string        // contexts told to suspend
	forbidden map[string]bool // permissions-policy features reported as not allowed
	failures  map[string]int  // method -> number of calls still to fail
	policy    chan struct{}   // when set, holds permissions-policy lookups until closed
}

func (f *fakeSession) Send(_ context.Context, method string, params any) (json.RawMessage, error) {
	var decoded map[string]any
	if params != nil {
		data, _ := json.Marshal(params)
		_ = json.Unmarshal(data, &decoded)
	}
	f.mu.Lock()
	policy := f.policy
	f.mu.Unlock()
	if method == "Page.getPermissionsPolicyState" && policy != nil {
		<-policy
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, command{method, decoded})
	if f.failures[method] > 0 {
		f.failures[method]--
		return nil, errors.New("injected failure")
	}
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
		if reply["suspend"] == true {
			f.suspended = append(f.suspended, decoded["uniqueContextId"].(string))
			break
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
	ctx context.Context
}

// fakeSigner answers with answer, holding each request until release is
// closed or the request is cancelled, as MetroSigner returns on cancellation.
type fakeSigner struct {
	mu          sync.Mutex
	requests    []recordedRequest
	answer      Answer
	release     chan struct{}
	inFlight    int
	maxInFlight int
}

func (f *fakeSigner) Configured() bool { return true }

func (f *fakeSigner) Answer(ctx context.Context, op string, req Request) Answer {
	f.mu.Lock()
	f.requests = append(f.requests, recordedRequest{op, req, ctx})
	release := f.release
	f.inFlight++
	f.maxInFlight = max(f.maxInFlight, f.inFlight)
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.inFlight--
		f.mu.Unlock()
	}()
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return notHandled
		}
	}
	return f.answer
}

func (f *fakeSigner) flight() (current, peak int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inFlight, f.maxInFlight
}

func (f *fakeSigner) calls() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedRequest(nil), f.requests...)
}

// switchableUpstream is a DevTools URL that tests can change, notifying
// subscribers the way the upstream manager does after a Chromium restart.
type switchableUpstream struct {
	mu   sync.Mutex
	url  string
	subs []chan string
}

func (u *switchableUpstream) Current() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.url
}

func (u *switchableUpstream) Subscribe() (<-chan string, func()) {
	u.mu.Lock()
	defer u.mu.Unlock()
	ch := make(chan string, 1)
	u.subs = append(u.subs, ch)
	return ch, func() {}
}

// set changes the URL; notify also tells subscribers, as a restart would.
func (u *switchableUpstream) set(url string, notify bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.url = url
	if !notify {
		return
	}
	for _, ch := range u.subs {
		select {
		case ch <- url:
		default:
		}
	}
}

type harness struct {
	hook   *Hook
	conn   *connection
	signer *fakeSigner
	path   string
	logs   *bytes.Buffer
	pages  sync.Map // session ID -> page session ID
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessAt(t, filepath.Join(t.TempDir(), "webauthn-hook.json"))
}

func newHarnessAt(t *testing.T, path string) *harness {
	t.Helper()
	h := &harness{
		path: path, logs: &bytes.Buffer{},
		signer: &fakeSigner{answer: Answer{Result: "credential", Credential: json.RawMessage(`{"id":"cred"}`)}},
	}
	logger := slog.New(slog.NewTextHandler(&lockedWriter{w: h.logs}, nil))
	h.hook = New(t.Context(), Config{StatePath: path, Signer: h.signer, Logger: logger}, &switchableUpstream{})
	t.Cleanup(h.hook.Stop)
	// Sessions attach to a connection that has no browser behind it. A page's
	// main frame ID is its session ID.
	h.conn = h.hook.newConnection(func(sessionID string) (string, string, bool) {
		page, ok := h.pages.Load(sessionID)
		if !ok {
			return "", "", false
		}
		return page.(string), page.(string), true
	})
	h.hook.mu.Lock()
	h.hook.conn = h.conn
	h.hook.mu.Unlock()
	return h
}

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// attach adds a session in the page whose session is page; a page passes "".
func (h *harness) attach(t *testing.T, id, targetType, page string) *fakeSession {
	t.Helper()
	return h.attachFake(id, targetType, page, &fakeSession{})
}

func (h *harness) attachFake(id, targetType, page string, fake *fakeSession) *fakeSession {
	if fake.forbidden == nil {
		fake.forbidden = map[string]bool{}
	}
	h.pages.Store(id, cmp.Or(page, id))
	h.conn.reconcile(h.conn.track(&session{id: id, targetType: targetType, cdp: fake}))
	return fake
}

func (h *harness) context(sessionID string, id int, origin, frameID string) {
	params, _ := json.Marshal(map[string]any{"context": map[string]any{
		"id": id, "uniqueId": sessionID + "-" + origin, "origin": origin,
		"auxData": map[string]any{"frameId": frameID, "isDefault": true},
	}})
	h.conn.protocolEvent(sessionID, "Runtime.executionContextCreated", params)
}

func (h *harness) call(sessionID string, contextID int, payload map[string]any) {
	data, _ := json.Marshal(payload)
	params, _ := json.Marshal(map[string]any{"name": bindingName, "payload": string(data), "executionContextId": contextID})
	h.conn.protocolEvent(sessionID, "Runtime.bindingCalled", params)
}

func getCall(id string) map[string]any {
	return map[string]any{"id": id, "op": "get", "publicKey": map[string]any{"challenge": "AAAA"}}
}

func TestSetInstallsAndSuspends(t *testing.T) {
	h := newHarness(t)
	page := h.attach(t, "page", "page", "")
	require.Empty(t, page.methods(), "a disabled hook must not touch sessions")
	h.conn.handleEvent(browsersurface.Event{
		Kind: browsersurface.EventSessionAttached, SessionID: "worker", Target: browsersurface.SessionTarget{Type: "worker"},
	})
	require.NotContains(t, h.conn.sessions, "worker", "only page and iframe documents can call WebAuthn")

	require.NoError(t, h.hook.Set(context.Background(), true))
	page = h.attach(t, "page", "page", "")
	require.Equal(t, []string{"Runtime.enable", "Page.enable", "Runtime.addBinding", "Page.addScriptToEvaluateOnNewDocument"}, page.methods())
	require.Equal(t, bindingName, page.commands[2].params["name"])
	require.Equal(t, true, page.commands[3].params["runImmediately"])
	require.Equal(t, hookSource, page.commands[3].params["source"])

	h.context("page", 1, "https://example.com", "page")
	h.context("page", 2, "https://example.com", "child")
	require.NoError(t, h.hook.Set(context.Background(), false))
	require.ElementsMatch(t, []string{"page-https://example.com", "page-https://example.com"}, page.suspended)
	require.Equal(t, []string{"Runtime.evaluate", "Runtime.evaluate"}, page.methods()[4:], "the closed connection removes the binding and script")
}

func TestSetStartsAndStopsConnectionLoop(t *testing.T) {
	h := newHarness(t)
	require.Nil(t, h.hook.stopRun)
	require.NoError(t, h.hook.Set(context.Background(), true))
	require.NotNil(t, h.hook.stopRun)
	require.NoError(t, h.hook.Set(context.Background(), true))
	require.NoError(t, h.hook.Set(context.Background(), false))
	require.Nil(t, h.hook.stopRun)
}

func TestSetupFailureIsRetriedOnce(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, h.hook.Set(context.Background(), true))
	page := h.attachFake("page", "page", "", &fakeSession{failures: map[string]int{"Runtime.addBinding": 1}})
	require.Equal(t, []string{
		"Runtime.enable", "Page.enable", "Runtime.addBinding",
		"Runtime.enable", "Page.enable", "Runtime.addBinding", "Page.addScriptToEvaluateOnNewDocument",
	}, page.methods())
	require.NotContains(t, h.logs.String(), "level=ERROR")

	h.attachFake("broken", "page", "", &fakeSession{failures: map[string]int{"Runtime.enable": 2}})
	require.Contains(t, h.logs.String(), "level=ERROR")
	require.Contains(t, h.logs.String(), "session=broken")
}

func TestEnabledFlagSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "webauthn-hook.json")
	first := newHarnessAt(t, path)
	require.NoError(t, first.hook.Set(context.Background(), true))

	restarted := newHarnessAt(t, path)
	require.NotNil(t, restarted.hook.stopRun, "a restored hook connects at start")
	page := restarted.attach(t, "page", "page", "")
	require.Contains(t, page.methods(), "Page.addScriptToEvaluateOnNewDocument")

	require.NoError(t, restarted.hook.Set(context.Background(), false))
	again := newHarnessAt(t, path)
	require.Nil(t, again.hook.stopRun)
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
	// The tracker resolves nested out-of-process frames to their page.
	h.attach(t, "outer", "iframe", "page")
	h.context("outer", 1, "https://widgets.example", "outer")
	inner := h.attach(t, "inner", "iframe", "page")
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
	for _, origin := range []string{"http://localhost:8080", "http://app.localhost:3000"} {
		h := newHarness(t)
		require.NoError(t, h.hook.Set(context.Background(), true))
		page := h.attach(t, "page", "page", "")
		h.context("page", 1, origin, "page")
		h.call("page", 1, getCall("r1"))
		require.Equal(t, "credential", page.finalReply(t, "r1")["result"], origin)
	}
}

// An iframe can call at load before the tracker has placed it in its page.
// The request waits for that rather than settling as not_handled.
func TestTopOriginWaitsForTheFrameToBePlaced(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, h.hook.Set(context.Background(), true))
	h.attach(t, "page", "page", "")
	h.context("page", 1, "https://shop.example", "page")
	frame := h.attach(t, "frame", "iframe", "page")
	h.pages.Delete("frame")
	h.context("frame", 1, "https://login.example.com", "frame")

	h.call("frame", 1, getCall("r1"))
	time.Sleep(topOriginWait / 4)
	h.pages.Store("frame", "page")
	require.Equal(t, "credential", frame.finalReply(t, "r1")["result"])
	require.Equal(t, "https://shop.example", h.signer.calls()[0].req.TopOrigin)
}

func TestTopOriginFollowsNewestMainFrameContext(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, h.hook.Set(context.Background(), true))
	h.attach(t, "page", "page", "")
	frame := h.attach(t, "frame", "iframe", "page")
	h.context("frame", 1, "https://login.example.com", "frame")
	// The navigated-to document's context arrives before the old one is destroyed.
	for i := 1; i <= 20; i++ {
		h.context("page", i, fmt.Sprintf("https://site%d.example", i), "page")
	}
	destroyed, _ := json.Marshal(map[string]any{"executionContextId": 3})
	h.conn.protocolEvent("page", "Runtime.executionContextDestroyed", destroyed)

	h.call("frame", 1, getCall("r1"))
	require.Equal(t, "credential", frame.finalReply(t, "r1")["result"])
	require.Equal(t, "https://site20.example", h.signer.calls()[0].req.TopOrigin)

	destroyed, _ = json.Marshal(map[string]any{"executionContextId": 20})
	h.conn.protocolEvent("page", "Runtime.executionContextDestroyed", destroyed)
	h.call("frame", 1, getCall("r2"))
	require.Equal(t, "not_handled", frame.finalReply(t, "r2")["result"], "a destroyed main-frame document has no top origin")
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
	h.conn.protocolEvent("page", "Runtime.bindingCalled", other)

	destroyed, _ := json.Marshal(map[string]any{"executionContextId": 1})
	h.conn.protocolEvent("page", "Runtime.executionContextDestroyed", destroyed)
	h.call("page", 1, getCall("after-destroy"))

	time.Sleep(50 * time.Millisecond)
	page.mu.Lock()
	defer page.mu.Unlock()
	require.Empty(t, page.replies)
	require.Empty(t, h.signer.calls())
}

func TestOnlyUnansweredCallsAreBounded(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, h.hook.Set(context.Background(), true))
	page := h.attach(t, "page", "page", "")
	h.context("page", 1, "https://example.com", "page")
	for i := range 2 * maxPendingCalls {
		id := fmt.Sprintf("answered-%d", i)
		h.call("page", 1, getCall(id))
		require.Equal(t, "credential", page.finalReply(t, id)["result"])
	}

	h.signer.release = make(chan struct{})
	defer close(h.signer.release)
	for i := range maxPendingCalls {
		h.call("page", 1, getCall(fmt.Sprintf("pending-%d", i)))
	}
	require.Eventually(t, func() bool { return len(h.signer.calls()) == 3*maxPendingCalls }, 5*time.Second, 10*time.Millisecond)
	require.NotContains(t, h.logs.String(), "too many unanswered requests")
	h.call("page", 1, getCall("overflow"))
	require.Equal(t, "not_handled", page.finalReply(t, "overflow")["result"])
	require.Contains(t, h.logs.String(), "level=WARN")
	require.Contains(t, h.logs.String(), "too many unanswered requests")
}

func TestAnsweredCallsAreEvictedOldestFirst(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, h.hook.Set(context.Background(), true))
	page := h.attach(t, "page", "page", "")
	h.context("page", 1, "https://example.com", "page")
	for i := range maxTrackedCalls + 1 {
		id := fmt.Sprintf("r%d", i)
		h.call("page", 1, getCall(id))
		page.finalReply(t, id)
	}
	h.conn.mu.Lock()
	defer h.conn.mu.Unlock()
	calls := h.conn.sessions["page"].calls
	require.Len(t, calls, maxTrackedCalls)
	require.NotContains(t, calls, callKey{contextID: "page-https://example.com", requestID: "r0"})
}

func TestAbortBeforeDispatchSkipsSigner(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, h.hook.Set(context.Background(), true))
	page := h.attachFake("page", "page", "", &fakeSession{policy: make(chan struct{})})
	h.context("page", 1, "https://example.com", "page")

	h.call("page", 1, getCall("r1"))
	require.Eventually(t, func() bool { return page.countReplies("r1", true) == 1 }, 5*time.Second, 10*time.Millisecond)
	h.call("page", 1, map[string]any{"id": "r1", "op": "abort"})
	close(page.policy)

	time.Sleep(100 * time.Millisecond)
	require.Empty(t, h.signer.calls())
	require.Zero(t, page.countReplies("r1", false), "an aborted request gets no answer")
}

func TestAbortAfterDispatchCancelsSigner(t *testing.T) {
	h := newHarness(t)
	h.signer.release = make(chan struct{})
	require.NoError(t, h.hook.Set(context.Background(), true))
	page := h.attach(t, "page", "page", "")
	h.context("page", 1, "https://example.com", "page")

	h.call("page", 1, getCall("r1"))
	require.Eventually(t, func() bool { return len(h.signer.calls()) == 1 }, 5*time.Second, 10*time.Millisecond)
	h.call("page", 1, map[string]any{"id": "r1", "op": "abort"})
	require.Error(t, h.signer.calls()[0].ctx.Err())
	close(h.signer.release)

	time.Sleep(100 * time.Millisecond)
	require.Zero(t, page.countReplies("r1", false))
}

func TestDroppedDocumentsCancelTheSigner(t *testing.T) {
	for name, drop := range map[string]func(h *harness){
		"context destroyed": func(h *harness) {
			params, _ := json.Marshal(map[string]any{"executionContextId": 1})
			h.conn.protocolEvent("page", "Runtime.executionContextDestroyed", params)
		},
		"contexts cleared": func(h *harness) {
			h.conn.protocolEvent("page", "Runtime.executionContextsCleared", nil)
		},
		"session removed": func(h *harness) {
			h.conn.handleEvent(browsersurface.Event{Kind: browsersurface.EventSessionRemoved, SessionID: "page"})
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.signer.release = make(chan struct{})
			defer close(h.signer.release)
			require.NoError(t, h.hook.Set(context.Background(), true))
			page := h.attach(t, "page", "page", "")
			h.context("page", 1, "https://example.com", "page")

			h.call("page", 1, getCall("r1"))
			require.Eventually(t, func() bool { return len(h.signer.calls()) == 1 }, 5*time.Second, 10*time.Millisecond)
			drop(h)
			require.Error(t, h.signer.calls()[0].ctx.Err(), "the signer must stop asking metro-api")
			require.Eventually(t, func() bool { current, _ := h.signer.flight(); return current == 0 }, 5*time.Second, 10*time.Millisecond)
			require.Zero(t, page.countReplies("r1", false))
		})
	}
}

// Frames that call once and go away must not let a page past the cap: each
// dropped call is cancelled rather than left running uncounted.
func TestFrameChurnStaysUnderTheCap(t *testing.T) {
	h := newHarness(t)
	h.signer.release = make(chan struct{})
	defer close(h.signer.release)
	require.NoError(t, h.hook.Set(context.Background(), true))
	h.attach(t, "page", "page", "")
	h.context("page", 1, "https://example.com", "page")
	for i := range 3 * maxPendingCalls {
		contextID := 100 + i
		h.context("page", contextID, "https://example.com", fmt.Sprintf("frame-%d", i))
		h.call("page", contextID, getCall("r"))
		require.Eventually(t, func() bool { return len(h.signer.calls()) == i+1 }, 5*time.Second, 10*time.Millisecond)
		destroyed, _ := json.Marshal(map[string]any{"executionContextId": contextID})
		h.conn.protocolEvent("page", "Runtime.executionContextDestroyed", destroyed)
	}
	require.Eventually(t, func() bool { current, _ := h.signer.flight(); return current == 0 }, 5*time.Second, 10*time.Millisecond)
	_, peak := h.signer.flight()
	require.LessOrEqual(t, peak, maxPendingCalls)
}

// A target that failed to attach stays unhooked on this connection, so the
// connection is closed and the next one attaches it.
func TestFailedAttachClosesConnection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		_, _, _ = conn.Read(r.Context())
	}))
	defer server.Close()
	protocol, err := cdpclient.DialWithEvents(context.Background(), "ws"+strings.TrimPrefix(server.URL, "http"))
	require.NoError(t, err)
	defer protocol.Close()

	h := newHarness(t)
	h.conn.protocol = protocol
	h.conn.handleEvent(browsersurface.Event{Kind: browsersurface.EventDiscoveryFailed})
	require.True(t, protocol.IsClosed())
	require.Contains(t, h.logs.String(), "failed to attach a browser target")
}
