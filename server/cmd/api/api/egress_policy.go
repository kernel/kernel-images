package api

import (
	"context"
	"errors"
	"strings"
	"unicode"

	"github.com/kernel/kernel-images/server/lib/egresspolicy"
	"github.com/kernel/kernel-images/server/lib/logger"
	oapi "github.com/kernel/kernel-images/server/lib/oapi"
)

// GetNetworkEgressPolicy handles GET /network/egress-policy.
func (s *ApiService) GetNetworkEgressPolicy(_ context.Context, _ oapi.GetNetworkEgressPolicyRequestObject) (oapi.GetNetworkEgressPolicyResponseObject, error) {
	policy := s.egressPolicy.Policy()
	return oapi.GetNetworkEgressPolicy200JSONResponse{Filtered: policy.Filtered, PrivateHosts: policy.PrivateHosts}, nil
}

// PutNetworkEgressPolicy handles PUT /network/egress-policy.
// Records whether the session's egress is restricted to an allowlist, which the
// CDP proxy consults before forwarding a command that would put a browser
// context on its own proxy, and pins Chromium's proxy with managed policy.
func (s *ApiService) PutNetworkEgressPolicy(ctx context.Context, req oapi.PutNetworkEgressPolicyRequestObject) (oapi.PutNetworkEgressPolicyResponseObject, error) {
	log := logger.FromContext(ctx)
	if req.Body == nil {
		return oapi.PutNetworkEgressPolicy400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "missing body"}}, nil
	}
	if req.Body.PrivateHosts != nil {
		for _, host := range *req.Body.PrivateHosts {
			// Chromium splits its bypass list on these.
			if host == "" || strings.ContainsAny(host, ";,") || strings.ContainsFunc(host, unicode.IsSpace) {
				return oapi.PutNetworkEgressPolicy400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "private_hosts entries must be non-empty and contain no ';', ',' or whitespace"}}, nil
			}
		}
	}
	policy := egresspolicy.Policy{Filtered: req.Body.Filtered, PrivateHosts: req.Body.PrivateHosts}

	// Applying the pin can restart Chromium.
	s.chromiumConfigMu.Lock()
	defer s.chromiumConfigMu.Unlock()

	// A policy that was not persisted would be lost if this process restarted,
	// so the caller is told it did not apply rather than being left to assume
	// a refusal is in place.
	if err := s.egressPolicy.Set(policy); err != nil {
		log.Error("failed to apply egress policy", "err", err, "filtered", policy.Filtered)
		return oapi.PutNetworkEgressPolicy500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to apply egress policy"}}, nil
	}
	if err := s.applyEgressPin(ctx, policy); err != nil {
		log.Error("failed to apply egress proxy pin", "err", err, "filtered", policy.Filtered)
		return oapi.PutNetworkEgressPolicy500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to apply egress policy"}}, nil
	}
	log.Info("egress policy applied", "filtered", policy.Filtered)
	return oapi.PutNetworkEgressPolicy200JSONResponse{Filtered: policy.Filtered, PrivateHosts: policy.PrivateHosts}, nil
}

// applyEgressPin brings Chromium's proxy pin in line with the policy. The
// caller must hold chromiumConfigMu.
//
// A missing or stale pin is written by restarting Chromium, so it is in force
// before the request returns; Chromium's own policy reload would be late.
// Removal needs no restart. A Chromium restarting on its own as the policy
// flips to unfiltered can write the pin back, which fails closed until its
// next start.
func (s *ApiService) applyEgressPin(ctx context.Context, policy egresspolicy.Policy) error {
	if !policy.Filtered {
		return s.egressPin.Remove()
	}
	pinned, err := s.egressPin.Matches(policy, s.chromiumBaseFlags)
	if err != nil {
		return err
	}
	if pinned {
		return nil
	}
	if err := s.restartChromiumAndWait(ctx, "egress policy"); err != nil {
		return err
	}
	pinned, err = s.egressPin.Matches(policy, s.chromiumBaseFlags)
	if err != nil {
		return err
	}
	if !pinned {
		return errors.New("chromium restarted without the egress proxy pin")
	}
	return nil
}
