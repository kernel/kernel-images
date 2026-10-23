package api

import (
	"context"
	"testing"

	oapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/kernel/kernel-images/server/lib/scaletozero"
	"github.com/stretchr/testify/require"
)

func newWebAuthnSvc(t *testing.T, metroURL string) *ApiService {
	t.Helper()
	ts, es := newTelemetrySession(t)
	svc, err := New(newMockRecordManager(), newMockFactory(), newTestUpstreamManager(), scaletozero.NewNoopController(), newMockNekoClient(t), ts, es, 0, nil, nil, testEgressPolicy(t), testWebAuthnWithMetro(t, metroURL), ProcessOutputConfig{})
	require.NoError(t, err)
	return svc
}

func TestPutWebAuthnHook(t *testing.T) {
	svc := newWebAuthnSvc(t, "https://metro.example")

	for _, want := range []bool{true, true, false, false} {
		resp, err := svc.PutWebAuthnHook(context.Background(), oapi.PutWebAuthnHookRequestObject{
			Body: &oapi.WebAuthnHook{Enabled: want},
		})
		require.NoError(t, err)
		require.Equal(t, oapi.PutWebAuthnHook200JSONResponse{Enabled: want}, resp)
	}
}

// Until the metro API URL reaches every VM, the endpoint refuses to turn the
// hook on rather than arming a hook nothing can answer. Turning it off still
// succeeds.
func TestPutWebAuthnHookWithoutMetroAPI(t *testing.T) {
	svc := newWebAuthnSvc(t, "")

	resp, err := svc.PutWebAuthnHook(context.Background(), oapi.PutWebAuthnHookRequestObject{
		Body: &oapi.WebAuthnHook{Enabled: true},
	})
	require.NoError(t, err)
	require.Equal(t, oapi.PutWebAuthnHook400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{
		Message: "WebAuthn hook is not configured: KERNEL_METRO_API_BASE_URL is not set",
	}}, resp)

	resp, err = svc.PutWebAuthnHook(context.Background(), oapi.PutWebAuthnHookRequestObject{
		Body: &oapi.WebAuthnHook{Enabled: false},
	})
	require.NoError(t, err)
	require.Equal(t, oapi.PutWebAuthnHook200JSONResponse{Enabled: false}, resp)
}

func TestPutWebAuthnHookRejectsMissingBody(t *testing.T) {
	svc := newWebAuthnSvc(t, "https://metro.example")

	resp, err := svc.PutWebAuthnHook(context.Background(), oapi.PutWebAuthnHookRequestObject{})
	require.NoError(t, err)
	require.IsType(t, oapi.PutWebAuthnHook400JSONResponse{}, resp)
}
