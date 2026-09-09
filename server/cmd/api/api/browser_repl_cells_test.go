package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/require"
)

func TestBrowserReplPersistentClosureIdentity(t *testing.T) {
	svc := newBrowserReplSvc(t)
	requireExec(t, svc, `let closureCount = 0; function incrementClosureCount() { return ++closureCount; }`, nil)
	requireExec(t, svc, `repl.write(JSON.stringify(incrementClosureCount()))`, float64(1))
	requireExec(t, svc, `repl.write(JSON.stringify(incrementClosureCount()))`, float64(2))
	requireExec(t, svc, `closureCount = 10; repl.write(JSON.stringify(closureCount))`, float64(10))
	requireExec(t, svc, `repl.write(JSON.stringify(incrementClosureCount()))`, float64(11))
	requireExec(t, svc, `setTimeout(() => { closureCount += 5; }, 1)`, nil)
	requireExec(t, svc, `await new Promise(resolve => setTimeout(resolve, 10)); repl.write(JSON.stringify(closureCount))`, float64(16))
}

func TestBrowserReplCanPersistPatchrightAndPlaywrightCoreImports(t *testing.T) {
	svc := newBrowserReplSvc(t)
	requireExec(t, svc, `var playwright = await import("patchright"); var playwrightReference = playwright; var vanillaPlaywright = await import("playwright-core")`, nil)
	requireExec(t, svc, `repl.write(JSON.stringify({ same: playwright === playwrightReference, patchrightConnect: typeof playwright.chromium.connectOverCDP, playwrightConnect: typeof vanillaPlaywright.chromium.connectOverCDP, endpoint: process.env.CDP_ENDPOINT }))`, map[string]interface{}{
		"same":              true,
		"patchrightConnect": "function",
		"playwrightConnect": "function",
		"endpoint":          "ws://127.0.0.1:9222",
	})
}

func TestBrowserReplFunctionDeclarationsUsePersistentAccessor(t *testing.T) {
	svc := newBrowserReplSvc(t)
	requireExec(t, svc, `function replFunctionValue() { return 1; } function replFunctionClosure() { return replFunctionValue(); } replFunctionValue = () => 3; repl.write(JSON.stringify(replFunctionClosure()))`, float64(3))
	requireExec(t, svc, `function replFunctionValue() { return 2; }`, nil)
	requireExec(t, svc, `repl.write(JSON.stringify(replFunctionClosure()))`, float64(2))
	requireExec(t, svc, `function replDuplicate() { return 1; } function replDuplicate() { return 2; } repl.write(JSON.stringify(replDuplicate()))`, float64(2))
	requireExec(t, svc, `function replFunctionNameProbe() {} repl.write(JSON.stringify(replFunctionNameProbe.name))`, "replFunctionNameProbe")
}

func TestBrowserReplBracelessVarPreservesControlFlow(t *testing.T) {
	svc := newBrowserReplSvc(t)
	for _, test := range []struct {
		code string
		want any
	}{
		{`if (false) var bracelessIfX = 1, bracelessIfY = 2; repl.write(JSON.stringify(typeof bracelessIfY))`, "undefined"},
		{`do var bracelessDoX = 1, bracelessDoY = 2; while (false); repl.write(JSON.stringify(bracelessDoX + bracelessDoY))`, float64(3)},
		{`for (const bracelessForElement of [1, 2]) var bracelessForX = bracelessForElement, bracelessForY = bracelessForElement * 2; repl.write(JSON.stringify(bracelessForY))`, float64(4)},
		{`var bracelessCommentX = 1 /* comma, stays */, bracelessCommentY = 2; repl.write(JSON.stringify(bracelessCommentX + bracelessCommentY))`, float64(3)},
		{`var replVarLog = []; if (false) var replIfNoInit; replVarLog.push('ran'); repl.write(JSON.stringify(replVarLog))`, []interface{}{"ran"}},
		{`do var replDoNoInit; while (false); repl.write(JSON.stringify(typeof replDoNoInit))`, "undefined"},
		{`for (let replForIndex = 0; replForIndex < 1; replForIndex++) var replForNoInit; repl.write(JSON.stringify(typeof replForNoInit))`, "undefined"},
		{`for (const replForOfIndex of [1]) var replForOfNoInit; repl.write(JSON.stringify(typeof replForOfNoInit))`, "undefined"},
	} {
		requireExec(t, svc, test.code, test.want)
	}
}

func TestBrowserReplStrayOutputBufferResetsAndPropagatesTruncation(t *testing.T) {
	svc := newBrowserReplSvc(t)
	const size = 256 * 1024
	requireExec(t, svc, fmt.Sprintf(`setTimeout(() => { repl.write("a".repeat(%d)); repl.write("dropped"); }, 10)`, size), nil)
	time.Sleep(50 * time.Millisecond)

	r := requireExec(t, svc, `void 0`, nil)
	require.True(t, *r.ContentTruncated)
	require.Len(t, *r.Content, 1)
	content, err := (*r.Content)[0].AsBrowserReplTextContent()
	require.NoError(t, err)
	require.Len(t, content.Text, size)

	requireExec(t, svc, fmt.Sprintf(`setTimeout(() => repl.write("b".repeat(%d)), 10)`, size), nil)
	time.Sleep(50 * time.Millisecond)
	r = requireExec(t, svc, `void 0`, nil)
	require.False(t, *r.ContentTruncated)
	require.Len(t, *r.Content, 1)
	content, err = (*r.Content)[0].AsBrowserReplTextContent()
	require.NoError(t, err)
	require.Len(t, content.Text, size)
}

func TestBrowserReplStrayItemLimitsPropagateTruncation(t *testing.T) {
	t.Run("text", func(t *testing.T) {
		svc := newBrowserReplSvc(t)
		requireExec(t, svc, `setTimeout(() => { for (let i = 0; i < 1500; i++) repl.write("s" + i); }, 10)`, nil)
		time.Sleep(50 * time.Millisecond)
		r := requireExec(t, svc, `void 0`, nil)
		require.True(t, *r.ContentTruncated)
		require.Len(t, *r.Content, 1000)
		first, err := (*r.Content)[0].AsBrowserReplTextContent()
		require.NoError(t, err)
		require.Equal(t, "s500", first.Text)
	})

	t.Run("images", func(t *testing.T) {
		svc := newBrowserReplSvc(t)
		requireExec(t, svc, `const png = Buffer.from([137, 80, 78, 71, 0, 0, 0, 0, 0]); setTimeout(async () => { for (let i = 0; i < 1500; i++) await repl.emitImage(png); }, 10)`, nil)
		time.Sleep(50 * time.Millisecond)
		r := requireExec(t, svc, `void 0`, nil)
		require.True(t, *r.ContentTruncated)
		require.Len(t, *r.Content, 1000)
		for _, item := range *r.Content {
			image, err := item.AsBrowserReplImageContent()
			require.NoError(t, err)
			require.Equal(t, oapi.BrowserReplImageContentType("image"), image.Type)
		}
	})
}

func TestBrowserReplActiveItemLimitTruncatesEmptyWrites(t *testing.T) {
	svc := newBrowserReplSvc(t)
	r := executeBrowserRepl(t, svc, &oapi.ExecuteBrowserReplJSONRequestBody{
		Code: `for (let i = 0; i < 20000; i++) repl.write("")`,
	})
	require.True(t, r.Success, "error: %v", r.Error)
	require.True(t, *r.ContentTruncated)
	require.Len(t, *r.Content, 10_000)

	largeError := executeBrowserRepl(t, svc, &oapi.ExecuteBrowserReplJSONRequestBody{
		Code: `throw new Error("x".repeat(1024 * 1024))`,
	})
	require.False(t, largeError.Success)
	require.LessOrEqual(t, len(*largeError.Error), 64*1024)

	stillAlive := executeBrowserRepl(t, svc, &oapi.ExecuteBrowserReplJSONRequestBody{Code: `repl.write("alive")`})
	require.True(t, stillAlive.Success, "bounded output must not destroy the REPL: %v", stillAlive.Error)
	require.Equal(t, r.ReplId, stillAlive.ReplId)
}

func TestBrowserReplNestedVarBindingsPersist(t *testing.T) {
	svc := newBrowserReplSvc(t)
	for _, test := range []struct {
		declaration string
		name        string
		value       float64
	}{
		{`for (var browserReplForVar = 0; browserReplForVar < 3; browserReplForVar++) {}`, "browserReplForVar", 3},
		{`for (var browserReplForOfVar of [1, 2, 3]) {}`, "browserReplForOfVar", 3},
		{`{ var browserReplBlockVar = 7; }`, "browserReplBlockVar", 7},
		{`if (true) var browserReplIfVar = 9;`, "browserReplIfVar", 9},
		{`switch (1) { case 1: var browserReplSwitchVar = 11; }`, "browserReplSwitchVar", 11},
		{`try { throw new Error("expected"); } catch (error) { var browserReplCatchVar = 13; }`, "browserReplCatchVar", 13},
	} {
		requireExec(t, svc, test.declaration, nil)
		requireExec(t, svc, `repl.write(JSON.stringify(`+test.name+`))`, test.value)
	}
}

func TestBrowserReplCatchParameterShadowsNestedVarInitializer(t *testing.T) {
	svc := newBrowserReplSvc(t)
	requireExec(t, svc, `var catchShadow = "outer"`, nil)
	requireExec(t, svc, `try { throw "caught" } catch (catchShadow) { var catchShadow = "inner"; repl.write(JSON.stringify(catchShadow)) }`, "inner")
	requireExec(t, svc, `repl.write(JSON.stringify(catchShadow))`, "outer")
}

func TestBrowserReplPartialDeclaratorInitialization(t *testing.T) {
	svc := newBrowserReplSvc(t)
	requireExecError(t, svc, `let partialDeclaratorA = 17, partialDeclaratorB = (() => { throw new Error("boom"); })();`, "boom")
	requireExec(t, svc, `repl.write(JSON.stringify(partialDeclaratorA))`, float64(17))
}

func TestBrowserReplErrorStackUsesCellLines(t *testing.T) {
	t.Run("after multiline function", func(t *testing.T) {
		svc := newBrowserReplSvc(t)
		r := executeBrowserRepl(t, svc, &oapi.ExecuteBrowserReplJSONRequestBody{Code: "function stackLineHelper() {\n  return 1;\n}\nconst stackLineValue = stackLineHelper();\nthrow new Error(\"line probe\");"})
		require.False(t, r.Success)
		require.NotNil(t, r.Stack)
		require.Contains(t, *r.Stack, ".mjs:5:", *r.Stack)
	})

	t.Run("inside multiline function", func(t *testing.T) {
		svc := newBrowserReplSvc(t)
		r := executeBrowserRepl(t, svc, &oapi.ExecuteBrowserReplJSONRequestBody{Code: "function stackLineHelper() {\n  throw new Error(\"line probe\");\n}\nstackLineHelper();"})
		require.False(t, r.Success)
		require.NotNil(t, r.Stack)
		require.Contains(t, *r.Stack, ".mjs:2:", *r.Stack)
	})

	t.Run("inside second multiline declarator", func(t *testing.T) {
		svc := newBrowserReplSvc(t)
		r := executeBrowserRepl(t, svc, &oapi.ExecuteBrowserReplJSONRequestBody{Code: "let stackDeclaratorA = 1,\n    stackDeclaratorB = (() => { throw new Error(\"line2boom\") })();"})
		require.False(t, r.Success)
		require.NotNil(t, r.Stack)
		require.Contains(t, *r.Stack, ".mjs:2:")
	})

	svc := newBrowserReplSvc(t)
	r := executeBrowserRepl(t, svc, &oapi.ExecuteBrowserReplJSONRequestBody{Code: "let stackLineProbe = 1;\nthrow new Error(\"line probe\");"})
	require.False(t, r.Success)
	require.NotNil(t, r.Stack)
	require.True(t, strings.Contains(*r.Stack, "browser-repl-cell-") && strings.Contains(*r.Stack, ".mjs:2:"), *r.Stack)
}

func TestBrowserReplObjectRestDestructuring(t *testing.T) {
	tests := []struct {
		name string
		code string
		want interface{}
	}{
		{
			name: "var",
			code: `var { a, ...rest } = { a: 1, b: 2, c: 3 }; repl.write(JSON.stringify(a + rest.b + rest.c))`,
			want: float64(6),
		},
		{
			name: "let",
			code: `let { a, ...rest } = { a: 1, b: 2 }; repl.write(JSON.stringify(a + rest.b))`,
			want: float64(3),
		},
		{
			name: "const",
			code: `const { a, ...rest } = { a: 1, b: 2 }; repl.write(JSON.stringify(a + rest.b))`,
			want: float64(3),
		},
		{
			name: "var for-of head",
			code: `for (var { a, ...rest } of [{ a: 4, b: 5 }]) {} repl.write(JSON.stringify(a + rest.b))`,
			want: float64(9),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			svc := newBrowserReplSvc(t)
			requireExec(t, svc, test.code, test.want)
		})
	}
}

func TestBrowserReplConstLetSemantics(t *testing.T) {
	svc := newBrowserReplSvc(t)
	await_ := "await new Promise(r => setTimeout(r, 1)); "

	requireExec(t, svc, `const qaConst = 1; `+await_+`repl.write(JSON.stringify('declared'))`, "declared")
	requireExecError(t, svc, `const qaConst = 2; `+await_+`repl.write(JSON.stringify(qaConst))`, "Identifier 'qaConst' has already been declared")
	requireExecError(t, svc, `qaConst = 99; `+await_+`repl.write(JSON.stringify(qaConst))`, "Assignment to constant variable.")
	requireExecError(t, svc, `qaConst = 99`, "Assignment to constant variable.")
	requireExec(t, svc, `repl.write(JSON.stringify(qaConst))`, float64(1))

	requireExec(t, svc, `let qaLet = 10; `+await_+`repl.write(JSON.stringify(qaLet))`, float64(10))
	requireExecError(t, svc, `let qaLet = 11; `+await_+`repl.write(JSON.stringify(qaLet))`, "Identifier 'qaLet' has already been declared")
	requireExec(t, svc, `qaLet = 42; `+await_+`repl.write(JSON.stringify(qaLet))`, float64(42))
	requireExec(t, svc, `repl.write(JSON.stringify(qaLet))`, float64(42))

	requireExec(t, svc, `class QaClass { hi() { return 'hi' } }; `+await_+`repl.write(JSON.stringify(new QaClass().hi()))`, "hi")
	requireExecError(t, svc, `class QaClass {}; `+await_+`1`, "Identifier 'QaClass' has already been declared")
	requireExec(t, svc, `var qaVar = 1; `+await_+`repl.write(JSON.stringify(qaVar))`, float64(1))
	requireExec(t, svc, `var qaVar = 2; `+await_+`repl.write(JSON.stringify(qaVar))`, float64(2))
	requireExec(t, svc, `function qaFn() { return 1 }; `+await_+`repl.write(JSON.stringify(qaFn()))`, float64(1))
	requireExec(t, svc, `function qaFn() { return 2 }; `+await_+`repl.write(JSON.stringify(qaFn()))`, float64(2))

	requireExec(t, svc, `const { a: qaA, b: qaB } = { a: 1, b: 2 }; `+await_+`repl.write(JSON.stringify(qaA + qaB))`, float64(3))
	requireExecError(t, svc, `qaA = 5`, "Assignment to constant variable.")
	requireExec(t, svc, `const qaFastConst = 'fc'`, nil)
	requireExecError(t, svc, `const qaFastConst = 'x'; `+await_+`1`, "Identifier 'qaFastConst' has already been declared")
	requireExec(t, svc, `const qaAsyncConst = 'ac'; `+await_+`repl.write(JSON.stringify(1))`, float64(1))
	requireExecError(t, svc, `const qaAsyncConst = 'x'`, "Identifier 'qaAsyncConst' has already been declared")
	requireExec(t, svc, `repl.write(JSON.stringify(qaAsyncConst))`, "ac")

	requireExecError(t, svc, `let qaFailLet = (() => { throw new Error('initfail') })(); 1`, "initfail")
	requireExecError(t, svc, `let qaFailLet = 2; 2`, "Identifier 'qaFailLet' has already been declared")
	requireExecError(t, svc, `qaWriteTdz = 1; let qaWriteTdz = 2`, "before initialization")
	requireExecError(t, svc, `qaWriteTdz = 3`, "before initialization")
	requireExecError(t, svc, `qaConstWriteTdz = 1; const qaConstWriteTdz = 2`, "before initialization")
	requireExecError(t, svc, `const qaFailConst = (() => { throw new Error('constinitfail') })()`, "constinitfail")
	requireExecError(t, svc, `qaFailConst = 7`, "before initialization")

	requireExec(t, svc, `const qaInitEscape = globalThis[Object.getOwnPropertyNames(globalThis).find(name => name.startsWith('__browser_repl_init_'))]`, nil)
	requireExecError(t, svc, `qaInitEscape.qaConst = 2`, "revoked")
	requireExec(t, svc, `repl.write(JSON.stringify(typeof globalThis["__browser_repl_init_target"]))`, "undefined")
	requireExec(t, svc, `repl.write(JSON.stringify(repl.id))`, svc.browserRepl.id)
}

func TestBrowserReplIgnoresExpressionValues(t *testing.T) {
	svc := newBrowserReplSvc(t)
	r := executeBrowserRepl(t, svc, &oapi.ExecuteBrowserReplJSONRequestBody{Code: `await Promise.resolve(); ({ignored: true})`})
	require.True(t, r.Success, "error: %v", r.Error)
	require.Empty(t, *r.Content)
}

func TestBrowserReplScrollDispatchesExactlyOnce(t *testing.T) {
	fake := newFakeCDPServer(t)
	t.Setenv("CDP_ENDPOINT", fake.wsURL())
	svc := newBrowserReplSvc(t)

	fake.mu.Lock()
	fake.swallowNextWheel = true
	fake.mu.Unlock()
	r := executeBrowserRepl(t, svc, &oapi.ExecuteBrowserReplJSONRequestBody{Code: `await scroll(100, 100, 700, 0)`})
	require.True(t, r.Success, "error: %v", r.Error)
	fake.mu.Lock()
	count := fake.wheelDispatchCount
	y := fake.scrollY
	fake.mu.Unlock()
	require.Equal(t, 1, count, "an acknowledged wheel must never be replayed")
	require.Equal(t, int64(0), y, "the helper must not substitute a second scrolling mechanism")

	r = executeBrowserRepl(t, svc, &oapi.ExecuteBrowserReplJSONRequestBody{Code: `await scroll(100, 100, 700, 0)`})
	require.True(t, r.Success, "error: %v", r.Error)
	fake.mu.Lock()
	count = fake.wheelDispatchCount
	y = fake.scrollY
	fake.mu.Unlock()
	require.Equal(t, 2, count)
	require.Equal(t, int64(700), y)
}

func TestBrowserReplScrollWaitsForAsyncWheelApplication(t *testing.T) {
	fake := newFakeCDPServer(t)
	t.Setenv("CDP_ENDPOINT", fake.wsURL())

	svc := newBrowserReplSvc(t)

	fake.delayedWheelMs.Store(100)

	scrollY := func() int64 {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return fake.scrollY
	}
	wheelCount := func() int {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return fake.wheelDispatchCount
	}

	r := executeBrowserRepl(t, svc, &oapi.ExecuteBrowserReplJSONRequestBody{Code: `await scroll(100, 100, 700, 0); "done"`})
	require.True(t, r.Success, "error: %v", r.Error)
	require.Eventually(t, func() bool { return scrollY() == 700 }, 3*time.Second, 20*time.Millisecond,
		"the wheel must apply exactly once")
	require.Equal(t, 1, wheelCount(), "an asynchronously-applied wheel must not be retried")

	r = executeBrowserRepl(t, svc, &oapi.ExecuteBrowserReplJSONRequestBody{Code: `await scroll(100, 100, 700, 0); "done2"`})
	require.True(t, r.Success, "error: %v", r.Error)
	require.Eventually(t, func() bool { return scrollY() == 1400 }, 3*time.Second, 20*time.Millisecond,
		"the second scroll must move the offset by exactly one delta")
	require.Equal(t, 2, wheelCount())
}

func TestBrowserReplAttachActivatesTarget(t *testing.T) {
	fake := newFakeCDPServer(t)
	t.Setenv("CDP_ENDPOINT", fake.wsURL())

	svc := newBrowserReplSvc(t)

	r := executeBrowserRepl(t, svc, &oapi.ExecuteBrowserReplJSONRequestBody{Code: `await ensureRealTab(); "done"`})
	require.True(t, r.Success, "error: %v", r.Error)

	fake.mu.Lock()
	activated := append([]string(nil), fake.activatedTargets...)
	fake.mu.Unlock()
	require.Contains(t, activated, "target-page-1", "attach must activate the attached target")
}

func TestStrictBrowserReplBodyMiddleware(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	})
	handler := StrictBrowserReplBodyMiddleware(next)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/repl", strings.NewReader(`{"code":"1","bogus":1}`)))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), `unknown field \"bogus\"`)

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/repl", strings.NewReader(`{"code":"1","timeout_sec":5,"reset":false}`)))
	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, `{"code":"1","timeout_sec":5,"reset":false}`, rec.Body.String())

	rec = httptest.NewRecorder()
	huge := `{"code":"` + strings.Repeat("x", maxBrowserReplBodyBytes) + `"}`
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/repl", strings.NewReader(huge)))
	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	require.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	var tooLarge oapi.BadRequestError
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &tooLarge))
	require.Contains(t, tooLarge.Message, "request body exceeds")

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/repl", strings.NewReader(`{nope`)))
	require.Equal(t, http.StatusOK, rec.Code)

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/repl", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/playwright/execute", strings.NewReader(`{"code":"1","bogus":1}`)))
	require.Equal(t, http.StatusOK, rec.Code)
}
