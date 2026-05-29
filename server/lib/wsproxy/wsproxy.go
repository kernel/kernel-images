package wsproxy

import (
	"context"
	"log/slog"
	"net/http"
	"sync"

	"github.com/coder/websocket"
)

// Conn abstracts a WebSocket connection for testing and flexibility.
type Conn interface {
	Read(ctx context.Context) (websocket.MessageType, []byte, error)
	Write(ctx context.Context, typ websocket.MessageType, p []byte) error
	Close(statusCode websocket.StatusCode, reason string) error
}

// MessageTransform is called for every message flowing through the proxy.
// direction is "->" for client-to-upstream and "<-" for upstream-to-client.
// It returns the (possibly modified) message bytes to forward.
type MessageTransform func(direction string, mt websocket.MessageType, msg []byte) []byte

// ProxyOptions configures the proxy accept/dial behavior and optional message
// transformation. Zero values are valid and use sensible defaults.
type ProxyOptions struct {
	AcceptOptions *websocket.AcceptOptions
	DialOptions   *websocket.DialOptions
	Logger        *slog.Logger
	Transform     MessageTransform
}

// PumpExitCause names which side caused Pump to return. Callers use this to
// distinguish a clean client close from an upstream failure or context
// cancellation when deciding telemetry attribution and reconnect policy.
type PumpExitCause string

const (
	// PumpExitClient indicates the client-side read or upstream-side write
	// returned an error first (typically: client closed the WS).
	PumpExitClient PumpExitCause = "client"
	// PumpExitUpstream indicates the upstream-side read or client-side write
	// returned an error first (typically: upstream died or restarted).
	PumpExitUpstream PumpExitCause = "upstream"
	// PumpExitContext indicates the pump's context was cancelled before
	// either side errored (typically: server shutdown).
	PumpExitContext PumpExitCause = "context"
)

// Pump bidirectionally copies messages between client and upstream until
// either side errors or ctx is cancelled, then calls onClose with the cause.
// If transform is non-nil it is called for every message; the returned bytes
// are forwarded to the other side.
func Pump(ctx context.Context, client, upstream Conn, onClose func(cause PumpExitCause), logger *slog.Logger, transform MessageTransform) {
	causeChan := make(chan PumpExitCause, 2)

	go func() {
		for {
			mt, msg, err := client.Read(ctx)
			if err != nil {
				logger.Error("client read error", slog.String("err", err.Error()))
				causeChan <- PumpExitClient
				return
			}
			if transform != nil {
				msg = transform("->", mt, msg)
			}
			if err := upstream.Write(ctx, mt, msg); err != nil {
				logger.Error("upstream write error", slog.String("err", err.Error()))
				causeChan <- PumpExitUpstream
				return
			}
		}
	}()

	go func() {
		for {
			mt, msg, err := upstream.Read(ctx)
			if err != nil {
				logger.Error("upstream read error", slog.String("err", err.Error()))
				causeChan <- PumpExitUpstream
				return
			}
			if transform != nil {
				msg = transform("<-", mt, msg)
			}
			if err := client.Write(ctx, mt, msg); err != nil {
				logger.Error("client write error", slog.String("err", err.Error()))
				causeChan <- PumpExitClient
				return
			}
		}
	}()

	var cause PumpExitCause
	select {
	case <-ctx.Done():
		cause = PumpExitContext
	case cause = <-causeChan:
	}
	onClose(cause)
}

// Proxy accepts a client WebSocket upgrade, dials the upstream URL, and pumps
// messages bidirectionally until either side closes. ProxyOptions fields are
// optional and use defaults when omitted.
func Proxy(w http.ResponseWriter, r *http.Request, upstreamURL string, opts ProxyOptions) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	acceptOpts := opts.AcceptOptions
	if acceptOpts == nil {
		acceptOpts = &websocket.AcceptOptions{OriginPatterns: []string{"*"}}
	}
	clientConn, err := websocket.Accept(w, r, acceptOpts)
	if err != nil {
		logger.Error("websocket accept failed", slog.String("err", err.Error()))
		return
	}
	clientConn.SetReadLimit(100 * 1024 * 1024)

	upstreamConn, _, err := websocket.Dial(r.Context(), upstreamURL, opts.DialOptions)
	if err != nil {
		logger.Error("dial upstream failed", slog.String("err", err.Error()), slog.String("url", upstreamURL))
		clientConn.Close(websocket.StatusInternalError, "failed to connect to upstream")
		return
	}
	upstreamConn.SetReadLimit(100 * 1024 * 1024)

	logger.Debug("proxying websocket", slog.String("url", upstreamURL))

	var once sync.Once
	cleanup := func(_ PumpExitCause) {
		once.Do(func() {
			upstreamConn.Close(websocket.StatusNormalClosure, "")
			clientConn.Close(websocket.StatusNormalClosure, "")
		})
	}

	Pump(r.Context(), clientConn, upstreamConn, cleanup, logger, opts.Transform)
}
