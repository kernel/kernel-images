package api

import (
	"bytes"
	"mime/multipart"
	"strings"
	"testing"
	"time"

	"github.com/kernel/kernel-images/server/lib/cdpclient"
	"github.com/stretchr/testify/require"
)

func TestFlagsContentNonEmpty(t *testing.T) {
	emptyArr := `{}`
	fl := `{"flags":[]}`
	real := `{"flags":["--kiosk"]}`
	require.False(t, flagsContentNonEmpty(&emptyArr))
	require.False(t, flagsContentNonEmpty(&fl))
	require.True(t, flagsContentNonEmpty(&real))
}

func TestPoliciesContentNonEmpty(t *testing.T) {
	emptyObj := `{}`
	real := `{"DefaultCookiesSetting": 1}`
	require.False(t, policiesContentNonEmpty(&emptyObj))
	require.True(t, policiesContentNonEmpty(&real))
}

func TestChromiumStartURLSpec_plainAndJSON(t *testing.T) {
	plain := "https://example.com/"
	out, errs := chromiumStartURLSpec(&plain)
	require.Empty(t, errs)
	require.True(t, out.needsNav)
	require.Equal(t, plain, out.url)
	require.Equal(t, 45*time.Second, out.timeout)
	require.Equal(t, cdpclient.NavigateWaitLoad, out.wait)

	raw := `{"url":"https://a.test/x","wait_until":"domcontentloaded","timeout_sec":12}`
	out, errs = chromiumStartURLSpec(&raw)
	require.Empty(t, errs)
	require.True(t, out.needsNav)
	require.Equal(t, "https://a.test/x", out.url)
	require.Equal(t, 12*time.Second, out.timeout)
	require.Equal(t, cdpclient.NavigateWaitDOMContentLoaded, out.wait)

	badScheme := "file:///etc/passwd"
	_, errs = chromiumStartURLSpec(&badScheme)
	require.NotEmpty(t, errs)

	badWait := `{"url":"https://x.example","wait_until":"networkidle"}`
	_, errs = chromiumStartURLSpec(&badWait)
	require.NotEmpty(t, errs)
}

func TestChromiumCfgParseMultipart(t *testing.T) {
	buf := bytes.NewBuffer(nil)
	w := multipart.NewWriter(buf)

	require.NoError(t, w.WriteField("chrome_policies", `{"HttpsUpgradesEnabled":false}`))
	require.NoError(t, w.WriteField("strip_components", "2"))
	require.NoError(t, w.WriteField("start_url", "https://kernel.example/route"))

	require.NoError(t, w.Close())

	br := multipart.NewReader(buf, w.Boundary())
	st := &chromiumConfigureState{}
	msg := chromiumCfgParseMultipart(br, st)
	defer st.cleanup()
	require.Empty(t, msg)

	require.True(t, policiesContentNonEmpty(st.chromePoliciesJSON))
	require.Equal(t, 2, st.stripComponents)
	require.NotNil(t, st.startURLRaw)
	require.Equal(t, "https://kernel.example/route", strings.TrimSpace(*st.startURLRaw))
}
