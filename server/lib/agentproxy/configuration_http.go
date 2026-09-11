package agentproxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

func (h *Handler) piConfiguration(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPut {
		expected := r.Header.Get("If-Match")
		if expected == "" {
			http.Error(w, "If-Match revision required", http.StatusPreconditionRequired)
			return
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		decoder.DisallowUnknownFields()
		var desired PiConfiguration
		if err := decoder.Decode(&desired); err != nil {
			http.Error(w, "invalid pi configuration", http.StatusBadRequest)
			return
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			http.Error(w, "expected one configuration", http.StatusBadRequest)
			return
		}
		if err := h.config.Pi.validateDesired(desired); err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		// Normalize empty arrays so configuration responses never serialize them as null.
		if desired.Shared.Extensions == nil {
			desired.Shared.Extensions = make([]string, 0)
		}
		if desired.Shared.MCPServers == nil {
			desired.Shared.MCPServers = make([]ManagedMCPServer, 0)
		}
		data, _ := json.Marshal(desired)
		ctx, cancel := context.WithCancel(r.Context())
		stop := context.AfterFunc(h.ctx, cancel)
		defer stop()
		defer cancel()
		if err := h.pi.apply(ctx, strings.Trim(expected, "\""), data); err != nil {
			if errors.Is(err, errConfigurationConflict) {
				http.Error(w, err.Error(), http.StatusConflict)
				return
			}
			http.Error(w, "configuration preparation failed; inspect GET configuration status", http.StatusUnprocessableEntity)
			return
		}
	} else if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET, PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	state := h.pi.snapshot()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("ETag", "\""+state.Revision+"\"")
	_ = json.NewEncoder(w).Encode(state)
}
