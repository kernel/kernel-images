package api

import (
	"context"
	"testing"
	"time"

	oapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/kernel/kernel-images/server/lib/scaletozero"
	"github.com/stretchr/testify/require"
)

func TestScaleToZeroLeaseHandlers(t *testing.T) {
	t.Parallel()

	svc := &ApiService{stz: scaletozero.NewNoopController()}
	acquired, err := svc.AcquireScaleToZeroLease(t.Context(), oapi.AcquireScaleToZeroLeaseRequestObject{
		LeaseId: "metro-lease",
		Params:  oapi.AcquireScaleToZeroLeaseParams{TtlSeconds: 30},
	})
	require.NoError(t, err)
	require.IsType(t, oapi.AcquireScaleToZeroLease204Response{}, acquired)

	released, err := svc.ReleaseScaleToZeroLease(t.Context(), oapi.ReleaseScaleToZeroLeaseRequestObject{LeaseId: "metro-lease"})
	require.NoError(t, err)
	require.IsType(t, oapi.ReleaseScaleToZeroLease204Response{}, released)
}

func TestScaleToZeroLeaseHandlerReturnsConflictAtLimit(t *testing.T) {
	t.Parallel()

	svc := &ApiService{stz: &leaseLimitController{NoopController: scaletozero.NewNoopController()}}
	response, err := svc.AcquireScaleToZeroLease(t.Context(), oapi.AcquireScaleToZeroLeaseRequestObject{
		LeaseId: "metro-lease",
		Params:  oapi.AcquireScaleToZeroLeaseParams{TtlSeconds: 30},
	})
	require.NoError(t, err)
	require.IsType(t, oapi.AcquireScaleToZeroLease409JSONResponse{}, response)
}

func TestScaleToZeroLeaseHandlersRejectInvalidInput(t *testing.T) {
	t.Parallel()

	svc := &ApiService{stz: scaletozero.NewNoopController()}
	acquired, err := svc.AcquireScaleToZeroLease(t.Context(), oapi.AcquireScaleToZeroLeaseRequestObject{
		LeaseId: "invalid/lease",
		Params:  oapi.AcquireScaleToZeroLeaseParams{TtlSeconds: 301},
	})
	require.NoError(t, err)
	require.IsType(t, oapi.AcquireScaleToZeroLease400JSONResponse{}, acquired)

	released, err := svc.ReleaseScaleToZeroLease(t.Context(), oapi.ReleaseScaleToZeroLeaseRequestObject{LeaseId: "invalid/lease"})
	require.NoError(t, err)
	require.IsType(t, oapi.ReleaseScaleToZeroLease400JSONResponse{}, released)
}

type leaseLimitController struct {
	*scaletozero.NoopController
}

func (*leaseLimitController) AcquireLease(context.Context, string, time.Duration) error {
	return scaletozero.ErrLeaseLimit
}
