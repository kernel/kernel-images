package api

import (
	"bytes"
	"errors"
	"io"
	"mime/multipart"
	"os"
	"path/filepath"
	"strings"
	"testing"

	oapi "github.com/kernel/kernel-images/server/lib/oapi"
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

func TestChromiumConfigureModeFor(t *testing.T) {
	stringPtr := func(value string) *string { return &value }

	tests := []struct {
		name     string
		state    chromiumConfigureState
		strategy oapi.ChromiumConfigureParamsExtensionLoadStrategy
		want     chromiumConfigureMode
	}{
		{name: "no restart fields", strategy: oapi.Restart, want: chromiumConfigureModeLive},
		{name: "display only", state: chromiumConfigureState{displayJSON: stringPtr(`{"width":1280}`)}, strategy: oapi.Restart, want: chromiumConfigureModeLive},
		{name: "start URL only", state: chromiumConfigureState{startURLRaw: stringPtr("https://example.com")}, strategy: oapi.Restart, want: chromiumConfigureModeLive},
		{name: "empty policies", state: chromiumConfigureState{chromePoliciesJSON: stringPtr(`{}`)}, strategy: oapi.Restart, want: chromiumConfigureModeLive},
		{name: "nonempty policies", state: chromiumConfigureState{chromePoliciesJSON: stringPtr(`{"QuicAllowed":false}`)}, strategy: oapi.PreferCdp, want: chromiumConfigureModeRestart},
		{name: "invalid policies", state: chromiumConfigureState{chromePoliciesJSON: stringPtr(`{bad-json`)}, strategy: oapi.PreferCdp, want: chromiumConfigureModeRestart},
		{name: "empty flags", state: chromiumConfigureState{chromiumFlagsJSON: stringPtr(`{"flags":[]}`)}, strategy: oapi.Restart, want: chromiumConfigureModeLive},
		{name: "nonempty flags", state: chromiumConfigureState{chromiumFlagsJSON: stringPtr(`{"flags":["--kiosk"]}`)}, strategy: oapi.PreferCdp, want: chromiumConfigureModeRestart},
		{name: "invalid flags", state: chromiumConfigureState{chromiumFlagsJSON: stringPtr(`{bad-json`)}, strategy: oapi.PreferCdp, want: chromiumConfigureModeRestart},
		{name: "profile", state: chromiumConfigureState{hasProfile: true}, strategy: oapi.PreferCdp, want: chromiumConfigureModeRestart},
		{name: "extensions default restart", state: chromiumConfigureState{extItems: []extensionZipItem{{name: "test"}}}, strategy: oapi.Restart, want: chromiumConfigureModeRestart},
		{name: "extensions prefer CDP", state: chromiumConfigureState{extItems: []extensionZipItem{{name: "test"}}}, strategy: oapi.PreferCdp, want: chromiumConfigureModePreferCDPExtensions},
		{name: "display and extensions prefer CDP", state: chromiumConfigureState{displayJSON: stringPtr(`{"width":1280}`), extItems: []extensionZipItem{{name: "test"}}}, strategy: oapi.PreferCdp, want: chromiumConfigureModePreferCDPExtensions},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, chromiumConfigureModeFor(&tt.state, tt.strategy))
		})
	}
}

func TestChromiumConfigureExtensionLoadStrategy(t *testing.T) {
	strategy, msg := chromiumConfigureExtensionLoadStrategy(oapi.ChromiumConfigureParams{})
	require.Empty(t, msg)
	require.Equal(t, oapi.Restart, strategy)

	preferCDP := oapi.PreferCdp
	strategy, msg = chromiumConfigureExtensionLoadStrategy(oapi.ChromiumConfigureParams{ExtensionLoadStrategy: &preferCDP})
	require.Empty(t, msg)
	require.Equal(t, oapi.PreferCdp, strategy)

	invalid := oapi.ChromiumConfigureParamsExtensionLoadStrategy("invalid")
	_, msg = chromiumConfigureExtensionLoadStrategy(oapi.ChromiumConfigureParams{ExtensionLoadStrategy: &invalid})
	require.Equal(t, "extension_load_strategy must be restart or prefer_cdp", msg)
}

func TestChromiumConfigureGeneratedClientQuery(t *testing.T) {
	preferCDP := oapi.PreferCdp
	req, err := oapi.NewChromiumConfigureRequestWithBody(
		"http://example.test",
		&oapi.ChromiumConfigureParams{ExtensionLoadStrategy: &preferCDP},
		"multipart/form-data; boundary=test",
		strings.NewReader("--test--"),
	)
	require.NoError(t, err)
	require.Equal(t, "prefer_cdp", req.URL.Query().Get("extension_load_strategy"))

	req, err = oapi.NewChromiumConfigureRequestWithBody(
		"http://example.test",
		nil,
		"multipart/form-data; boundary=test",
		strings.NewReader("--test--"),
	)
	require.NoError(t, err)
	require.Empty(t, req.URL.RawQuery)
}

func TestChromiumConfigureActionables(t *testing.T) {
	emptyFlags := `{"flags":[]}`
	realFlags := `{"flags":["--kiosk"]}`
	emptyPolicies := `{}`
	realPolicies := `{"QuicAllowed":false}`

	tests := []struct {
		name  string
		state chromiumConfigureState
		want  int
	}{
		{name: "empty flags", state: chromiumConfigureState{chromiumFlagsJSON: &emptyFlags}},
		{name: "nonempty flags", state: chromiumConfigureState{chromiumFlagsJSON: &realFlags}, want: 1},
		{name: "empty policies", state: chromiumConfigureState{chromePoliciesJSON: &emptyPolicies}},
		{name: "nonempty policies", state: chromiumConfigureState{chromePoliciesJSON: &realPolicies}, want: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, cfgActionables(&tt.state))
		})
	}
}

func TestChromiumStartURLSpec(t *testing.T) {
	bareHost := "roblox.com"
	out, errs := chromiumStartURLSpec(&bareHost)
	require.Empty(t, errs)
	require.True(t, out.needsNav)
	require.Equal(t, "https://roblox.com", out.url)

	plain := "https://example.com/"
	out, errs = chromiumStartURLSpec(&plain)
	require.Empty(t, errs)
	require.True(t, out.needsNav)
	require.Equal(t, plain, out.url)

	fileURL := "file:///etc/passwd"
	out, errs = chromiumStartURLSpec(&fileURL)
	require.Empty(t, errs)
	require.Equal(t, fileURL, out.url)

	longURL := strings.Repeat("a", maxStartURLLen+1)
	_, errs = chromiumStartURLSpec(&longURL)
	require.NotEmpty(t, errs)
}

func TestStripProfileSessionRestore(t *testing.T) {
	prepared := t.TempDir()
	sessions := filepath.Join(prepared, "Default", "Sessions")
	require.NoError(t, os.MkdirAll(sessions, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(sessions, "Session_123"), []byte("tabs"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(prepared, "Default", "Preferences"), []byte("{}"), 0o644))

	require.NoError(t, stripProfileSessionRestore(prepared))

	_, err := os.Stat(sessions)
	require.True(t, os.IsNotExist(err))
	_, err = os.Stat(filepath.Join(prepared, "Default", "Preferences"))
	require.NoError(t, err)

	// Absent Sessions directory is a no-op, not an error.
	require.NoError(t, stripProfileSessionRestore(prepared))
}

func TestChromiumValidateFlags(t *testing.T) {
	valid := `{"flags":["--kiosk"]}`
	plan, err := chromiumValidateFlags(&valid)
	require.NoError(t, err)
	require.Equal(t, []string{"--kiosk"}, plan.flags)

	empty := `{"flags":[]}`
	plan, err = chromiumValidateFlags(&empty)
	require.NoError(t, err)
	require.Nil(t, plan)

	cases := []string{
		`{bad-json`,
		`{"flags":[""]}`,
		`{"flags":["kiosk"]}`,
	}
	for _, tc := range cases {
		_, err := chromiumValidateFlags(&tc)
		require.Error(t, err, "case %s", tc)
		var bad cfgBadRequestError
		require.ErrorAs(t, err, &bad)
	}
}

func TestChromiumValidatePoliciesBadRequest(t *testing.T) {
	blocked := `{"ExtensionSettings":{}}`
	_, err := chromiumValidatePolicies(&blocked)
	require.Error(t, err)
	var bad cfgBadRequestError
	require.ErrorAs(t, err, &bad)
}

func TestChromiumParseDisplayPartsValidation(t *testing.T) {
	badJSON := `{bad-json`
	_, msg := chromiumParseDisplayParts(&badJSON)
	require.Equal(t, "invalid display JSON", msg)

	empty := `{}`
	_, msg = chromiumParseDisplayParts(&empty)
	require.Equal(t, "display payload empty", msg)
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
	err := chromiumCfgParseMultipart(br, st)
	defer st.cleanup()
	require.NoError(t, err)

	require.True(t, policiesContentNonEmpty(st.chromePoliciesJSON))
	require.Equal(t, 2, st.stripComponents)
	require.NotNil(t, st.startURLRaw)
	require.Equal(t, "https://kernel.example/route", strings.TrimSpace(*st.startURLRaw))
}

func TestChromiumCfgParseMultipartValidation(t *testing.T) {
	cases := []struct {
		name  string
		build func(*testing.T, *multipart.Writer)
		want  string
	}{
		{
			name: "invalid strip_components",
			build: func(t *testing.T, w *multipart.Writer) {
				t.Helper()
				require.NoError(t, w.WriteField("strip_components", "-1"))
			},
			want: "strip_components must be a non-negative integer",
		},
		{
			name: "duplicate scalar",
			build: func(t *testing.T, w *multipart.Writer) {
				t.Helper()
				require.NoError(t, w.WriteField("start_url", "https://a.example"))
				require.NoError(t, w.WriteField("start_url", "https://b.example"))
			},
			want: "duplicate start_url field",
		},
		{
			name: "incomplete extension pair",
			build: func(t *testing.T, w *multipart.Writer) {
				t.Helper()
				require.NoError(t, w.WriteField("extensions.name", "missingzip"))
			},
			want: "each extension pair needs extensions.zip_file plus extensions.name",
		},
		{
			name: "duplicate extension zip",
			build: func(t *testing.T, w *multipart.Writer) {
				t.Helper()
				part, err := w.CreateFormFile("extensions.zip_file", "one.zip")
				require.NoError(t, err)
				_, err = io.WriteString(part, "first")
				require.NoError(t, err)
				part, err = w.CreateFormFile("extensions.zip_file", "two.zip")
				require.NoError(t, err)
				_, err = io.WriteString(part, "second")
				require.NoError(t, err)
			},
			want: "duplicate extensions.zip_file pair",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf := bytes.NewBuffer(nil)
			w := multipart.NewWriter(buf)
			tc.build(t, w)
			require.NoError(t, w.Close())

			st := &chromiumConfigureState{}
			err := chromiumCfgParseMultipart(multipart.NewReader(buf, w.Boundary()), st)
			defer st.cleanup()
			require.EqualError(t, err, tc.want)
			var parseErr chromiumCfgParseError
			require.True(t, errors.As(err, &parseErr))
			require.False(t, parseErr.internal)
		})
	}
}

func TestChromiumCfgParseMultipartMultipleExtensionPairs(t *testing.T) {
	buf := bytes.NewBuffer(nil)
	w := multipart.NewWriter(buf)

	part, err := w.CreateFormFile("extensions.zip_file", "one.zip")
	require.NoError(t, err)
	_, err = io.WriteString(part, "not validated by parser")
	require.NoError(t, err)
	require.NoError(t, w.WriteField("extensions.name", "one"))

	require.NoError(t, w.WriteField("extensions.name", "two"))
	part, err = w.CreateFormFile("extensions.zip_file", "two.zip")
	require.NoError(t, err)
	_, err = io.WriteString(part, "not validated by parser")
	require.NoError(t, err)
	require.NoError(t, w.Close())

	st := &chromiumConfigureState{}
	err = chromiumCfgParseMultipart(multipart.NewReader(buf, w.Boundary()), st)
	defer st.cleanup()
	require.NoError(t, err)
	require.Len(t, st.extItems, 2)
	require.Equal(t, "one", st.extItems[0].name)
	require.Equal(t, "two", st.extItems[1].name)
}
