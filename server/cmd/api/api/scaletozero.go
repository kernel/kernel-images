package api

import (
	"context"
	"errors"
	"regexp"
	"time"

	"github.com/kernel/kernel-images/server/lib/logger"
	oapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/kernel/kernel-images/server/lib/scaletozero"
)

var scaleToZeroLeaseIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

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

func (s *ApiService) AcquireScaleToZeroLease(ctx context.Context, req oapi.AcquireScaleToZeroLeaseRequestObject) (oapi.AcquireScaleToZeroLeaseResponseObject, error) {
	if !scaleToZeroLeaseIDPattern.MatchString(req.LeaseId) || req.Params.TtlSeconds < 1 || req.Params.TtlSeconds > 300 {
		return oapi.AcquireScaleToZeroLease400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "invalid scale-to-zero lease"}}, nil
	}
	ttl := time.Duration(req.Params.TtlSeconds) * time.Second
	if err := s.stz.AcquireLease(ctx, req.LeaseId, ttl); err != nil {
		if errors.Is(err, scaletozero.ErrLeaseLimit) {
			return oapi.AcquireScaleToZeroLease409JSONResponse{ConflictErrorJSONResponse: oapi.ConflictErrorJSONResponse{Message: scaletozero.ErrLeaseLimit.Error()}}, nil
		}
		logger.FromContext(ctx).Error("failed to acquire scale-to-zero lease", "err", err, "lease_id", req.LeaseId)
		return oapi.AcquireScaleToZeroLease500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to acquire scale-to-zero lease"}}, nil
	}
	return oapi.AcquireScaleToZeroLease204Response{}, nil
}

func (s *ApiService) ReleaseScaleToZeroLease(ctx context.Context, req oapi.ReleaseScaleToZeroLeaseRequestObject) (oapi.ReleaseScaleToZeroLeaseResponseObject, error) {
	if !scaleToZeroLeaseIDPattern.MatchString(req.LeaseId) {
		return oapi.ReleaseScaleToZeroLease400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "invalid scale-to-zero lease"}}, nil
	}
	if err := s.stz.ReleaseLease(ctx, req.LeaseId); err != nil {
		logger.FromContext(ctx).Error("failed to release scale-to-zero lease", "err", err, "lease_id", req.LeaseId)
		return oapi.ReleaseScaleToZeroLease500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to release scale-to-zero lease"}}, nil
	}
	return oapi.ReleaseScaleToZeroLease204Response{}, nil
}
