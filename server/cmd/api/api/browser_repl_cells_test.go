package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/require"
)

func TestBrowserReplPersistentClosureIdentity(t *testing.T) {
	svc := newBrowserReplSvc(t)
	exec := func(code string) oapi.ExecuteBrowserCode200JSONResponse {
		t.Helper()
		return execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: code})
	}

	r := exec(`let closureCount = 0; function incrementClosureCount() { return ++closureCount; }`)
	require.True(t, r.Success, "error: %v", r.Error)
	r = exec(`incrementClosureCount()`)
	require.True(t, r.Success, "error: %v", r.Error)
	require.Equal(t, float64(1), r.Result)
	r = exec(`incrementClosureCount()`)
	require.True(t, r.Success, "error: %v", r.Error)
	require.Equal(t, float64(2), r.Result)

	// A later cell updates the same binding captured by the earlier closure.
	r = exec(`closureCount = 10`)
	require.True(t, r.Success, "error: %v", r.Error)
	r = exec(`incrementClosureCount()`)
	require.True(t, r.Success, "error: %v", r.Error)
	require.Equal(t, float64(11), r.Result)

	// A timer uses the same accessor rather than a cell-local snapshot.
	r = exec(`setTimeout(() => { closureCount += 5; }, 1)`)
	require.True(t, r.Success, "error: %v", r.Error)
	r = exec(`await new Promise(resolve => setTimeout(resolve, 10)); closureCount`)
	require.True(t, r.Success, "error: %v", r.Error)
	require.Equal(t, float64(16), r.Result)
}

func TestBrowserReplFunctionDeclarationsUsePersistentAccessor(t *testing.T) {
	svc := newBrowserReplSvc(t)
	exec := func(code string) oapi.ExecuteBrowserCode200JSONResponse {
		t.Helper()
		return execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: code})
	}

	r := exec(`function replFunctionValue() { return 1; } function replFunctionClosure() { return replFunctionValue(); } replFunctionValue = () => 3; replFunctionClosure()`)
	require.True(t, r.Success, "same-cell assignment failed: %v", r.Error)
	require.Equal(t, float64(3), r.Result)
	r = exec(`function replFunctionValue() { return 2; }`)
	require.True(t, r.Success, "redefinition failed: %v", r.Error)
	r = exec(`replFunctionClosure()`)
	require.True(t, r.Success, "closure did not observe redefinition: %v", r.Error)
	require.Equal(t, float64(2), r.Result)

	// Duplicate function declarations are valid and initialize the accessor
	// exactly once with the last declaration.
	r = exec(`function replDuplicate() { return 1; } function replDuplicate() { return 2; } replDuplicate()`)
	require.True(t, r.Success, "duplicate declaration failed: %v", r.Error)
	require.Equal(t, float64(2), r.Result)

	r = exec(`function replFunctionNameProbe() {} replFunctionNameProbe.name`)
	require.True(t, r.Success, "function name probe failed: %v", r.Error)
	require.Equal(t, "replFunctionNameProbe", r.Result)
}

func TestBrowserReplBracelessMultiDeclaratorVarPreservesControlFlow(t *testing.T) {
	svc := newBrowserReplSvc(t)
	exec := func(code string) oapi.ExecuteBrowserCode200JSONResponse {
		t.Helper()
		return execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: code})
	}

	cases := []struct {
		code string
		want interface{}
	}{
		{`if (false) var bracelessIfX = 1, bracelessIfY = 2; typeof bracelessIfY`, "undefined"},
		{`do var bracelessDoX = 1, bracelessDoY = 2; while (false); bracelessDoX + bracelessDoY`, float64(3)},
		{`for (const bracelessForElement of [1, 2]) var bracelessForX = bracelessForElement, bracelessForY = bracelessForElement * 2; bracelessForY`, float64(4)},
		{`var bracelessCommentX = 1 /* comma, stays */, bracelessCommentY = 2; bracelessCommentX + bracelessCommentY`, float64(3)},
	}
	for _, test := range cases {
		r := exec(test.code)
		require.True(t, r.Success, "code %q failed: %v", test.code, r.Error)
		require.Equal(t, test.want, r.Result, "code: %q", test.code)
	}
}

func TestBrowserReplBracelessVarWithoutInitializerPreservesControlFlow(t *testing.T) {
	svc := newBrowserReplSvc(t)
	exec := func(code string) oapi.ExecuteBrowserCode200JSONResponse {
		t.Helper()
		return execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: code})
	}

	cases := []struct {
		code string
		want interface{}
	}{
		{`var replVarLog = []; if (false) var replIfNoInit; replVarLog.push('ran'); replVarLog`, []interface{}{"ran"}},
		{`do var replDoNoInit; while (false); typeof replDoNoInit`, "undefined"},
		{`for (let replForIndex = 0; replForIndex < 1; replForIndex++) var replForNoInit; typeof replForNoInit`, "undefined"},
		{`for (const replForOfIndex of [1]) var replForOfNoInit; typeof replForOfNoInit`, "undefined"},
	}
	for _, test := range cases {
		r := exec(test.code)
		require.True(t, r.Success, "code %q failed: %v", test.code, r.Error)
		require.Equal(t, test.want, r.Result, "code: %q", test.code)
	}
}

func TestBrowserReplStrayOutputBufferResetsAndPropagatesTruncation(t *testing.T) {
	svc := newBrowserReplSvc(t)
	exec := func(code string) oapi.ExecuteBrowserCode200JSONResponse {
		t.Helper()
		return execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: code})
	}
	largeOutput := 256 * 1024
	schedule := fmt.Sprintf(`setTimeout(() => { repl.write("a".repeat(%d)); repl.write("dropped"); }, 10); "scheduled"`, largeOutput)

	r := exec(schedule)
	require.True(t, r.Success, "schedule failed: %v", r.Error)
	time.Sleep(50 * time.Millisecond)

	r = exec(`"drain"`)
	require.True(t, r.Success, "drain failed: %v", r.Error)
	require.NotNil(t, r.ContentTruncated)
	require.True(t, *r.ContentTruncated, "dropped stray output must be reported")
	require.NotNil(t, r.Content)
	require.Len(t, *r.Content, 1)
	content, err := (*r.Content)[0].AsBrowserExecutionTextContent()
	require.NoError(t, err)
	require.Len(t, content.Text, largeOutput)

	// The first drain must reset the collector's private byte budget. A second
	// max-sized stray item should therefore survive into the next execution.
	r = exec(fmt.Sprintf(`setTimeout(() => repl.write("b".repeat(%d)), 10); "scheduled"`, largeOutput))
	require.True(t, r.Success, "second schedule failed: %v", r.Error)
	time.Sleep(50 * time.Millisecond)
	r = exec(`"second drain"`)
	require.True(t, r.Success, "second drain failed: %v", r.Error)
	require.NotNil(t, r.ContentTruncated)
	require.False(t, *r.ContentTruncated)
	require.NotNil(t, r.Content)
	require.Len(t, *r.Content, 1)
	content, err = (*r.Content)[0].AsBrowserExecutionTextContent()
	require.NoError(t, err)
	require.Len(t, content.Text, largeOutput)
}

func TestBrowserReplNestedVarBindingsPersist(t *testing.T) {
	svc := newBrowserReplSvc(t)
	exec := func(code string) oapi.ExecuteBrowserCode200JSONResponse {
		t.Helper()
		return execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: code})
	}

	cases := []struct {
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
	}
	for _, test := range cases {
		r := exec(test.declaration)
		require.True(t, r.Success, "declaration %q failed: %v", test.declaration, r.Error)
		r = exec(test.name)
		require.True(t, r.Success, "read %s failed: %v", test.name, r.Error)
		require.Equal(t, test.value, r.Result, "read %s", test.name)
	}
}

func TestBrowserReplPartialDeclaratorInitialization(t *testing.T) {
	svc := newBrowserReplSvc(t)
	exec := func(code string) oapi.ExecuteBrowserCode200JSONResponse {
		t.Helper()
		return execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: code})
	}

	r := exec(`let partialDeclaratorA = 17, partialDeclaratorB = (() => { throw new Error("boom"); })();`)
	require.False(t, r.Success)
	require.Contains(t, *r.Error, "boom")
	r = exec(`partialDeclaratorA`)
	require.True(t, r.Success, "initialized declarator was lost: %v", r.Error)
	require.Equal(t, float64(17), r.Result)
}

func TestBrowserReplErrorStackUsesCellLines(t *testing.T) {
	t.Run("after multiline function", func(t *testing.T) {
		svc := newBrowserReplSvc(t)
		r := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "function stackLineHelper() {\n  return 1;\n}\nconst stackLineValue = stackLineHelper();\nthrow new Error(\"line probe\");"})
		require.False(t, r.Success)
		require.NotNil(t, r.Stack)
		require.Contains(t, *r.Stack, ".mjs:5:", *r.Stack)
	})

	t.Run("inside multiline function", func(t *testing.T) {
		svc := newBrowserReplSvc(t)
		r := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "function stackLineHelper() {\n  throw new Error(\"line probe\");\n}\nstackLineHelper();"})
		require.False(t, r.Success)
		require.NotNil(t, r.Stack)
		require.Contains(t, *r.Stack, ".mjs:2:", *r.Stack)
	})

	t.Run("inside second multiline declarator", func(t *testing.T) {
		svc := newBrowserReplSvc(t)
		r := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "let stackDeclaratorA = 1,\n    stackDeclaratorB = (() => { throw new Error(\"line2boom\") })();"})
		require.False(t, r.Success)
		require.NotNil(t, r.Stack)
		require.Contains(t, *r.Stack, ".mjs:2:")
	})

	// A cell without a declaration still uses the same one-line offset.
	svc := newBrowserReplSvc(t)
	r := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "let stackLineProbe = 1;\nthrow new Error(\"line probe\");"})
	require.False(t, r.Success)
	require.NotNil(t, r.Stack)
	require.True(t, strings.Contains(*r.Stack, "browser-repl-cell-") && strings.Contains(*r.Stack, ".mjs:2:"), *r.Stack)
}

func TestBrowserReplConstLetSemantics(t *testing.T) {
	svc := newBrowserReplSvc(t)
	await_ := "await new Promise(r => setTimeout(r, 1)); "

	exec := func(code string) oapi.ExecuteBrowserCode200JSONResponse {
		t.Helper()
		return execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: code})
	}
	expectErr := func(code string, substr string) {
		t.Helper()
		r := exec(code)
		require.False(t, r.Success, "expected failure for %q", code)
		require.NotNil(t, r.Error)
		require.Contains(t, *r.Error, substr, "code: %q", code)
	}
	expectResult := func(code string, want interface{}) {
		t.Helper()
		r := exec(code)
		require.True(t, r.Success, "code %q failed: %v", code, r.Error)
		require.Equal(t, want, r.Result, "code: %q", code)
	}

	// const keeps redeclaration protection and immutability with and without
	// top-level await.
	exec(`const qaConst = 1; ` + await_ + `'declared'`)
	expectErr(`const qaConst = 2; `+await_+`qaConst`, "Identifier 'qaConst' has already been declared")
	expectErr(`qaConst = 99; `+await_+`qaConst`, "Assignment to constant variable.")
	expectErr(`qaConst = 99`, "Assignment to constant variable.")
	expectResult(`qaConst`, float64(1))

	// let stays mutable but cannot be redeclared.
	expectResult(`let qaLet = 10; `+await_+`qaLet`, float64(10))
	expectErr(`let qaLet = 11; `+await_+`qaLet`, "Identifier 'qaLet' has already been declared")
	expectResult(`qaLet = 42; `+await_+`qaLet`, float64(42))
	expectResult(`qaLet`, float64(42))

	// class declarations get let-like protection.
	expectResult(`class QaClass { hi() { return 'hi' } }; `+await_+`new QaClass().hi()`, "hi")
	expectErr(`class QaClass {}; `+await_+`1`, "Identifier 'QaClass' has already been declared")

	// var and function declarations stay freely redeclarable.
	expectResult(`var qaVar = 1; `+await_+`qaVar`, float64(1))
	expectResult(`var qaVar = 2; `+await_+`qaVar`, float64(2))
	expectResult(`function qaFn() { return 1 }; `+await_+`qaFn()`, float64(1))
	expectResult(`function qaFn() { return 2 }; `+await_+`qaFn()`, float64(2))

	// Destructured const bindings persist and are protected.
	expectResult(`const { a: qaA, b: qaB } = { a: 1, b: 2 }; `+await_+`qaA + qaB`, float64(3))
	expectErr(`qaA = 5`, "Assignment to constant variable.")

	// Redeclaration protection is consistent with and without top-level await.
	exec(`const qaFastConst = 'fc'`)
	expectErr(`const qaFastConst = 'x'; `+await_+`1`, "Identifier 'qaFastConst' has already been declared")
	exec(`const qaAsyncConst = 'ac'; ` + await_ + `1`)
	expectErr(`const qaAsyncConst = 'x'`, "Identifier 'qaAsyncConst' has already been declared")
	expectResult(`qaAsyncConst`, "ac")

	// A failed initializer still reserves the name and leaves it in the TDZ
	// (module declarations instantiate before the first statement runs).
	expectErr(`let qaFailLet = (() => { throw new Error('initfail') })(); 1`, "initfail")
	expectErr(`let qaFailLet = 2; 2`, "Identifier 'qaFailLet' has already been declared")

	// Writes before a lexical declaration are also TDZ errors, rather than
	// initialization through the persistent accessor.
	expectErr(`qaWriteTdz = 1; let qaWriteTdz = 2`, "before initialization")
	expectErr(`qaWriteTdz = 3`, "before initialization")
	expectErr(`qaConstWriteTdz = 1; const qaConstWriteTdz = 2`, "before initialization")

	// A failed const initializer remains uninitialized and cannot be repaired by
	// a later assignment.
	expectErr(`const qaFailConst = (() => { throw new Error('constinitfail') })()`, "constinitfail")
	expectErr(`qaFailConst = 7`, "before initialization")

	// The initialization target is per-cell, revoked, and removed after
	// evaluation, even if user code discovers and retains the proxy.
	exec(`const qaInitEscape = globalThis[Object.getOwnPropertyNames(globalThis).find(name => name.startsWith('__browser_repl_init_'))]`)
	expectErr(`qaInitEscape.qaConst = 2`, "revoked")
	expectResult(`typeof globalThis["__browser_repl_init_target"]`, "undefined")

	// None of the failures above terminated the REPL.
	r := exec(`repl.id`)
	require.True(t, r.Success)
}

// TestBrowserReplTrailingBlockResult verifies that every SourceTextModule cell
// reports only an implicit final expression, never a block completion value.
func TestBrowserReplTrailingBlockResult(t *testing.T) {
	svc := newBrowserReplSvc(t)

	r := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{
		Code: `try { await new Promise(r => setTimeout(r, 1)); 'after' } catch { 'caught' }`,
	})
	require.True(t, r.Success, "error: %v", r.Error)
	require.NotNil(t, r.ResultRepr)
	require.Equal(t, "undefined", *r.ResultRepr)

	// A block has no implicit result on the module-cell path either.
	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `try { 42 } catch {}`})
	require.True(t, r.Success, "error: %v", r.Error)
	require.Equal(t, "undefined", *r.ResultRepr)

	// A trailing expression after the await still yields an implicit result.
	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{
		Code: `await new Promise(r => setTimeout(r, 1)); 'tail'`,
	})
	require.True(t, r.Success, "error: %v", r.Error)
	require.Equal(t, "tail", r.Result)
}

// TestBrowserReplScrollRetriesSwallowedWheel covers the QA finding that
// Chromium silently swallows the first mouseWheel dispatch after a
// navigation: the command answers normally but the page never scrolls, so
// the dispatch-timeout fallback cannot detect it. scroll() must detect a
// no-op dispatch on a scrollable page and retry once.
func TestBrowserReplScrollRetriesSwallowedWheel(t *testing.T) {
	fake := newFakeCDPServer(t)
	t.Setenv("CDP_ENDPOINT", fake.wsURL())

	svc := newBrowserReplSvc(t)

	fake.mu.Lock()
	fake.maxScrollY = 2000
	fake.swallowNextWheel = true
	fake.mu.Unlock()

	r := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `await scroll(100, 100, 0, 700); "done"`})
	require.True(t, r.Success, "error: %v", r.Error)
	fake.mu.Lock()
	count := fake.wheelDispatchCount
	y := fake.scrollY
	fake.mu.Unlock()
	require.Equal(t, 2, count, "the swallowed dispatch must be retried exactly once")
	require.Equal(t, int64(700), y, "the retry must actually scroll")
	require.NotNil(t, r.Content)
	sawNote := false
	for _, item := range *r.Content {
		txt, err := item.AsBrowserExecutionTextContent()
		if err == nil && txt.Channel == "stderr" && strings.Contains(txt.Text, "retrying once") {
			sawNote = true
		}
	}
	require.True(t, sawNote, "the retry must be surfaced as a stderr content item, got %v", r.Content)

	// Once the input pipeline is awake the next scroll dispatches exactly
	// once and moves without a retry.
	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `await scroll(100, 100, 0, 700); "done2"`})
	require.True(t, r.Success, "error: %v", r.Error)
	fake.mu.Lock()
	count = fake.wheelDispatchCount
	y = fake.scrollY
	fake.mu.Unlock()
	require.Equal(t, 3, count)
	require.Equal(t, int64(1400), y)
	if r.Content != nil {
		for _, item := range *r.Content {
			txt, err := item.AsBrowserExecutionTextContent()
			require.False(t, err == nil && txt.Channel == "stderr" && strings.Contains(txt.Text, "retrying once"),
				"no retry expected once the pipeline is awake")
		}
	}

	// An unscrollable page is left unverified: a single dispatch, no retry.
	fake.mu.Lock()
	fake.maxScrollY = 0
	fake.swallowNextWheel = true
	fake.mu.Unlock()
	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `await scroll(100, 100, 0, 700); "done3"`})
	require.True(t, r.Success, "error: %v", r.Error)
	fake.mu.Lock()
	count = fake.wheelDispatchCount
	fake.mu.Unlock()
	require.Equal(t, 4, count, "unscrollable pages must not be retried")
}

// TestBrowserReplScrollWaitsForAsyncWheelApplication covers the QA finding
// that scroll() probed the scroll offset before Chromium asynchronously
// applied the wheel, so the swallowed-wheel retry false-positived on every
// scroll and the page landed 2x past its target. scroll() must poll the
// offset through a settle window before declaring a dispatch had no effect.
func TestBrowserReplScrollWaitsForAsyncWheelApplication(t *testing.T) {
	fake := newFakeCDPServer(t)
	t.Setenv("CDP_ENDPOINT", fake.wsURL())

	svc := newBrowserReplSvc(t)

	fake.mu.Lock()
	fake.maxScrollY = 5000
	fake.mu.Unlock()
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

	// The wheel applies ~100ms after the command answers. scroll() must not
	// mistake the not-yet-applied offset for a swallowed dispatch: exactly
	// one dispatch, no retry, no double scroll.
	r := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `await scroll(100, 100, 0, 700); "done"`})
	require.True(t, r.Success, "error: %v", r.Error)
	require.Eventually(t, func() bool { return scrollY() == 700 }, 3*time.Second, 20*time.Millisecond,
		"the wheel must apply exactly once")
	require.Equal(t, 1, wheelCount(), "an asynchronously-applied wheel must not be retried")
	if r.Content != nil {
		for _, item := range *r.Content {
			txt, err := item.AsBrowserExecutionTextContent()
			require.False(t, err == nil && txt.Channel == "stderr" && strings.Contains(txt.Text, "retrying once"),
				"no retry expected when the wheel applies within the settle window")
		}
	}

	// A second scroll lands exactly one more delta (cumulative 1400).
	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `await scroll(100, 100, 0, 700); "done2"`})
	require.True(t, r.Success, "error: %v", r.Error)
	require.Eventually(t, func() bool { return scrollY() == 1400 }, 3*time.Second, 20*time.Millisecond,
		"the second scroll must move the offset by exactly one delta")
	require.Equal(t, 2, wheelCount())
}

// TestBrowserReplAttachActivatesTarget covers the QA finding that JS
// dialogs were non-deterministically auto-cancelled after a Chromium
// restart: which tab is foreground is not deterministic across restarts,
// and headless Chromium auto-cancels hidden tabs' dialogs
// (Page.javascriptDialogClosed result:false immediately after opening).
// Attach must activate the target so dialog semantics are stable.
func TestBrowserReplAttachActivatesTarget(t *testing.T) {
	fake := newFakeCDPServer(t)
	t.Setenv("CDP_ENDPOINT", fake.wsURL())

	svc := newBrowserReplSvc(t)

	r := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `await ensure_real_tab(); "done"`})
	require.True(t, r.Success, "error: %v", r.Error)

	fake.mu.Lock()
	activated := append([]string(nil), fake.activatedTargets...)
	fake.mu.Unlock()
	require.Contains(t, activated, "target-page-1", "attach must activate the attached target")
}

// TestBrowserReplOrphanedDaemonKilled covers the QA finding that an
// orphaned REPL daemon (started outside the API process) was unlinked but
// never killed, leaking for the container lifetime. Lazy start must kill
// the orphan holding the socket before replacing it.
func TestBrowserReplOrphanedDaemonKilled(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("orphan detection requires /proc")
	}
	script := ensureBrowserReplBundle(t)
	svc := newBrowserReplSvc(t)
	socketPath := browserReplSocketPath()

	// Start a rogue daemon on the API's socket path, outside the API's
	// supervision (the pdeathsig path cannot cover it).
	rogue := exec.Command("node", script)
	rogue.Env = append(os.Environ(),
		"BROWSER_REPL_SOCKET="+socketPath,
		"BROWSER_REPL_ID=rogue0000000000000000000",
	)
	require.NoError(t, rogue.Start())
	t.Cleanup(func() {
		_ = rogue.Process.Kill()
		_, _ = rogue.Process.Wait()
	})

	// Wait until the rogue is listening on the socket.
	deadline := time.Now().Add(10 * time.Second)
	for {
		conn, err := net.DialTimeout("unix", socketPath, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		require.False(t, time.Now().After(deadline), "rogue daemon never started listening")
		time.Sleep(50 * time.Millisecond)
	}

	// The next execution must refuse adoption, kill the orphan, and serve
	// from a fresh child with a different repl_id.
	r := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "1 + 1"})
	require.True(t, r.Success, "error: %v", r.Error)
	require.Equal(t, float64(2), r.Result)
	require.NotEqual(t, "rogue0000000000000000000", r.ReplId)

	// The rogue process must be dead (zombie until this test reaps it).
	deadline = time.Now().Add(3 * time.Second)
	for {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", rogue.Process.Pid))
		if err != nil {
			break // gone
		}
		state := ""
		if idx := strings.LastIndex(string(data), ")"); idx >= 0 {
			if fields := strings.Fields(string(data)[idx+1:]); len(fields) > 0 {
				state = fields[0]
			}
		}
		if state == "Z" {
			break
		}
		require.False(t, time.Now().After(deadline), "orphaned REPL process %d still alive", rogue.Process.Pid)
		time.Sleep(50 * time.Millisecond)
	}

	// The fresh child keeps serving with the same new repl_id.
	r2 := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "repl.id"})
	require.True(t, r2.Success)
	require.Equal(t, r.ReplId, r2.Result)
}

// TestBrowserReplOrphanedDaemonKilledOnChildDeath covers the QA finding
// that the orphan kill only ran on the API-restart path: when the owned
// child died and a foreign daemon took the socket before the next request,
// the stale socket was unlinked before the orphan-detection dial, so the
// orphan leaked for the container lifetime. The kill must run before the
// unlink on every lazy-start path.
func TestBrowserReplOrphanedDaemonKilledOnChildDeath(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("orphan detection requires /proc")
	}
	script := ensureBrowserReplBundle(t)
	svc := newBrowserReplSvc(t)
	socketPath := browserReplSocketPath()

	// Start the owned child.
	r := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "1 + 1"})
	require.True(t, r.Success, "error: %v", r.Error)

	// Kill the API's child out from under it and give the wait goroutine a
	// moment to reap it.
	svc.browserReplMu.Lock()
	child := svc.browserRepl
	svc.browserReplMu.Unlock()
	require.NotNil(t, child)
	require.NoError(t, child.cmd.Process.Kill())
	time.Sleep(300 * time.Millisecond)

	// A foreign daemon takes the socket before the next request.
	require.NoError(t, os.Remove(socketPath))
	rogue := exec.Command("node", script)
	rogue.Env = append(os.Environ(),
		"BROWSER_REPL_SOCKET="+socketPath,
		"BROWSER_REPL_ID=rogue0000000000000000000",
	)
	require.NoError(t, rogue.Start())
	t.Cleanup(func() {
		_ = rogue.Process.Kill()
		_, _ = rogue.Process.Wait()
	})

	// Wait until the rogue is listening on the socket.
	deadline := time.Now().Add(10 * time.Second)
	for {
		conn, err := net.DialTimeout("unix", socketPath, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		require.False(t, time.Now().After(deadline), "rogue daemon never started listening")
		time.Sleep(50 * time.Millisecond)
	}

	// The next execution must kill the orphan (not just unlink its socket)
	// and serve from a fresh child with a different repl_id.
	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "1 + 1"})
	require.True(t, r.Success, "error: %v", r.Error)
	require.Equal(t, float64(2), r.Result)
	require.NotEqual(t, "rogue0000000000000000000", r.ReplId)

	// The rogue process must be dead (zombie until this test reaps it).
	deadline = time.Now().Add(3 * time.Second)
	for {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", rogue.Process.Pid))
		if err != nil {
			break // gone
		}
		state := ""
		if idx := strings.LastIndex(string(data), ")"); idx >= 0 {
			if fields := strings.Fields(string(data)[idx+1:]); len(fields) > 0 {
				state = fields[0]
			}
		}
		if state == "Z" {
			break
		}
		require.False(t, time.Now().After(deadline), "orphaned REPL process %d still alive", rogue.Process.Pid)
		time.Sleep(50 * time.Millisecond)
	}
}

// TestBrowserReplRecordingIDAlias covers the QA finding that
// start_recording({recorder_id: ...}) silently ignored the field (the
// Kernel recording API's request field is `id`, while the helpers return
// `recorder_id`), breaking the documented "fresh ID per recording"
// workaround. The helpers must map recorder_id to id.
func TestBrowserReplRecordingIDAlias(t *testing.T) {
	fake := newFakeCDPServer(t)

	var mu sync.Mutex
	var startBody, stopBody map[string]any
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		switch r.URL.Path {
		case "/recording/start":
			_ = json.Unmarshal(body, &startBody)
		case "/recording/stop":
			_ = json.Unmarshal(body, &stopBody)
		}
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(api.Close)
	apiPort := api.URL[strings.LastIndex(api.URL, ":")+1:]

	t.Setenv("CDP_ENDPOINT", fake.wsURL())
	t.Setenv("KERNEL_API_PORT", apiPort)

	svc := newBrowserReplSvc(t)

	r := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `
		const started = await start_recording({ recorder_id: "custom-rec-1" });
		const stopped = await stop_recording({ recorder_id: "custom-rec-1" });
		({ started: started.recorder_id, stopped: stopped.recorder_id })
	`})
	require.True(t, r.Success, "error: %v", r.Error)
	res, ok := r.Result.(map[string]any)
	require.True(t, ok)
	require.Equal(t, "custom-rec-1", res["started"])
	require.Equal(t, "custom-rec-1", res["stopped"])

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, "custom-rec-1", startBody["id"], "recorder_id maps to the API's id field")
	require.NotContains(t, startBody, "recorder_id", "the alias must not leak to the API")
	require.Equal(t, "custom-rec-1", stopBody["id"])
	require.NotContains(t, stopBody, "recorder_id")
}

// TestStrictBrowserExecuteBodyMiddleware verifies that unknown request
// fields are rejected per the schema's additionalProperties: false, while
// valid and otherwise-malformed requests pass through to the strict
// handler's own decoding.
func TestStrictBrowserExecuteBodyMiddleware(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	})
	handler := StrictBrowserExecuteBodyMiddleware(next)

	// Unknown fields are rejected.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/browser/execute", strings.NewReader(`{"code":"1","bogus":1}`)))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), `unknown field \"bogus\"`)

	// A valid body passes through intact.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/browser/execute", strings.NewReader(`{"code":"1","timeout_sec":5,"reset":false}`)))
	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, `{"code":"1","timeout_sec":5,"reset":false}`, rec.Body.String())

	// The middleware bounds its own buffering before strict decoding.
	rec = httptest.NewRecorder()
	huge := `{"code":"` + strings.Repeat("x", maxBrowserExecuteBodyBytes) + `"}`
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/browser/execute", strings.NewReader(huge)))
	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	require.Contains(t, rec.Body.String(), "request body exceeds")

	// Malformed JSON is left for the strict handler's own 400 path.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/browser/execute", strings.NewReader(`{nope`)))
	require.Equal(t, http.StatusOK, rec.Code)

	// Other methods and paths pass through untouched.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/browser/execute", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/playwright/execute", strings.NewReader(`{"code":"1","bogus":1}`)))
	require.Equal(t, http.StatusOK, rec.Code)
}

// TestBrowserReplSourceTextModuleCells covers the JavaScript-only evaluator
// contract independently of browser connection behavior.
func TestBrowserReplSourceTextModuleCells(t *testing.T) {
	svc := newBrowserReplSvc(t)
	exec := func(code string) oapi.ExecuteBrowserCode200JSONResponse {
		t.Helper()
		return execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: code})
	}

	r := exec(`var cellVar = 1; let cellLet = 2; const cellConst = 3;
		function cellFn() { return cellVar + cellLet; }
		class CellClass {}
		({ cellVar, cellLet, cellConst, fn: cellFn(), className: new CellClass().constructor.name })`)
	require.True(t, r.Success, "error: %v", r.Error)

	r = exec(`cellVar++; cellLet += 2; cellFn = () => 99; cellVar + cellLet + cellFn()`)
	require.True(t, r.Success, "error: %v", r.Error)
	require.Equal(t, float64(105), r.Result)

	r = exec(`cellConst = 4`)
	require.False(t, r.Success)
	require.Contains(t, *r.Error, "Assignment to constant variable")

	r = exec(`const cellLet = 9`)
	require.False(t, r.Success)
	require.Contains(t, *r.Error, "Identifier 'cellLet' has already been declared")

	r = exec(`let initializedBeforeFailure = 7; throw new Error("cell failure")`)
	require.False(t, r.Success)
	r = exec(`initializedBeforeFailure`)
	require.True(t, r.Success, "error: %v", r.Error)
	require.Equal(t, float64(7), r.Result)

	r = exec(`(await import("node:path")).basename("/tmp/cell")`)
	require.True(t, r.Success, "error: %v", r.Error)
	require.Equal(t, "cell", r.Result)

	r = exec(`return 1`)
	require.False(t, r.Success)
	require.Contains(t, *r.Error, "top-level return is not supported")
}
