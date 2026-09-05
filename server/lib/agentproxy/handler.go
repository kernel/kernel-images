package agentproxy

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/kernel/kernel-images/server/lib/wsdrain"
	"github.com/kernel/kernel-images/server/lib/wsproxy"
)

type Handler struct {
	ctx      context.Context
	config   Config
	logger   *slog.Logger
	registry *wsdrain.Registry
	slots    chan struct{}
}

func New(ctx context.Context, config Config, logger *slog.Logger, registry *wsdrain.Registry) (*Handler, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	return &Handler{ctx: ctx, config: config, logger: logger, registry: registry, slots: make(chan struct{}, config.MaxConnections)}, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/agent/v1/harnesses":
		names := make([]string, 0, len(h.config.Harnesses))
		for name := range h.config.Harnesses {
			names = append(names, name)
		}
		sort.Strings(names)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Configured []string `json:"configured"`
		}{names})
	case r.Method == http.MethodGet && r.URL.Path == "/agent/v1/acp":
		h.connect(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (h *Handler) connect(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("harness")
	harness, ok := h.config.Harnesses[name]
	if !ok {
		http.Error(w, "harness is not configured", http.StatusNotFound)
		return
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		http.Error(w, "WebSocket upgrade required", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(h.ctx, cancel)
	defer stop()
	defer cancel()
	if h.ctx.Err() != nil {
		http.Error(w, "agent service is shutting down", http.StatusServiceUnavailable)
		return
	}
	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	default:
		http.Error(w, "agent connection limit reached", http.StatusTooManyRequests)
		return
	}
	b, err := startBridge(ctx, h.config.ACPRemote, harness)
	if err != nil {
		h.logger.Warn("ACP bridge startup failed", "harness", name, "err", err)
		http.Error(w, "ACP bridge unavailable", http.StatusServiceUnavailable)
		return
	}
	defer b.close()
	wsproxy.Proxy(w, r.WithContext(ctx), b.url, wsproxy.ProxyOptions{
		// Same browser-scoped authentication boundary as the process attach API.
		AcceptOptions: &websocket.AcceptOptions{OriginPatterns: []string{"*"}, Subprotocols: []string{"acp.v1"}},
		DialOptions:   &websocket.DialOptions{HTTPHeader: b.headers},
		Logger:        h.logger,
		Registry:      h.registry,
		ReadLimit:     1 << 20, // acpremote 1.7.0's default message limit
		PingInterval:  20 * time.Second,
	})
}
