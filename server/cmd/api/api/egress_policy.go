package api

import (
	"context"

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
// browser context on its own proxy.
func (s *ApiService) PutNetworkEgressPolicy(ctx context.Context, req oapi.PutNetworkEgressPolicyRequestObject) (oapi.PutNetworkEgressPolicyResponseObject, error) {
	log := logger.FromContext(ctx)
	if req.Body == nil {
		return oapi.PutNetworkEgressPolicy400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "missing body"}}, nil
	}
	// A policy that was not persisted would be lost if this process restarted,
	// so the caller is told it did not apply rather than being left to assume
	// a refusal is in place.
	if err := s.egressPolicy.SetFiltered(req.Body.Filtered); err != nil {
		log.Error("failed to apply egress policy", "err", err, "filtered", req.Body.Filtered)
		return oapi.PutNetworkEgressPolicy500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to apply egress policy"}}, nil
	}
	log.Info("egress policy applied", "filtered", req.Body.Filtered)
	return oapi.PutNetworkEgressPolicy200JSONResponse{Filtered: req.Body.Filtered}, nil
}
