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

// Pump bidirectionally copies messages between client and upstream until
// either side errors or ctx is cancelled, then calls onClose.
// If transform is non-nil it is called for every message; the returned bytes
// are forwarded to the other side.
func Pump(ctx context.Context, client, upstream Conn, onClose func(), logger *slog.Logger, transform MessageTransform) {
	errChan := make(chan error, 2)

	go func() {
		for {
			mt, msg, err := client.Read(ctx)
			if err != nil {
				logger.Error("client read error", slog.String("err", err.Error()))
				errChan <- err
				return
			}
			if transform != nil {
				msg = transform("->", mt, msg)
			}
			if err := upstream.Write(ctx, mt, msg); err != nil {
				logger.Error("upstream write error", slog.String("err", err.Error()))
				errChan <- err
				return
			}
		}
	}()

	go func() {
		for {
			mt, msg, err := upstream.Read(ctx)
			if err != nil {
				logger.Error("upstream read error", slog.String("err", err.Error()))
				errChan <- err
				return
			}
			if transform != nil {
				msg = transform("<-", mt, msg)
			}
			if err := client.Write(ctx, mt, msg); err != nil {
				logger.Error("client write error", slog.String("err", err.Error()))
				errChan <- err
				return
			}
		}
	}()

	select {
	case <-ctx.Done():
	case <-errChan:
	}
	onClose()
}

// Proxy accepts a client WebSocket upgrade, dials the upstream URL, and pumps
// messages bidirectionally until either side closes. acceptOpts and dialOpts
// may be nil for defaults. If transform is non-nil it is called for every
// message and the returned bytes are forwarded.
func Proxy(w http.ResponseWriter, r *http.Request, upstreamURL string, acceptOpts *websocket.AcceptOptions, dialOpts *websocket.DialOptions, logger *slog.Logger, transform MessageTransform) {
	if acceptOpts == nil {
		acceptOpts = &websocket.AcceptOptions{OriginPatterns: []string{"*"}}
	}
	clientConn, err := websocket.Accept(w, r, acceptOpts)
	if err != nil {
		logger.Error("websocket accept failed", slog.String("err", err.Error()))
		return
	}
	clientConn.SetReadLimit(100 * 1024 * 1024)

	upstreamConn, _, err := websocket.Dial(r.Context(), upstreamURL, dialOpts)
	if err != nil {
		logger.Error("dial upstream failed", slog.String("err", err.Error()), slog.String("url", upstreamURL))
		clientConn.Close(websocket.StatusInternalError, "failed to connect to upstream")
		return
	}
	upstreamConn.SetReadLimit(100 * 1024 * 1024)

	logger.Debug("proxying websocket", slog.String("url", upstreamURL))

	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			upstreamConn.Close(websocket.StatusNormalClosure, "")
			clientConn.Close(websocket.StatusNormalClosure, "")
		})
	}

	Pump(r.Context(), clientConn, upstreamConn, cleanup, logger, transform)
}
