package api

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/onkernel/kernel-images/server/lib/chromiumflags"
	oapi "github.com/onkernel/kernel-images/server/lib/oapi"
	"github.com/onkernel/kernel-images/server/lib/recorder"
	"github.com/onkernel/kernel-images/server/lib/scaletozero"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newPACTestService(t *testing.T) *ApiService {
	t.Helper()

	mgr := recorder.NewFFmpegManager()
	svc, err := New(mgr, newMockFactory(), newTestUpstreamManager(), scaletozero.NewNoopController(), newMockNekoClient(t))
	require.NoError(t, err)

	base := t.TempDir()
	svc.chromiumFlagsPath = filepath.Join(base, "chromium", "flags")
	svc.chromiumPACPath = filepath.Join(base, "chromium", "proxy.pac")
	svc.chromiumPACStatePath = filepath.Join(base, "chromium", "pac-state.json")
	return svc
}

func TestApiService_PutChromiumProxyPac_EnableDisableAndGet(t *testing.T) {
	ctx := context.Background()
	svc := newPACTestService(t)

	restart := false
	content := "function FindProxyForURL(url, host) { return 'DIRECT'; }\n"
	enableReq := oapi.PutChromiumProxyPacRequestObject{
		Body: &oapi.PutChromiumProxyPacJSONRequestBody{
			Enabled:         true,
			Content:         &content,
			RestartChromium: &restart,
		},
	}

	enableResp, err := svc.PutChromiumProxyPac(ctx, enableReq)
	require.NoError(t, err)
	enableOK, ok := enableResp.(oapi.PutChromiumProxyPac200JSONResponse)
	require.True(t, ok, "unexpected response type: %T", enableResp)
	assert.True(t, enableOK.Config.Enabled)
	require.NotNil(t, enableOK.Config.Content)
	assert.Equal(t, content, *enableOK.Config.Content)
	require.NotNil(t, enableOK.Config.PacUrl)
	assert.Equal(t, svc.chromiumPACServeURL(), *enableOK.Config.PacUrl)
	assert.False(t, enableOK.RestartRequested)
	assert.False(t, enableOK.ChromiumRestarted)

	tokens, err := chromiumflags.ReadOptionalFlagFile(svc.chromiumFlagsPath)
	require.NoError(t, err)
	for _, token := range tokens {
		assert.NotContains(t, token, chromiumPACFlagPrefix)
	}

	disableReq := oapi.PutChromiumProxyPacRequestObject{
		Body: &oapi.PutChromiumProxyPacJSONRequestBody{
			Enabled:         false,
			RestartChromium: &restart,
		},
	}
	disableResp, err := svc.PutChromiumProxyPac(ctx, disableReq)
	require.NoError(t, err)
	disableOK, ok := disableResp.(oapi.PutChromiumProxyPac200JSONResponse)
	require.True(t, ok, "unexpected response type: %T", disableResp)
	assert.False(t, disableOK.Config.Enabled)
	assert.Nil(t, disableOK.Config.PacUrl)

	tokens, err = chromiumflags.ReadOptionalFlagFile(svc.chromiumFlagsPath)
	require.NoError(t, err)
	for _, token := range tokens {
		assert.NotContains(t, token, chromiumPACFlagPrefix)
	}

	getResp, err := svc.GetChromiumProxyPac(ctx, oapi.GetChromiumProxyPacRequestObject{})
	require.NoError(t, err)
	getOK, ok := getResp.(oapi.GetChromiumProxyPac200JSONResponse)
	require.True(t, ok, "unexpected response type: %T", getResp)
	assert.False(t, getOK.Enabled)
	require.NotNil(t, getOK.Content)
	assert.Equal(t, content, *getOK.Content)
	assert.False(t, getOK.RestartRequiredForImmediateApply)
	assert.True(t, getOK.DynamicUpdateWithoutRestartSupported)
}

func TestApiService_PutChromiumProxyPac_EnableWithoutContent(t *testing.T) {
	ctx := context.Background()
	svc := newPACTestService(t)

	restart := false
	resp, err := svc.PutChromiumProxyPac(ctx, oapi.PutChromiumProxyPacRequestObject{
		Body: &oapi.PutChromiumProxyPacJSONRequestBody{
			Enabled:         true,
			RestartChromium: &restart,
		},
	})
	require.NoError(t, err)
	require.IsType(t, oapi.PutChromiumProxyPac400JSONResponse{}, resp)
}

func TestApiService_GetChromiumProxyPacScript(t *testing.T) {
	ctx := context.Background()
	svc := newPACTestService(t)

	// No PAC content configured yet.
	notFoundResp, err := svc.GetChromiumProxyPacScript(ctx, oapi.GetChromiumProxyPacScriptRequestObject{})
	require.NoError(t, err)
	require.IsType(t, oapi.GetChromiumProxyPacScript404JSONResponse{}, notFoundResp)

	restart := false
	content := "function FindProxyForURL(url, host) { return 'DIRECT'; }\n"
	_, err = svc.PutChromiumProxyPac(ctx, oapi.PutChromiumProxyPacRequestObject{
		Body: &oapi.PutChromiumProxyPacJSONRequestBody{
			Enabled:         true,
			Content:         &content,
			RestartChromium: &restart,
		},
	})
	require.NoError(t, err)

	resp, err := svc.GetChromiumProxyPacScript(ctx, oapi.GetChromiumProxyPacScriptRequestObject{})
	require.NoError(t, err)
	textResp, ok := resp.(oapi.GetChromiumProxyPacScript200TextResponse)
	require.True(t, ok, "unexpected response type: %T", resp)
	assert.Equal(t, content, string(textResp))
}

func TestApiService_PutChromiumProxyPac_DefaultRestartBehavior(t *testing.T) {
	ctx := context.Background()
	svc := newPACTestService(t)

	content := "function FindProxyForURL(url, host) { return 'DIRECT'; }\n"
	resp, err := svc.PutChromiumProxyPac(ctx, oapi.PutChromiumProxyPacRequestObject{
		Body: &oapi.PutChromiumProxyPacJSONRequestBody{
			Enabled: true,
			Content: &content,
		},
	})
	require.NoError(t, err)
	okResp, ok := resp.(oapi.PutChromiumProxyPac200JSONResponse)
	require.True(t, ok, "unexpected response type: %T", resp)
	assert.False(t, okResp.RestartRequested)
	assert.False(t, okResp.ChromiumRestarted)
}

func TestUpsertPACFlag(t *testing.T) {
	pacURL := "http://127.0.0.1:10001/chromium/proxy/pac/script"
	got := upsertPACFlag(
		[]string{"--foo", "--proxy-pac-url=http://example.test/old.pac", "--foo", "  ", "--bar=1"},
		&pacURL,
	)
	assert.Equal(t, []string{"--foo", "--bar=1", "--proxy-pac-url=http://127.0.0.1:10001/chromium/proxy/pac/script"}, got)

	got = upsertPACFlag(got, nil)
	assert.Equal(t, []string{"--foo", "--bar=1"}, got)
}
