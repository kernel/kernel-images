package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/require"
)

func setBrowserReplEnv(t *testing.T, svc *ApiService, env map[string]string) oapi.SetBrowserReplEnv200JSONResponse {
	t.Helper()
	resp, err := svc.SetBrowserReplEnv(context.Background(), oapi.SetBrowserReplEnvRequestObject{
		Body: &oapi.SetBrowserReplEnvJSONRequestBody{Env: env},
	})
	require.NoError(t, err)
	typed, ok := resp.(oapi.SetBrowserReplEnv200JSONResponse)
	require.True(t, ok, "expected 200 response, got %T", resp)
	return typed
}

func TestBrowserReplEnvValidation(t *testing.T) {
	svc := newBrowserReplSvc(t)

	got, err := svc.GetBrowserReplEnv(context.Background(), oapi.GetBrowserReplEnvRequestObject{})
	require.NoError(t, err)
	require.NotNil(t, got.(oapi.GetBrowserReplEnv200JSONResponse).Names, "names must serialize as [] rather than null")

	tooMany := make(map[string]string)
	for i := range maxBrowserReplEnvVars + 1 {
		tooMany["VAR_"+strings.Repeat("A", i+1)] = "x"
	}
	for name, env := range map[string]map[string]string{
		"nil":             nil,
		"leading digit":   {"1ABC": "x"},
		"dash":            {"MY-KEY": "x"},
		"long name":       {strings.Repeat("A", maxBrowserReplEnvNameBytes+1): "x"},
		"reserved":        {"NODE_OPTIONS": "--inspect"},
		"reserved prefix": {"BROWSER_REPL_SOCKET": "/tmp/other.sock"},
		"NUL":             {"KEY": "a\x00b"},
		"long value":      {"KEY": strings.Repeat("x", maxBrowserReplEnvValueBytes+1)},
		"too many":        tooMany,
	} {
		resp, err := svc.SetBrowserReplEnv(context.Background(), oapi.SetBrowserReplEnvRequestObject{
			Body: &oapi.SetBrowserReplEnvJSONRequestBody{Env: env},
		})
		require.NoError(t, err, name)
		require.IsType(t, oapi.SetBrowserReplEnv400JSONResponse{}, resp, name)
	}
	require.Empty(t, svc.browserRepl.envNames())
	require.Nil(t, svc.browserRepl.child, "validation failures must not start a REPL")
}

func TestBrowserReplEnvAppliesLiveAndToNewREPLs(t *testing.T) {
	const inherited = "KERNEL_IMAGES_TEST_INHERITED"
	t.Setenv(inherited, "from-api")
	svc := newBrowserReplSvc(t)

	// Variables set before the REPL starts are present in its first process.
	set := setBrowserReplEnv(t, svc, map[string]string{"FIRST": "1"})
	require.Equal(t, []string{"FIRST"}, set.Names)
	require.Nil(t, set.ReplTerminated)
	first := requireExec(t, svc, `var kept = "state"; repl.write(JSON.stringify(process.env.FIRST))`, "1")

	// A running REPL receives changes without losing state.
	set = setBrowserReplEnv(t, svc, map[string]string{"SECOND": "2", inherited: "override"})
	require.Equal(t, []string{inherited, "SECOND"}, set.Names)
	require.Nil(t, set.ReplTerminated)
	live := requireExec(t, svc, `repl.write(JSON.stringify([process.env.FIRST ?? null, process.env.SECOND, process.env.`+inherited+`, kept]))`,
		[]any{nil, "2", "override", "state"})
	require.Equal(t, first.ReplId, live.ReplId)

	// A fresh REPL starts with the stored variables.
	reset := true
	executeBrowserRepl(t, svc, &oapi.ExecuteBrowserReplJSONRequestBody{Reset: &reset})
	requireExec(t, svc, `repl.write(JSON.stringify([process.env.SECOND, process.env.`+inherited+`]))`, []any{"2", "override"})

	// Removing a variable restores the API process's own value.
	resp, err := svc.DeleteBrowserReplEnv(context.Background(), oapi.DeleteBrowserReplEnvRequestObject{})
	require.NoError(t, err)
	deleted := resp.(oapi.DeleteBrowserReplEnv200JSONResponse)
	require.Equal(t, []string{}, deleted.Names)
	requireExec(t, svc, `repl.write(JSON.stringify([process.env.SECOND ?? null, process.env.`+inherited+`]))`, []any{nil, "from-api"})
}

func TestBrowserReplClearEnvDropsVariables(t *testing.T) {
	svc := newBrowserReplSvc(t)
	setBrowserReplEnv(t, svc, map[string]string{"FORKED_KEY": "secret"})
	requireExec(t, svc, `var kept = 1; repl.write(JSON.stringify(process.env.FORKED_KEY))`, "secret")

	svc.ClearBrowserReplEnv()
	require.Empty(t, svc.browserRepl.envNames(), "stored variables are cleared immediately")
	require.Eventually(t, func() bool {
		resp := execCode(t, svc, `repl.write(JSON.stringify([process.env.FORKED_KEY ?? null, kept]))`)
		if !resp.Success || resp.Content == nil {
			return false
		}
		text, err := (*resp.Content)[len(*resp.Content)-1].AsBrowserReplTextContent()
		return err == nil && text.Text == `[null,1]`
	}, 5*time.Second, 50*time.Millisecond)
}

func TestBrowserReplModelsNamespace(t *testing.T) {
	// The models namespace resolves credentials from the REPL environment.
	t.Setenv("OPENROUTER_API_KEY", "")
	require.NoError(t, os.Unsetenv("OPENROUTER_API_KEY"))
	svc := newBrowserReplSvc(t)

	requireExec(t, svc, `
		const jev = await models.getModelOfType("classifier", "openrouter", "typesafe/jev-1.13");
		repl.write(JSON.stringify({
			frozen: Object.isFrozen(models),
			id: jev.id,
			listed: (await models.getModelsOfType("classifier")).some((m) => m.id === jev.id),
			available: (await models.getAvailableOfType("classifier", "openrouter")).length,
		}))
	`, map[string]any{"frozen": true, "id": "typesafe/jev-1.13", "listed": true, "available": float64(0)})

	setBrowserReplEnv(t, svc, map[string]string{"OPENROUTER_API_KEY": "sk-or-test"})
	requireExec(t, svc, `repl.write(JSON.stringify((await models.getAvailableOfType("classifier", "openrouter")).length > 0))`, true)

	requireExecError(t, svc, `await models.getModelsOfType("video")`, `Unknown model type "video"`)
	requireExecError(t, svc, `await models.classify({ provider: "openrouter", id: "nope" }, {})`, `Unknown classifier model "openrouter/nope"`)
	requireExec(t, svc, `repl.write(JSON.stringify(repl.help("models.classify").startsWith("models.classify(model, { state, questions })")))`, true)
}

func TestStrictBrowserReplEnvBody(t *testing.T) {
	handler := StrictBrowserReplBodyMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/repl/env", strings.NewReader(`{"env":{},"bogus":1}`)))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), `unknown field \"bogus\"`)

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/repl/env", strings.NewReader(`{"env":{"KEY":"value"}}`)))
	require.Equal(t, http.StatusOK, rec.Code)

	rec = httptest.NewRecorder()
	huge := `{"env":{"KEY":"` + strings.Repeat("x", maxBrowserReplEnvBodyBytes) + `"}}`
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/repl/env", strings.NewReader(huge)))
	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
}
