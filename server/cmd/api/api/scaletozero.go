package api

import (
	"context"

	"github.com/kernel/kernel-images/server/lib/logger"
	oapi "github.com/kernel/kernel-images/server/lib/oapi"
)

func (s *ApiService) DisableScaleToZero(ctx context.Context, _ oapi.DisableScaleToZeroRequestObject) (oapi.DisableScaleToZeroResponseObject, error) {
	if err := s.stz.Pin(ctx); err != nil {
		logger.FromContext(ctx).Error("failed to disable scale-to-zero", "err", err)
		return oapi.DisableScaleToZero500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to disable scale-to-zero"}}, nil
	}
	return oapi.DisableScaleToZero204Response{}, nil
}

func (s *ApiService) EnableScaleToZero(ctx context.Context, _ oapi.EnableScaleToZeroRequestObject) (oapi.EnableScaleToZeroResponseObject, error) {
	if err := s.stz.Unpin(ctx); err != nil {
		logger.FromContext(ctx).Error("failed to enable scale-to-zero", "err", err)
		return oapi.EnableScaleToZero500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to enable scale-to-zero"}}, nil
	}
	return oapi.EnableScaleToZero204Response{}, nil
}
