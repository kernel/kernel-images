package wsproxy

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/kernel/kernel-images/server/lib/wsdrain"
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

// Observer is called after a message has been successfully written to the
// other side, with ts set to the time that write completed (Unix
// microseconds). It runs on the pump goroutine, so anything it does delays the
// next message: hand work to a worker rather than doing it here. msg is not
// retained by the pump after the call, so an observer may take ownership.
type Observer func(direction string, mt websocket.MessageType, msg []byte, ts int64)

// Gate inspects a client-to-upstream message before it is forwarded. refuse
// reports whether to hold the message back; reply, when non-empty, is written
// to the client in its place so the caller still gets an answer to the request
// it made. The two are separate so that a gate which cannot build a reply
// still refuses the message rather than letting it through.
//
// Unlike an Observer, a Gate runs ahead of the forward, so whatever it does is
// latency on the message it is looking at rather than on the next one. Keep it
// to a decision.
type Gate func(mt websocket.MessageType, msg []byte) (reply []byte, refuse bool)

// ProxyOptions configures the proxy accept/dial behavior and optional message
// transformation. Zero values are valid and use sensible defaults.
type ProxyOptions struct {
	AcceptOptions *websocket.AcceptOptions
	DialOptions   *websocket.DialOptions
	Logger        *slog.Logger
	Transform     MessageTransform
	Observe       Observer
	// Registry, when set, tracks the accepted client connection so it is
	// closed with a Going Away frame on server shutdown.
	Registry *wsdrain.Registry
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
// are forwarded to the other side. If gate is non-nil it is called for every
// client-to-upstream message after transform, and a message it refuses is
// answered to the client instead of being forwarded. If observe is non-nil it
// is called for every message that was forwarded successfully, so a message
// whose write failed, or that the gate refused, is never observed.
func Pump(ctx context.Context, client, upstream Conn, onClose func(cause PumpExitCause), logger *slog.Logger, transform MessageTransform, gate Gate, observe Observer) {
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
			if gate != nil {
				if reply, refuse := gate(mt, msg); refuse {
					// Writing to the client from this goroutine while the
					// other one writes upstream frames to it is supported:
					// coder/websocket allows concurrent Write.
					if len(reply) > 0 {
						if err := client.Write(ctx, mt, reply); err != nil {
							logger.Error("client write error", slog.String("err", err.Error()))
							causeChan <- PumpExitClient
							return
						}
					}
					continue
				}
			}
			if err := upstream.Write(ctx, mt, msg); err != nil {
				logger.Error("upstream write error", slog.String("err", err.Error()))
				causeChan <- PumpExitUpstream
				return
			}
			if observe != nil {
				observe("->", mt, msg, time.Now().UnixMicro())
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
			if observe != nil {
				observe("<-", mt, msg, time.Now().UnixMicro())
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

	untrack := opts.Registry.Track(clientConn)
	defer untrack()

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

	Pump(r.Context(), clientConn, upstreamConn, cleanup, logger, opts.Transform, nil, opts.Observe)
}
