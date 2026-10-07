package devtoolsproxy

// Refusal of the CDP commands that would put a browser context on its own
// proxy, for a session whose egress is restricted to an allowlist.
//
// Chromium is launched with --proxy-server pointing at Kernel's egress proxy,
// and the allowlist is enforced there. Target.createBrowserContext lets a CDP
// client give a new context a different proxy, and the VM has a direct route
// to the internet, so a context that sets one reaches origins without the
// egress proxy ever seeing the request. Playwright spells this
// browser.newContext({ proxy }), which makes it a one-line way for the client
// an allowlist is meant to contain to step out of it.
//
// The refusal happens here rather than in Chromium because the proxy is the
// one place every CDP client passes through: a customer's own connection, the
// in-VM Playwright daemon, and ChromeDriver, which is handed this proxy's
// address as its debuggerAddress.
//
// Reading the top-level method of a text frame is enough to see every way a
// client can send this command, which was checked against a real browser
// rather than assumed:
//
//   - A flat session from Target.attachToBrowserTarget creates contexts, and
//     its commands carry the method at the top level with a sessionId beside
//     it, so they are decoded here like any other.
//   - Target.sendMessageToTarget, which would nest the command out of sight,
//     is refused by Chromium itself: with the session in the params it answers
//     "When using flat protocol, messages are routed to the target via the
//     sessionId attribute", and with the session on the envelope, "Session id
//     must be specified". Neither created a context.
//   - A page session answers "Not allowed", so a context cannot be created
//     from one at all.
//   - A binary frame carrying the same JSON creates nothing; Chromium drops
//     the connection instead of acting on it. That is why only text frames are
//     inspected.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"

	"github.com/coder/websocket"
)

// EgressFilteredFunc reports whether the session's egress is restricted to an
// allowlist. It is consulted before each client command is forwarded, so it
// must be cheap: egresspolicy.State.Filtered is a lock-free load.
type EgressFilteredFunc func() bool

const targetCreateBrowserContext = "Target.createBrowserContext"

// contextProxyParams are the Target.createBrowserContext arguments that decide
// a context's proxy. Presence is what matters, not the value: an empty
// proxyServer is enough to put the context on direct connections, so a check
// for a non-empty string would miss it.
//
// proxyBypassList cannot bypass on its own, because a bypass list only applies
// to a proxy the context set itself. It is refused anyway: it is meaningless
// without proxyServer, so nothing is lost by rejecting it, and silently
// accepting it would leave the refusal resting on a Chromium behaviour nobody
// here controls.
var contextProxyParams = []string{"proxyServer", "proxyBypassList"}

// cdpInvalidParams is the DevTools protocol's InvalidParams code. A refusal on
// policy grounds is not one of the protocol's own codes; clients surface the
// message rather than the number, so this uses the closest standard code
// instead of inventing one.
const cdpInvalidParams = -32602

type cdpErrorResponse struct {
	ID        int64    `json:"id"`
	SessionID string   `json:"sessionId,omitempty"`
	Error     cdpError `json:"error"`
}

type cdpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// egressGate refuses context-proxy commands while the session is filtered.
type egressGate struct {
	filtered EgressFilteredFunc
	logger   *slog.Logger
}

// newEgressGate returns a gate for the connection, or nil when the image has
// no way to know the session's policy, which leaves forwarding untouched.
func newEgressGate(filtered EgressFilteredFunc, logger *slog.Logger) *egressGate {
	if filtered == nil {
		return nil
	}
	return &egressGate{filtered: filtered, logger: logger}
}

// refusal is why a command is held back: reason names it for the log, message
// explains it to the client. The zero value forwards the command.
type refusal struct {
	reason  string
	message string
}

func (r refusal) refuses() bool { return r.message != "" }

// refuse answers a client command that would give a context its own proxy.
func (g *egressGate) refuse(mt websocket.MessageType, msg []byte) ([]byte, bool) {
	if g == nil || mt != websocket.MessageText || !g.filtered() {
		return nil, false
	}
	// Resolve the method first and without copying the arguments, the way
	// admission does: a client library sends far more DOM and Runtime traffic
	// than this, and none of it should pay for a full decode.
	var probe cdpCommandMethod
	if err := json.Unmarshal(msg, &probe); err != nil || probe.Method != targetCreateBrowserContext {
		return nil, false
	}
	// Past here the frame names the gated command, so its fields are read one
	// at a time and anything unreadable is refused rather than forwarded.
	// Binding the envelope to typed fields made the decision depend on the
	// shape of fields it does not care about: an id of 1.0 failed a decode
	// into an integer and took the whole command through unchecked, while
	// Chromium accepted that id and created the context.
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(msg, &envelope); err != nil {
		// The probe already decoded this frame as an object, so this is the
		// fail-closed default rather than a path a client can reach.
		g.log(probe.Method, "unreadable_envelope")
		return nil, true
	}
	ref := refusalFor(envelope["params"])
	if !ref.refuses() {
		return nil, false
	}
	g.log(probe.Method, ref.reason)
	// A reply is matched to its command by id, so one that does not carry a
	// usable id is not worth sending. The command is still refused: Chromium
	// runs neither an id-less command nor one whose id it cannot read, so the
	// client sees the same outcome either way.
	id, ok := commandID(envelope["id"])
	if !ok {
		return nil, true
	}
	reply, _ := json.Marshal(cdpErrorResponse{
		ID:        id,
		SessionID: commandSessionID(envelope["sessionId"]),
		Error:     cdpError{Code: cdpInvalidParams, Message: ref.message},
	})
	return reply, true
}

func (g *egressGate) log(method, reason string) {
	g.logger.Warn("refused CDP browser context proxy on a session with an egress allowlist",
		slog.String("method", method),
		slog.String("reason", reason))
}

// refusalFor decides a command from its raw params alone, so the rest of the
// envelope cannot change the answer.
func refusalFor(rawParams json.RawMessage) refusal {
	if len(rawParams) == 0 || string(rawParams) == "null" {
		return refusal{}
	}
	var params map[string]json.RawMessage
	if err := json.Unmarshal(rawParams, &params); err != nil {
		return refusal{
			reason: "unreadable_params",
			message: targetCreateBrowserContext + ": the parameters could not be read, so they could not be checked" +
				" against the browser's egress allowlist. Send parameters as an object.",
		}
	}
	for _, name := range contextProxyParams {
		if value, ok := params[name]; ok && string(value) != "null" {
			return refusal{
				reason: name,
				message: fmt.Sprintf(
					"%s: %s is not allowed on a browser with an egress allowlist, because the context would reach destinations directly instead of through the proxy that enforces network.allowed_hosts. Create the context without a proxy, or create the browser without network.allowed_hosts.",
					targetCreateBrowserContext, name),
			}
		}
	}
	return refusal{}
}

// commandID reads the id a reply has to carry. Chromium answers an integral
// float such as 1.0 with the integer, so this accepts one the same way; a
// string, a fraction, or anything too large for the reply is not something it
// can answer, and the command is refused without one.
func commandID(raw json.RawMessage) (int64, bool) {
	trimmed := bytes.TrimSpace(raw)
	// json.Number takes a JSON string as readily as a number, and Chromium
	// does not answer a string id at all, so one is rejected before decoding.
	if len(trimmed) == 0 || trimmed[0] == '"' {
		return 0, false
	}
	var number json.Number
	if err := json.Unmarshal(trimmed, &number); err != nil {
		return 0, false
	}
	if id, err := number.Int64(); err == nil {
		return id, true
	}
	value, err := number.Float64()
	if err != nil || value != math.Trunc(value) || value < math.MinInt64 || value > math.MaxInt64 {
		return 0, false
	}
	return int64(value), true
}

// commandSessionID reads the session a reply is matched to, if the command
// named one readably. It only labels the reply, so an unreadable one is
// dropped rather than refusing the command over it.
func commandSessionID(raw json.RawMessage) string {
	var sessionID string
	if len(raw) == 0 || json.Unmarshal(raw, &sessionID) != nil {
		return ""
	}
	return sessionID
}
