package api

import (
	"bytes"
	"errors"
	"io"
	"mime/multipart"
	"strings"
	"testing"

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

func TestChromiumConfigureActionableFlags(t *testing.T) {
	emptyFlags := `{"flags":[]}`
	realFlags := `{"flags":["--kiosk"]}`

	st := &chromiumConfigureState{chromiumFlagsJSON: &emptyFlags}
	require.Equal(t, 0, cfgActionables(st))
	require.False(t, chromiumNeedsStopCycle(st))

	st = &chromiumConfigureState{chromiumFlagsJSON: &realFlags}
	require.Equal(t, 1, cfgActionables(st))
	require.True(t, chromiumNeedsStopCycle(st))
}

func TestChromiumConfigureActionablePolicies(t *testing.T) {
	emptyPolicies := `{}`
	realPolicies := `{"QuicAllowed":false}`

	st := &chromiumConfigureState{chromePoliciesJSON: &emptyPolicies}
	require.Equal(t, 0, cfgActionables(st))
	require.False(t, chromiumNeedsStopCycle(st))

	st = &chromiumConfigureState{chromePoliciesJSON: &realPolicies}
	require.Equal(t, 1, cfgActionables(st))
	require.True(t, chromiumNeedsStopCycle(st))
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
