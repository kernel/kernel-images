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

import (
	"encoding/json"
	"fmt"
	"log/slog"

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
	var cmd struct {
		ID        *int64                     `json:"id"`
		SessionID string                     `json:"sessionId"`
		Params    map[string]json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(msg, &cmd); err != nil {
		return nil, false
	}
	param, ok := contextProxyParam(cmd.Params)
	if !ok {
		return nil, false
	}
	g.logger.Warn("refused CDP browser context proxy on a session with an egress allowlist",
		slog.String("method", probe.Method),
		slog.String("param", param))
	// A command with no id gets no reply, because a response is matched to its
	// command by id. It is still refused: Chromium rejects an id-less command
	// without running it, so the client sees the same outcome either way.
	if cmd.ID == nil {
		return nil, true
	}
	reply, _ := json.Marshal(cdpErrorResponse{
		ID:        *cmd.ID,
		SessionID: cmd.SessionID,
		Error: cdpError{
			Code: cdpInvalidParams,
			Message: fmt.Sprintf(
				"%s: %s is not allowed on a browser with an egress allowlist, because the context would reach destinations directly instead of through the proxy that enforces network.allowed_hosts. Create the context without a proxy, or create the browser without network.allowed_hosts.",
				targetCreateBrowserContext, param),
		},
	})
	return reply, true
}

// contextProxyParam names the proxy argument the command carried, if any.
func contextProxyParam(params map[string]json.RawMessage) (string, bool) {
	for _, name := range contextProxyParams {
		if raw, ok := params[name]; ok && string(raw) != "null" {
			return name, true
		}
	}
	return "", false
}
