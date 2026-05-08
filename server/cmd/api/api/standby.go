package api

import (
	"context"

	"github.com/kernel/kernel-images/server/lib/logger"
	oapi "github.com/kernel/kernel-images/server/lib/oapi"
)

func (s *ApiService) DisableStandby(ctx context.Context, _ oapi.DisableStandbyRequestObject) (oapi.DisableStandbyResponseObject, error) {
	if err := s.stz.DisablePin(ctx); err != nil {
		logger.FromContext(ctx).Error("failed to pin scale-to-zero disabled", "err", err)
		return oapi.DisableStandby500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to disable standby"}}, nil
	}
	return oapi.DisableStandby204Response{}, nil
}

func (s *ApiService) EnableStandby(ctx context.Context, _ oapi.EnableStandbyRequestObject) (oapi.EnableStandbyResponseObject, error) {
	if err := s.stz.EnablePin(ctx); err != nil {
		logger.FromContext(ctx).Error("failed to release scale-to-zero pin", "err", err)
		return oapi.EnableStandby500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to enable standby"}}, nil
	}
	return oapi.EnableStandby204Response{}, nil
}
