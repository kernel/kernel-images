package api

import (
	"context"

	"github.com/kernel/kernel-images/server/lib/logger"
	oapi "github.com/kernel/kernel-images/server/lib/oapi"
)

func (s *ApiService) PinScaleToZero(ctx context.Context, _ oapi.PinScaleToZeroRequestObject) (oapi.PinScaleToZeroResponseObject, error) {
	if err := s.stz.Pin(ctx); err != nil {
		logger.FromContext(ctx).Error("failed to pin scale-to-zero", "err", err)
		return oapi.PinScaleToZero500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to pin scale-to-zero"}}, nil
	}
	return oapi.PinScaleToZero204Response{}, nil
}

func (s *ApiService) UnpinScaleToZero(ctx context.Context, _ oapi.UnpinScaleToZeroRequestObject) (oapi.UnpinScaleToZeroResponseObject, error) {
	if err := s.stz.Unpin(ctx); err != nil {
		logger.FromContext(ctx).Error("failed to unpin scale-to-zero", "err", err)
		return oapi.UnpinScaleToZero500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to unpin scale-to-zero"}}, nil
	}
	return oapi.UnpinScaleToZero204Response{}, nil
}
