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
	requireExec(t, svc, `let closureCount = 0; function incrementClosureCount() { return ++closureCount; }`, nil)
	requireExec(t, svc, `incrementClosureCount()`, float64(1))
	requireExec(t, svc, `incrementClosureCount()`, float64(2))
	requireExec(t, svc, `closureCount = 10`, float64(10))
	requireExec(t, svc, `incrementClosureCount()`, float64(11))
	requireExec(t, svc, `setTimeout(() => { closureCount += 5; }, 1); undefined`, nil)
	requireExec(t, svc, `await new Promise(resolve => setTimeout(resolve, 10)); closureCount`, float64(16))
}

func TestBrowserReplFunctionDeclarationsUsePersistentAccessor(t *testing.T) {
	svc := newBrowserReplSvc(t)
	requireExec(t, svc, `function replFunctionValue() { return 1; } function replFunctionClosure() { return replFunctionValue(); } replFunctionValue = () => 3; replFunctionClosure()`, float64(3))
	requireExec(t, svc, `function replFunctionValue() { return 2; }`, nil)
	requireExec(t, svc, `replFunctionClosure()`, float64(2))
	requireExec(t, svc, `function replDuplicate() { return 1; } function replDuplicate() { return 2; } replDuplicate()`, float64(2))
	requireExec(t, svc, `function replFunctionNameProbe() {} replFunctionNameProbe.name`, "replFunctionNameProbe")
}

func TestBrowserReplBracelessVarPreservesControlFlow(t *testing.T) {
	svc := newBrowserReplSvc(t)
	for _, test := range []struct {
		code string
		want any
	}{
		{`if (false) var bracelessIfX = 1, bracelessIfY = 2; typeof bracelessIfY`, "undefined"},
		{`do var bracelessDoX = 1, bracelessDoY = 2; while (false); bracelessDoX + bracelessDoY`, float64(3)},
		{`for (const bracelessForElement of [1, 2]) var bracelessForX = bracelessForElement, bracelessForY = bracelessForElement * 2; bracelessForY`, float64(4)},
		{`var bracelessCommentX = 1 /* comma, stays */, bracelessCommentY = 2; bracelessCommentX + bracelessCommentY`, float64(3)},
		{`var replVarLog = []; if (false) var replIfNoInit; replVarLog.push('ran'); replVarLog`, []interface{}{"ran"}},
		{`do var replDoNoInit; while (false); typeof replDoNoInit`, "undefined"},
		{`for (let replForIndex = 0; replForIndex < 1; replForIndex++) var replForNoInit; typeof replForNoInit`, "undefined"},
		{`for (const replForOfIndex of [1]) var replForOfNoInit; typeof replForOfNoInit`, "undefined"},
	} {
		requireExec(t, svc, test.code, test.want)
	}
}

func TestBrowserReplStrayOutputBufferResetsAndPropagatesTruncation(t *testing.T) {
	svc := newBrowserReplSvc(t)
	const size = 256 * 1024
	requireExec(t, svc, fmt.Sprintf(`setTimeout(() => { repl.write("a".repeat(%d)); repl.write("dropped"); }, 10); "scheduled"`, size), "scheduled")
	time.Sleep(50 * time.Millisecond)

	r := requireExec(t, svc, `"drain"`, "drain")
	require.True(t, *r.ContentTruncated)
	require.Len(t, *r.Content, 1)
	content, err := (*r.Content)[0].AsBrowserExecutionTextContent()
	require.NoError(t, err)
	require.Len(t, content.Text, size)

	requireExec(t, svc, fmt.Sprintf(`setTimeout(() => repl.write("b".repeat(%d)), 10); "scheduled"`, size), "scheduled")
	time.Sleep(50 * time.Millisecond)
	r = requireExec(t, svc, `"second drain"`, "second drain")
	require.False(t, *r.ContentTruncated)
	require.Len(t, *r.Content, 1)
	content, err = (*r.Content)[0].AsBrowserExecutionTextContent()
	require.NoError(t, err)
	require.Len(t, content.Text, size)
}

func TestBrowserReplStrayItemLimitsPropagateTruncation(t *testing.T) {
	t.Run("text", func(t *testing.T) {
		svc := newBrowserReplSvc(t)
		requireExec(t, svc, `setTimeout(() => { for (let i = 0; i < 1500; i++) repl.write("s" + i); }, 10); "scheduled"`, "scheduled")
		time.Sleep(50 * time.Millisecond)
		r := requireExec(t, svc, `"drain"`, "drain")
		require.True(t, *r.ContentTruncated)
		require.Len(t, *r.Content, 1000)
		first, err := (*r.Content)[0].AsBrowserExecutionTextContent()
		require.NoError(t, err)
		require.Equal(t, "s500", first.Text)
	})

	t.Run("images", func(t *testing.T) {
		svc := newBrowserReplSvc(t)
		requireExec(t, svc, `const png = Buffer.from([137, 80, 78, 71, 0, 0, 0, 0, 0]); setTimeout(async () => { for (let i = 0; i < 1500; i++) await repl.emitImage(png); }, 10); "scheduled"`, "scheduled")
		time.Sleep(50 * time.Millisecond)
		r := requireExec(t, svc, `"drain"`, "drain")
		require.True(t, *r.ContentTruncated)
		require.Len(t, *r.Content, 1000)
		for _, item := range *r.Content {
			image, err := item.AsBrowserExecutionImageContent()
			require.NoError(t, err)
			require.Equal(t, oapi.BrowserExecutionImageContentType("image"), image.Type)
		}
	})
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
		requireExec(t, svc, test.name, test.value)
	}
}

func TestBrowserReplPartialDeclaratorInitialization(t *testing.T) {
	svc := newBrowserReplSvc(t)
	requireExecError(t, svc, `let partialDeclaratorA = 17, partialDeclaratorB = (() => { throw new Error("boom"); })();`, "boom")
	requireExec(t, svc, `partialDeclaratorA`, float64(17))
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

	svc := newBrowserReplSvc(t)
	r := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "let stackLineProbe = 1;\nthrow new Error(\"line probe\");"})
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
			code: `var { a, ...rest } = { a: 1, b: 2, c: 3 }; a + rest.b + rest.c`,
			want: float64(6),
		},
		{
			name: "let",
			code: `let { a, ...rest } = { a: 1, b: 2 }; a + rest.b`,
			want: float64(3),
		},
		{
			name: "const",
			code: `const { a, ...rest } = { a: 1, b: 2 }; a + rest.b`,
			want: float64(3),
		},
		{
			name: "var for-of head",
			code: `for (var { a, ...rest } of [{ a: 4, b: 5 }]) {} a + rest.b`,
			want: float64(9),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			svc := newBrowserReplSvc(t)
			r := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: test.code})
			require.True(t, r.Success, "code failed: %v", r.Error)
			require.Equal(t, test.want, r.Result)
		})
	}
}

func TestBrowserReplConstLetSemantics(t *testing.T) {
	svc := newBrowserReplSvc(t)
	await_ := "await new Promise(r => setTimeout(r, 1)); "

	requireExec(t, svc, `const qaConst = 1; `+await_+`'declared'`, "declared")
	requireExecError(t, svc, `const qaConst = 2; `+await_+`qaConst`, "Identifier 'qaConst' has already been declared")
	requireExecError(t, svc, `qaConst = 99; `+await_+`qaConst`, "Assignment to constant variable.")
	requireExecError(t, svc, `qaConst = 99`, "Assignment to constant variable.")
	requireExec(t, svc, `qaConst`, float64(1))

	requireExec(t, svc, `let qaLet = 10; `+await_+`qaLet`, float64(10))
	requireExecError(t, svc, `let qaLet = 11; `+await_+`qaLet`, "Identifier 'qaLet' has already been declared")
	requireExec(t, svc, `qaLet = 42; `+await_+`qaLet`, float64(42))
	requireExec(t, svc, `qaLet`, float64(42))

	requireExec(t, svc, `class QaClass { hi() { return 'hi' } }; `+await_+`new QaClass().hi()`, "hi")
	requireExecError(t, svc, `class QaClass {}; `+await_+`1`, "Identifier 'QaClass' has already been declared")
	requireExec(t, svc, `var qaVar = 1; `+await_+`qaVar`, float64(1))
	requireExec(t, svc, `var qaVar = 2; `+await_+`qaVar`, float64(2))
	requireExec(t, svc, `function qaFn() { return 1 }; `+await_+`qaFn()`, float64(1))
	requireExec(t, svc, `function qaFn() { return 2 }; `+await_+`qaFn()`, float64(2))

	requireExec(t, svc, `const { a: qaA, b: qaB } = { a: 1, b: 2 }; `+await_+`qaA + qaB`, float64(3))
	requireExecError(t, svc, `qaA = 5`, "Assignment to constant variable.")
	requireExec(t, svc, `const qaFastConst = 'fc'`, nil)
	requireExecError(t, svc, `const qaFastConst = 'x'; `+await_+`1`, "Identifier 'qaFastConst' has already been declared")
	requireExec(t, svc, `const qaAsyncConst = 'ac'; `+await_+`1`, float64(1))
	requireExecError(t, svc, `const qaAsyncConst = 'x'`, "Identifier 'qaAsyncConst' has already been declared")
	requireExec(t, svc, `qaAsyncConst`, "ac")

	requireExecError(t, svc, `let qaFailLet = (() => { throw new Error('initfail') })(); 1`, "initfail")
	requireExecError(t, svc, `let qaFailLet = 2; 2`, "Identifier 'qaFailLet' has already been declared")
	requireExecError(t, svc, `qaWriteTdz = 1; let qaWriteTdz = 2`, "before initialization")
	requireExecError(t, svc, `qaWriteTdz = 3`, "before initialization")
	requireExecError(t, svc, `qaConstWriteTdz = 1; const qaConstWriteTdz = 2`, "before initialization")
	requireExecError(t, svc, `const qaFailConst = (() => { throw new Error('constinitfail') })()`, "constinitfail")
	requireExecError(t, svc, `qaFailConst = 7`, "before initialization")

	requireExec(t, svc, `const qaInitEscape = globalThis[Object.getOwnPropertyNames(globalThis).find(name => name.startsWith('__browser_repl_init_'))]`, nil)
	requireExecError(t, svc, `qaInitEscape.qaConst = 2`, "revoked")
	requireExec(t, svc, `typeof globalThis["__browser_repl_init_target"]`, "undefined")
	requireExec(t, svc, `repl.id`, svc.browserRepl.id)
}

func TestBrowserReplTrailingBlockResult(t *testing.T) {
	svc := newBrowserReplSvc(t)

	r := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{
		Code: `try { await new Promise(r => setTimeout(r, 1)); 'after' } catch { 'caught' }`,
	})
	require.True(t, r.Success, "error: %v", r.Error)
	require.NotNil(t, r.ResultRepr)
	require.Equal(t, "undefined", *r.ResultRepr)

	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `try { 42 } catch {}`})
	require.True(t, r.Success, "error: %v", r.Error)
	require.Equal(t, "undefined", *r.ResultRepr)

	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{
		Code: `await new Promise(r => setTimeout(r, 1)); 'tail'`,
	})
	require.True(t, r.Success, "error: %v", r.Error)
	require.Equal(t, "tail", r.Result)
}

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

	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `await scroll(100, 100, 0, 700); "done2"`})
	require.True(t, r.Success, "error: %v", r.Error)
	require.Eventually(t, func() bool { return scrollY() == 1400 }, 3*time.Second, 20*time.Millisecond,
		"the second scroll must move the offset by exactly one delta")
	require.Equal(t, 2, wheelCount())
}

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

func TestBrowserReplOrphanedDaemonKilled(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("orphan detection requires /proc")
	}
	script := ensureBrowserReplBundle(t)
	svc := newBrowserReplSvc(t)
	socketPath := browserReplSocketPath()

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

	r := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "1 + 1"})
	require.True(t, r.Success, "error: %v", r.Error)
	require.Equal(t, float64(2), r.Result)
	require.NotEqual(t, "rogue0000000000000000000", r.ReplId)

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

	r2 := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "repl.id"})
	require.True(t, r2.Success)
	require.Equal(t, r.ReplId, r2.Result)
}

func TestBrowserReplOrphanedDaemonKilledOnChildDeath(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("orphan detection requires /proc")
	}
	script := ensureBrowserReplBundle(t)
	svc := newBrowserReplSvc(t)
	socketPath := browserReplSocketPath()

	r := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "1 + 1"})
	require.True(t, r.Success, "error: %v", r.Error)

	svc.browserReplMu.Lock()
	child := svc.browserRepl
	svc.browserReplMu.Unlock()
	require.NotNil(t, child)
	require.NoError(t, child.cmd.Process.Kill())
	time.Sleep(300 * time.Millisecond)

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

	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "1 + 1"})
	require.True(t, r.Success, "error: %v", r.Error)
	require.Equal(t, float64(2), r.Result)
	require.NotEqual(t, "rogue0000000000000000000", r.ReplId)

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

func TestStrictBrowserExecuteBodyMiddleware(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	})
	handler := StrictBrowserExecuteBodyMiddleware(next)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/browser/execute", strings.NewReader(`{"code":"1","bogus":1}`)))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), `unknown field \"bogus\"`)

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/browser/execute", strings.NewReader(`{"code":"1","timeout_sec":5,"reset":false}`)))
	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, `{"code":"1","timeout_sec":5,"reset":false}`, rec.Body.String())

	rec = httptest.NewRecorder()
	huge := `{"code":"` + strings.Repeat("x", maxBrowserExecuteBodyBytes) + `"}`
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/browser/execute", strings.NewReader(huge)))
	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	require.Contains(t, rec.Body.String(), "request body exceeds")

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/browser/execute", strings.NewReader(`{nope`)))
	require.Equal(t, http.StatusOK, rec.Code)

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/browser/execute", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/playwright/execute", strings.NewReader(`{"code":"1","bogus":1}`)))
	require.Equal(t, http.StatusOK, rec.Code)
}
