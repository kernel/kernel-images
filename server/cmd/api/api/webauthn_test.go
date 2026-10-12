package api

import (
	"context"
	"testing"

	oapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/require"
)

func TestPutWebAuthnHook(t *testing.T) {
	svc, err := newSvc(t, newMockRecordManager())
	require.NoError(t, err)

	for _, want := range []bool{true, true, false, false} {
		resp, err := svc.PutWebAuthnHook(context.Background(), oapi.PutWebAuthnHookRequestObject{
			Body: &oapi.WebAuthnHook{Enabled: want},
		})
		require.NoError(t, err)
		require.Equal(t, oapi.PutWebAuthnHook200JSONResponse{Enabled: want}, resp)
	}
}

func TestPutWebAuthnHookRejectsMissingBody(t *testing.T) {
	svc, err := newSvc(t, newMockRecordManager())
	require.NoError(t, err)

	resp, err := svc.PutWebAuthnHook(context.Background(), oapi.PutWebAuthnHookRequestObject{})
	require.NoError(t, err)
	require.IsType(t, oapi.PutWebAuthnHook400JSONResponse{}, resp)
}
