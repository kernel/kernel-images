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
	if req.Body == nil {
		return oapi.PutNetworkEgressPolicy400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "missing body"}}, nil
	}
	s.egressPolicy.SetFiltered(req.Body.Filtered)
	logger.FromContext(ctx).Info("egress policy applied", "filtered", req.Body.Filtered)
	return oapi.PutNetworkEgressPolicy200JSONResponse{Filtered: req.Body.Filtered}, nil
}
