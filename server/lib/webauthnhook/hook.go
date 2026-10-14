// Package webauthnhook answers pages' navigator.credentials.get/create
// publicKey requests with passkeys held outside the VM.
//
// While the hook is on it keeps its own DevTools connection to Chromium,
// separate from telemetry's. Over it, a script installed in every page and
// iframe document replaces the WebAuthn entry points and sends each request
// through a CDP binding. The hook adds the calling document's origin as
// Chromium reports it, forwards the request to a Signer (metro-api), and
// replies in the same execution context. Requests nothing answers fall back
// to the browser's own implementation. Turning the hook off closes the
// connection, which removes its bindings and scripts.
package webauthnhook

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/kernel/kernel-images/server/lib/cdpclient"
)

// bindingName must match BINDING and replyFunction must match REPLY in hook.js.
const (
	bindingName   = "__kernelPasskey"
	replyFunction = "__kernelPasskeyReply"
)

const (
	setupTimeout     = 10 * time.Second
	setupRetryDelay  = time.Second
	deliverTimeout   = 5 * time.Second
	maxCallBytes     = 256 << 10
	maxRequestIDLen  = 64
	maxPendingCalls  = 16  // unanswered calls per session
	maxTrackedCalls  = 256 // answered calls kept per session for resends
	answeredRetained = 30 * time.Second
	maxFrameDepth    = 32
)

// ErrNotConfigured means the hook cannot be turned on because the VM has no
// metro API to answer it.
var ErrNotConfigured = errors.New("WebAuthn hook is not configured: KERNEL_METRO_API_BASE_URL is not set")

// suspendExpression tells a hooked document to leave every request to the
// browser until the hook script runs in it again.
const suspendExpression = "typeof " + replyFunction + ` === "function" && ` + replyFunction + `({"suspend":true})`

//go:embed hook.js
var hookSource string

type Config struct {
	// StatePath keeps the enabled flag across restarts of this process.
	StatePath string
	Signer    Signer
	Logger    *slog.Logger
}

// Upstream reports Chromium's DevTools URL and its changes.
// *devtoolsproxy.UpstreamManager implements it.
type Upstream interface {
	Current() string
	Subscribe() (<-chan string, func())
}

type executionContext struct {
	uniqueID string
	origin   string
	frameID  string
}

type callKey struct {
	contextID string // unique execution context ID
	requestID string
}

// call is one page request. reply is empty until the request is answered;
// the page resends until it sees that reply. cancel ends the request when the
// page aborts it.
type call struct {
	reply      json.RawMessage
	answeredAt time.Time
	cancel     context.CancelFunc
}

type sender interface {
	Send(ctx context.Context, method string, params any) (json.RawMessage, error)
}

// cdpSession sends commands to one target over the hook's connection.
type cdpSession struct {
	protocol *cdpclient.Client
	id       string
}

func (c cdpSession) Send(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return c.protocol.Send(ctx, method, params, c.id)
}

type session struct {
	id            string
	targetID      string
	targetType    string
	parentFrameID string
	cdp           sender

	attempted     bool                     // Hook.mu; first setup attempt has finished
	contexts      map[int]executionContext // Hook.mu; main-world contexts only
	frameContexts map[string]int           // Hook.mu; frame ID -> its newest main-world context
	calls         map[callKey]*call        // Hook.mu
}

type Hook struct {
	cfg      Config
	upstream Upstream
	log      *slog.Logger

	setMu     sync.Mutex         // serializes Start, Stop, and Set
	lifecycle context.Context    // setMu; set by Start
	stopRun   context.CancelFunc // setMu; non-nil while the connection loop runs
	runDone   chan struct{}      // setMu

	mu       sync.Mutex
	enabled  bool
	conn     *connection // started connection, nil while there is none
	sessions map[string]*session
}

// New restores the flag a previous run of the process left. Nothing connects
// until Start.
func New(cfg Config, upstream Upstream) *Hook {
	return &Hook{
		cfg: cfg, upstream: upstream, log: cfg.Logger,
		enabled:  loadEnabled(cfg.StatePath, cfg.Logger),
		sessions: make(map[string]*session),
	}
}

// Start connects to Chromium if the hook is on. Connections live until ctx
// ends, Stop, or the hook is turned off.
func (h *Hook) Start(ctx context.Context) {
	h.setMu.Lock()
	defer h.setMu.Unlock()
	h.lifecycle = ctx
	h.mu.Lock()
	enabled := h.enabled
	h.mu.Unlock()
	if enabled {
		h.startLocked()
	}
}

// Stop closes the connection without changing the flag.
func (h *Hook) Stop() {
	h.setMu.Lock()
	defer h.setMu.Unlock()
	h.stopLocked()
}

// Set turns the hook on or off, persisting the flag first. Turning it on
// fails with ErrNotConfigured when the signer is not configured, and otherwise
// connects and waits, within ctx and installWait, until the documents already
// open are hooked. Turning it off tells hooked documents to leave requests to
// the browser, then closes the connection.
func (h *Hook) Set(ctx context.Context, enabled bool) error {
	h.setMu.Lock()
	defer h.setMu.Unlock()
	if enabled && !h.cfg.Signer.Configured() {
		return ErrNotConfigured
	}
	if err := saveEnabled(h.cfg.StatePath, enabled); err != nil {
		return err
	}
	h.mu.Lock()
	h.enabled = enabled
	h.mu.Unlock()
	if enabled {
		h.startLocked()
		h.waitInstalled(ctx)
		return nil
	}
	h.suspendAll()
	h.stopLocked()
	return nil
}

func (h *Hook) startLocked() {
	if h.lifecycle == nil || h.stopRun != nil {
		return
	}
	ctx, cancel := context.WithCancel(h.lifecycle)
	done := make(chan struct{})
	h.stopRun, h.runDone = cancel, done
	go func() {
		defer close(done)
		h.run(ctx)
	}()
}

func (h *Hook) stopLocked() {
	if h.stopRun == nil {
		return
	}
	h.stopRun()
	<-h.runDone
	h.stopRun, h.runDone = nil, nil
}

// suspendAll makes every hooked document fall back at once, rather than after
// the page's timeout once the connection is gone.
func (h *Hook) suspendAll() {
	type target struct {
		s  *session
		ec executionContext
	}
	var targets []target
	h.mu.Lock()
	for _, s := range h.sessions {
		for _, ec := range s.contexts {
			targets = append(targets, target{s, ec})
		}
	}
	h.mu.Unlock()
	var wg sync.WaitGroup
	for _, t := range targets {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), deliverTimeout)
			defer cancel()
			_, _ = t.s.cdp.Send(ctx, "Runtime.evaluate", map[string]any{
				"expression": suspendExpression, "uniqueContextId": t.ec.uniqueID, "silent": true,
			})
		})
	}
	wg.Wait()
}

func (h *Hook) track(s *session) *session {
	s.contexts = make(map[int]executionContext)
	s.frameContexts = make(map[string]int)
	s.calls = make(map[callKey]*call)
	h.mu.Lock()
	h.sessions[s.id] = s
	h.mu.Unlock()
	return s
}

// reconcile hooks a newly attached session, retrying once after a short
// delay so a tab is not left unhooked by a transient failure.
func (h *Hook) reconcile(s *session) {
	err := h.install(s)
	if err != nil {
		time.Sleep(setupRetryDelay)
		if err = h.install(s); err != nil && h.live(s) {
			h.log.Error("webauthn: failed to install hook", "session", s.id, "target_type", s.targetType, "err", err)
		}
	}
	h.mu.Lock()
	s.attempted = true
	h.mu.Unlock()
}

func (h *Hook) live(s *session) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sessions[s.id] == s
}

func (h *Hook) install(s *session) error {
	h.mu.Lock()
	enabled, live := h.enabled, h.sessions[s.id] == s
	h.mu.Unlock()
	if !enabled || !live {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	defer cancel()
	for _, command := range []struct {
		method string
		params any
	}{
		{"Runtime.enable", nil},
		{"Page.enable", nil},
		{"Runtime.addBinding", map[string]any{"name": bindingName}},
		{"Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": hookSource, "runImmediately": true}},
	} {
		if _, err := s.cdp.Send(ctx, command.method, command.params); err != nil {
			return err
		}
	}
	return nil
}

func (h *Hook) protocolEvent(sessionID, method string, params json.RawMessage) {
	switch method {
	case "Runtime.executionContextCreated":
		var event struct {
			Context struct {
				ID       int    `json:"id"`
				UniqueID string `json:"uniqueId"`
				Origin   string `json:"origin"`
				AuxData  struct {
					FrameID   string `json:"frameId"`
					IsDefault bool   `json:"isDefault"`
				} `json:"auxData"`
			} `json:"context"`
		}
		if json.Unmarshal(params, &event) != nil || !event.Context.AuxData.IsDefault {
			return
		}
		h.mu.Lock()
		if s := h.sessions[sessionID]; s != nil {
			s.contexts[event.Context.ID] = executionContext{
				uniqueID: event.Context.UniqueID, origin: event.Context.Origin, frameID: event.Context.AuxData.FrameID,
			}
			s.frameContexts[event.Context.AuxData.FrameID] = event.Context.ID
		}
		h.mu.Unlock()
	case "Runtime.executionContextDestroyed":
		var event struct {
			ID int `json:"executionContextId"`
		}
		if json.Unmarshal(params, &event) != nil {
			return
		}
		h.mu.Lock()
		if s := h.sessions[sessionID]; s != nil {
			if destroyed, ok := s.contexts[event.ID]; ok {
				delete(s.contexts, event.ID)
				if s.frameContexts[destroyed.frameID] == event.ID {
					delete(s.frameContexts, destroyed.frameID)
				}
				for key := range s.calls {
					if key.contextID == destroyed.uniqueID {
						delete(s.calls, key)
					}
				}
			}
		}
		h.mu.Unlock()
	case "Runtime.executionContextsCleared":
		h.mu.Lock()
		if s := h.sessions[sessionID]; s != nil {
			clear(s.contexts)
			clear(s.frameContexts)
			clear(s.calls)
		}
		h.mu.Unlock()
	case "Runtime.bindingCalled":
		var event struct {
			Name      string `json:"name"`
			Payload   string `json:"payload"`
			ContextID int    `json:"executionContextId"`
		}
		if json.Unmarshal(params, &event) == nil && event.Name == bindingName && len(event.Payload) <= maxCallBytes {
			h.handleCall(sessionID, event.ContextID, event.Payload)
		}
	}
}

type pageCall struct {
	ID        string          `json:"id"`
	Op        string          `json:"op"`
	Mediation string          `json:"mediation"`
	PublicKey json.RawMessage `json:"publicKey"`
}

type pageReply struct {
	ID string `json:"id"`
	Answer
}

func (h *Hook) handleCall(sessionID string, contextID int, payload string) {
	var pc pageCall
	if json.Unmarshal([]byte(payload), &pc) != nil || pc.ID == "" || len(pc.ID) > maxRequestIDLen ||
		(pc.Op != "get" && pc.Op != "create" && pc.Op != "abort") {
		return
	}
	h.mu.Lock()
	s := h.sessions[sessionID]
	if s == nil {
		h.mu.Unlock()
		return
	}
	ec, ok := s.contexts[contextID]
	if !ok {
		h.mu.Unlock()
		return
	}
	key := callKey{contextID: ec.uniqueID, requestID: pc.ID}
	existing := s.calls[key]
	if pc.Op == "abort" {
		// A request the page discarded is not sent to metro-api if it has not
		// been already, and stops retrying if it has.
		if existing != nil && existing.reply == nil {
			existing.cancel()
			delete(s.calls, key)
		}
		h.mu.Unlock()
		return
	}
	if existing != nil {
		reply := existing.reply
		h.mu.Unlock()
		if reply == nil {
			reply = ack(pc.ID)
		}
		go h.deliver(s, ec, reply)
		return
	}
	if !h.admitLocked(s) {
		h.mu.Unlock()
		h.log.Warn("webauthn: too many unanswered requests; leaving the request to the browser", "session", sessionID, "limit", maxPendingCalls)
		go h.deliver(s, ec, encodeReply(pc.ID, notHandled))
		return
	}
	callCtx, cancel := context.WithCancel(context.Background())
	c := &call{cancel: cancel}
	s.calls[key] = c
	enabled := h.enabled
	topOrigin, topKnown := h.topOriginLocked(s)
	h.mu.Unlock()

	go func() {
		defer cancel()
		h.deliver(s, ec, ack(pc.ID))
		answer := notHandled
		origin, secure := secureOrigin(ec.origin)
		top, topSecure := secureOrigin(topOrigin)
		if enabled && secure && topKnown && topSecure && h.permitted(s, ec.frameID, pc.Op) && callCtx.Err() == nil {
			answer = h.cfg.Signer.Answer(callCtx, pc.Op, Request{
				Origin: origin, TopOrigin: top, Mediation: pc.Mediation, PublicKey: pc.PublicKey,
			})
		}
		if callCtx.Err() != nil {
			h.log.Debug("webauthn: page aborted request", "op", pc.Op, "origin", ec.origin)
			return
		}
		h.log.Debug("webauthn: answered page request", "op", pc.Op, "origin", ec.origin, "top_origin", topOrigin, "result", answer.Result)
		reply := encodeReply(pc.ID, answer)
		h.mu.Lock()
		c.reply, c.answeredAt = reply, time.Now()
		h.mu.Unlock()
		h.deliver(s, ec, reply)
	}()
}

// admitLocked reports whether the session may start another request. Only
// unanswered requests count; answered ones are kept for resends until they
// expire, or until the oldest is evicted to bound memory.
func (h *Hook) admitLocked(s *session) bool {
	pending := 0
	var oldest callKey
	var oldestAt time.Time
	for key, c := range s.calls {
		switch {
		case c.reply == nil:
			pending++
		case time.Since(c.answeredAt) > answeredRetained:
			delete(s.calls, key)
		case oldestAt.IsZero() || c.answeredAt.Before(oldestAt):
			oldest, oldestAt = key, c.answeredAt
		}
	}
	if pending >= maxPendingCalls {
		return false
	}
	if len(s.calls) >= maxTrackedCalls {
		delete(s.calls, oldest)
	}
	return true
}

// topOriginLocked follows out-of-process iframes up to their page. Every
// frame in a session shares the session root's top-level document.
func (h *Hook) topOriginLocked(s *session) (string, bool) {
	for range maxFrameDepth {
		if s.targetType == "page" {
			id, ok := s.frameContexts[s.targetID]
			return s.contexts[id].origin, ok
		}
		if s = h.frameOwnerLocked(s.parentFrameID); s == nil {
			return "", false
		}
	}
	return "", false
}

func (h *Hook) frameOwnerLocked(frameID string) *session {
	if frameID == "" {
		return nil
	}
	for _, s := range h.sessions {
		if _, ok := s.frameContexts[frameID]; ok {
			return s
		}
	}
	return nil
}

// permitted applies the document's permissions policy, which governs the
// WebAuthn API in cross-origin frames. A refused request falls back to the
// browser, which rejects it the way it always would.
func (h *Hook) permitted(s *session, frameID, op string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), deliverTimeout)
	defer cancel()
	raw, err := s.cdp.Send(ctx, "Page.getPermissionsPolicyState", map[string]any{"frameId": frameID})
	if err != nil {
		return false
	}
	var result struct {
		States []struct {
			Feature string `json:"feature"`
			Allowed bool   `json:"allowed"`
		} `json:"states"`
	}
	if json.Unmarshal(raw, &result) != nil {
		return false
	}
	feature := "publickey-credentials-" + op
	for _, state := range result.States {
		if state.Feature == feature {
			return state.Allowed
		}
	}
	return false
}

// secureOrigin accepts the origins WebAuthn serves: https, and http on
// localhost and its subdomains. Opaque origins do not parse to a host.
func secureOrigin(origin string) (string, bool) {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", false
	}
	switch {
	case u.Scheme == "https", u.Scheme == "http" && (u.Hostname() == "localhost" || strings.HasSuffix(u.Hostname(), ".localhost")):
		return origin, true
	}
	return "", false
}

func (h *Hook) deliver(s *session, ec executionContext, reply json.RawMessage) {
	ctx, cancel := context.WithTimeout(context.Background(), deliverTimeout)
	defer cancel()
	if _, err := s.cdp.Send(ctx, "Runtime.evaluate", map[string]any{
		"expression":      replyFunction + "(" + string(reply) + ")",
		"uniqueContextId": ec.uniqueID,
		"silent":          true,
	}); err != nil {
		h.log.Debug("webauthn: failed to deliver reply", "session", s.id, "err", err)
	}
}

func ack(id string) json.RawMessage {
	data, _ := json.Marshal(struct {
		ID  string `json:"id"`
		Ack bool   `json:"ack"`
	}{id, true})
	return data
}

func encodeReply(id string, answer Answer) json.RawMessage {
	data, _ := json.Marshal(pageReply{ID: id, Answer: answer})
	return data
}
