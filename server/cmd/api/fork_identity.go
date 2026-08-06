package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/kernel/kernel-images/server/lib/forkidentity"
)

// onApplied runs once the guest has taken the identity, so services this
// process owns can retarget themselves to it. It is called with the applied
// payload and only on success; pass nil when there is nothing to retarget.
// It must not block: the handoff is the control plane's critical path and
// does not wait on optional sinks.
func forkIdentityHandler(log *slog.Logger, onApplied func(forkidentity.Payload)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		enabled, err := forkidentity.WaitEnabled()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !enabled {
			http.Error(w, "fork identity wait is disabled", http.StatusConflict)
			return
		}
		appliedInstance, err := forkidentity.ReadAppliedMarker()
		if err != nil {
			log.Error("fork identity applied marker read failed", "err", err)
			http.Error(w, "failed to read fork identity", http.StatusInternalServerError)
			return
		}
		if appliedInstance != "" {
			http.Error(w, "fork identity already applied", http.StatusConflict)
			return
		}

		var payload forkidentity.Payload
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, forkidentity.MaxPayloadBytes))
		if err := dec.Decode(&payload); err != nil {
			http.Error(w, fmt.Sprintf("decode payload: %v", err), http.StatusBadRequest)
			return
		}
		if err := payload.Validate(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := os.Remove(forkidentity.AppliedFile); err != nil && !os.IsNotExist(err) {
			log.Error("fork identity applied marker reset failed", "err", err)
			http.Error(w, "failed to reset fork identity", http.StatusInternalServerError)
			return
		}
		if err := forkidentity.WritePayload(payload); err != nil {
			log.Error("fork identity payload write failed", "err", err)
			http.Error(w, "failed to write fork identity", http.StatusInternalServerError)
			return
		}
		if err := forkidentity.WaitAppliedMarker(payload.InstanceName(), forkidentity.ApplyTimeout); err != nil {
			log.Error("fork identity apply wait failed", "err", err)
			http.Error(w, "fork identity not applied", http.StatusInternalServerError)
			return
		}
		if onApplied != nil {
			onApplied(payload)
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func forkIdentityConfigHandler(log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		payload, err := forkidentity.ReadPayload()
		if err != nil {
			if os.IsNotExist(err) {
				enabled, parseErr := forkidentity.WaitEnabled()
				if parseErr != nil {
					http.Error(w, parseErr.Error(), http.StatusInternalServerError)
					return
				}
				if enabled {
					w.WriteHeader(http.StatusAccepted)
					return
				}
				http.NotFound(w, r)
				return
			}
			log.Error("fork identity config read failed", "err", err)
			http.Error(w, "failed to read fork identity", http.StatusInternalServerError)
			return
		}
		enabled, err := forkidentity.WaitEnabled()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if enabled {
			applied, err := forkidentity.AppliedMarkerMatches(payload.InstanceName())
			if err != nil {
				log.Error("fork identity applied marker read failed", "err", err)
				http.Error(w, "failed to read fork identity", http.StatusInternalServerError)
				return
			}
			if !applied {
				w.WriteHeader(http.StatusAccepted)
				return
			}
		}
		resp := forkidentity.ExtensionConfigFromPayload(payload)
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			log.Error("fork identity config encode failed", "err", err)
		}
	}
}
