package api

import (
	"context"
	"errors"

	"github.com/kernel/kernel-images/server/lib/logger"
	oapi "github.com/kernel/kernel-images/server/lib/oapi"
)

// GetNetworkEgressPolicy handles GET /network/egress-policy.
func (s *ApiService) GetNetworkEgressPolicy(_ context.Context, _ oapi.GetNetworkEgressPolicyRequestObject) (oapi.GetNetworkEgressPolicyResponseObject, error) {
	return oapi.GetNetworkEgressPolicy200JSONResponse{Filtered: s.egressPolicy.Filtered()}, nil
}

// PutNetworkEgressPolicy handles PUT /network/egress-policy.
// Records whether the session's egress is restricted to an allowlist, which is
// what the CDP proxy consults before forwarding a command that would put a
// browser context on its own proxy, and pins Chromium's proxy with managed
// policy so an extension cannot switch it to direct connections.
func (s *ApiService) PutNetworkEgressPolicy(ctx context.Context, req oapi.PutNetworkEgressPolicyRequestObject) (oapi.PutNetworkEgressPolicyResponseObject, error) {
	log := logger.FromContext(ctx)
	if req.Body == nil {
		return oapi.PutNetworkEgressPolicy400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "missing body"}}, nil
	}

	// The pin is written by the launcher from the flags Chromium starts with,
	// so applying it means a restart, which must not interleave with another
	// configuration change.
	s.chromiumConfigMu.Lock()
	defer s.chromiumConfigMu.Unlock()

	// A policy that was not persisted would be lost if this process restarted,
	// so the caller is told it did not apply rather than being left to assume
	// a refusal is in place.
	if err := s.egressPolicy.SetFiltered(req.Body.Filtered); err != nil {
		log.Error("failed to apply egress policy", "err", err, "filtered", req.Body.Filtered)
		return oapi.PutNetworkEgressPolicy500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to apply egress policy"}}, nil
	}
	if err := s.applyEgressPin(ctx, req.Body.Filtered); err != nil {
		log.Error("failed to apply egress proxy pin", "err", err, "filtered", req.Body.Filtered)
		return oapi.PutNetworkEgressPolicy500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to apply egress policy"}}, nil
	}
	log.Info("egress policy applied", "filtered", req.Body.Filtered)
	return oapi.PutNetworkEgressPolicy200JSONResponse{Filtered: req.Body.Filtered}, nil
}

// applyEgressPin brings Chromium's proxy pin in line with the policy. The
// caller must hold chromiumConfigMu.
//
// A missing pin is written by restarting Chromium rather than by waiting for
// it to reload its policy directory, so the pin is in force before the control
// plane hands the session to a client. Chromium does not need a restart for the
// pin to come off.
//
// The launcher does not take chromiumConfigMu, so a Chromium that crashed and is
// restarting as the policy flips to unfiltered can write the pin again after it
// is removed. That leaves the session pinned until Chromium next starts, which
// fails closed.
func (s *ApiService) applyEgressPin(ctx context.Context, filtered bool) error {
	if !filtered {
		return s.egressPin.Remove()
	}
	present, err := s.egressPin.Present()
	if err != nil || present {
		return err
	}
	if err := s.restartChromiumAndWait(ctx, "egress policy"); err != nil {
		return err
	}
	present, err = s.egressPin.Present()
	if err != nil {
		return err
	}
	if !present {
		return errors.New("chromium restarted without the egress proxy pin")
	}
	return nil
}
