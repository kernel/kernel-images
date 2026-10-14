package api

import (
	"context"
	"errors"

	"github.com/kernel/kernel-images/server/lib/logger"
	oapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/kernel/kernel-images/server/lib/webauthnhook"
)

// PutWebAuthnHook handles PUT /webauthn/hook.
func (s *ApiService) PutWebAuthnHook(ctx context.Context, req oapi.PutWebAuthnHookRequestObject) (oapi.PutWebAuthnHookResponseObject, error) {
	log := logger.FromContext(ctx)
	if req.Body == nil {
		return oapi.PutWebAuthnHook400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "missing body"}}, nil
	}
	if err := s.webauthnHook.Set(ctx, req.Body.Enabled); errors.Is(err, webauthnhook.ErrNotConfigured) {
		return oapi.PutWebAuthnHook400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: err.Error()}}, nil
	} else if err != nil {
		log.Error("failed to apply WebAuthn hook", "err", err, "enabled", req.Body.Enabled)
		return oapi.PutWebAuthnHook500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to apply WebAuthn hook"}}, nil
	}
	log.Info("WebAuthn hook applied", "enabled", req.Body.Enabled)
	return oapi.PutWebAuthnHook200JSONResponse{Enabled: req.Body.Enabled}, nil
}
