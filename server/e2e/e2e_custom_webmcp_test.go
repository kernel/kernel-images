package e2e

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	instanceoapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testCustomWebMCPTargetBinding(t *testing.T, ctx context.Context, client *instanceoapi.ClientWithResponses) {
	t.Helper()

	var urls []string
	executeWebMCPPlaywright(t, ctx, client, `
		await context.route('http://127.0.0.1:10001/target-binding/*', route => {
			const marker = route.request().url().endsWith('/target-binding/owner') ? 'owner' : 'other';
			return route.fulfill({contentType: 'text/html', body: '<body data-marker="' + marker + '">' + marker + '</body>'});
		});
		await page.goto('http://127.0.0.1:10001/target-binding/owner');
		const other = await context.newPage();
		await other.goto('http://127.0.0.1:10001/target-binding/other');
		return [page.url(), other.url()];
	`, &urls)
	require.Equal(t, []string{
		"http://127.0.0.1:10001/target-binding/owner",
		"http://127.0.0.1:10001/target-binding/other",
	}, urls)

	added, err := client.AddCustomWebMCPToolsWithResponse(ctx, instanceoapi.AddCustomWebMCPToolsJSONRequestBody{
		Namespace: "target-binding.test",
		Source: `[{kind:'cdp', match:{url_patterns:['http://127.0.0.1:10001/target-binding/owner']},
			tool:{name:'mark_registered_page',description:'Mark the registering page.',inputSchema:{type:'object'}},
			execute:async ()=>await js(()=>{
				document.body.dataset.probe='touched';
				return {marker:document.body.dataset.marker,url:location.href};
			})}]`,
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, added.StatusCode(), "%s", added.Body)

	var ref string
	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		tools, err := client.GetWebMCPToolsWithResponse(ctx, &instanceoapi.GetWebMCPToolsParams{})
		if !assert.NoError(collect, err) || !assert.Equal(collect, http.StatusOK, tools.StatusCode()) || tools.JSON200 == nil {
			return
		}
		for _, tool := range tools.JSON200.Tools {
			if tool.Tool.Name == "mark_registered_page" {
				ref = tool.ToolRef
				assert.Equal(collect, urls[0], tool.Source.PageUrl)
				return
			}
		}
		assert.Fail(collect, "tool not registered on owner tab")
	}, 10*time.Second, 200*time.Millisecond)

	selected, err := client.ExecuteBrowserReplWithResponse(ctx, instanceoapi.ExecuteBrowserReplJSONRequestBody{
		Code: `const unrelated = (await listTabs()).find(tab => tab.url.endsWith('/target-binding/other'));
			if (!unrelated) throw new Error('unrelated tab not found');
			await switchTab(unrelated.targetId);
			if ((await currentTab()).url !== unrelated.url) throw new Error('unrelated tab not attached');`,
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, selected.StatusCode(), "%s", selected.Body)
	require.NotNil(t, selected.JSON200)
	require.True(t, selected.JSON200.Success, "%s", selected.Body)

	timeout := 15
	result, err := client.InvokeWebMCPToolWithResponse(ctx, instanceoapi.WebMCPInvokeRequest{
		ToolRef: ref, Input: map[string]any{}, TimeoutSec: &timeout,
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, result.StatusCode(), "%s", result.Body)
	require.NotNil(t, result.JSON200)
	require.Equal(t, instanceoapi.WebMCPInvocationResultStatusCompleted, result.JSON200.Status)
	require.Equal(t, map[string]any{"marker": "owner", "url": urls[0]}, result.JSON200.Output)

	attached, err := client.ExecuteBrowserReplWithResponse(ctx, instanceoapi.ExecuteBrowserReplJSONRequestBody{
		Code: `const current = await currentTab();
			if (!current.url.endsWith('/target-binding/other'))
				throw new Error('custom tool changed attached tab to ' + current.url);`,
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, attached.StatusCode(), "%s", attached.Body)
	require.NotNil(t, attached.JSON200)
	require.True(t, attached.JSON200.Success, "%s", attached.Body)

	var state struct {
		Owner string `json:"owner"`
		Other string `json:"other"`
	}
	executeWebMCPPlaywright(t, ctx, client, `
		const pages = context.pages();
		return {
			owner: await pages.find(candidate => candidate.url().endsWith('/target-binding/owner')).evaluate(() => document.body.dataset.probe || ''),
			other: await pages.find(candidate => candidate.url().endsWith('/target-binding/other')).evaluate(() => document.body.dataset.probe || ''),
		};
	`, &state)
	require.Equal(t, "touched", state.Owner)
	require.Empty(t, state.Other)
}

func testCustomWebMCPSlowPage(t *testing.T, ctx context.Context, client *instanceoapi.ClientWithResponses) {
	t.Helper()
	patched, err := client.ExecuteBrowserReplWithResponse(ctx, instanceoapi.ExecuteBrowserReplJSONRequestBody{
		Code: `const owner = (await listTabs()).find(tab => tab.url.endsWith('/target-binding/owner'));
			await switchTab(owner.targetId);
			const tree = await cdp('Page.getFrameTree');
			await cdp('Runtime.enable');
			const candidates = [...new Set((await drainEvents()).filter(event =>
				event.method === 'Runtime.executionContextCreated' &&
				event.params.context.name === 'kernel-custom-webmcp' &&
				event.params.context.auxData?.frameId === tree.frameTree.frame.id
			).map(event => event.params.context.id))];
			let worldContextId;
			for (const id of candidates) {
				try {
					const state = await cdp('Runtime.evaluate', {contextId:id,
						expression:'Boolean(globalThis.__kernelCustomWebMCP)',returnByValue:true});
					if (state.result?.value) worldContextId=id;
				} catch (error) {
					if (!String(error).includes('Cannot find context')) throw error;
				}
			}
			if (!worldContextId) throw new Error('custom tool isolated world not found');
			const patched = await cdp('Runtime.evaluate', {contextId: worldContextId, expression: ` + "`" + `
				const prototype = Object.getPrototypeOf(document.modelContext);
				const register = prototype.registerTool;
				Object.defineProperty(prototype, 'registerTool', {configurable: true, value: function(tool, ...args) {
					if (tool.name.endsWith('.slow_page')) globalThis.__slowPageTool = tool.execute;
					return register.call(this, tool, ...args);
				}});
				document.addEventListener('kernel-run-slow', () => {
					document.body.dataset.slowStarted = 'true';
					Promise.resolve(globalThis.__slowPageTool({})).then(
						() => document.body.dataset.slowDone = 'true',
						error => document.body.dataset.slowDone = String(error)
					);
				});
			` + "`" + `});
			if (patched.exceptionDetails) throw new Error(JSON.stringify(patched.exceptionDetails));`,
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, patched.StatusCode(), "%s", patched.Body)
	require.True(t, patched.JSON200.Success, "%s", patched.Body)

	added, err := client.AddCustomWebMCPToolsWithResponse(ctx, instanceoapi.AddCustomWebMCPToolsJSONRequestBody{
		Namespace: "slow-page.test",
		Source: `[{kind:'cdp', match:{url_patterns:['http://127.0.0.1:10001/target-binding/owner']},
			tool:{name:'slow_page',description:'Wait in a page invocation.',inputSchema:{type:'object'}},
			execute:async ()=>{await waitMs(6000);return {done:true}}}]`,
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, added.StatusCode(), "%s", added.Body)
	defer func() {
		listed, err := client.ListCustomWebMCPToolsWithResponse(ctx)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, listed.StatusCode(), "%s", listed.Body)
		for _, tool := range listed.JSON200.Tools {
			if tool.Namespace != "target-binding.test" && tool.Namespace != "slow-page.test" {
				continue
			}
			removed, err := client.RemoveCustomWebMCPToolWithResponse(ctx, tool.Id)
			require.NoError(t, err)
			require.Equal(t, http.StatusNoContent, removed.StatusCode(), "%s", removed.Body)
		}
		executeWebMCPPlaywright(t, ctx, client, `
			for (const candidate of context.pages()) {
				if (candidate.url().includes('/target-binding/')) await candidate.close();
			}
			return true;
		`, new(bool))
	}()

	before, err := client.ExecuteBrowserReplWithResponse(ctx, instanceoapi.ExecuteBrowserReplJSONRequestBody{
		Code: `repl.write('before page invocation');`,
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, before.StatusCode(), "%s", before.Body)
	require.True(t, before.JSON200.Success, "%s", before.Body)

	tools, err := client.GetWebMCPToolsWithResponse(ctx, &instanceoapi.GetWebMCPToolsParams{})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, tools.StatusCode(), "%s", tools.Body)
	var markRef string
	for _, tool := range tools.JSON200.Tools {
		if tool.Tool.Name == "mark_registered_page" {
			markRef = tool.ToolRef
			break
		}
	}
	require.NotEmpty(t, markRef)

	executeWebMCPPlaywright(t, ctx, client, `
		const owner = context.pages().find(candidate => candidate.url().endsWith('/target-binding/owner'));
		await owner.evaluate(() => document.dispatchEvent(new Event('kernel-run-slow')));
		await owner.waitForFunction(() => document.body.dataset.slowStarted === 'true');
		return owner.url();
	`, new(string))

	short := 1
	cell, err := client.ExecuteBrowserReplWithResponse(ctx, instanceoapi.ExecuteBrowserReplJSONRequestBody{
		Code: `repl.write('should not run on the wrong tab');`, TimeoutSec: &short,
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, cell.StatusCode(), "%s", cell.Body)
	require.NotNil(t, cell.JSON200)
	require.False(t, cell.JSON200.Success, "%s", cell.Body)
	require.Contains(t, *cell.JSON200.Error, "in progress")
	if cell.JSON200.ReplTerminated != nil {
		require.False(t, *cell.JSON200.ReplTerminated, "%s", cell.Body)
	}
	require.Equal(t, before.JSON200.ReplId, cell.JSON200.ReplId)

	invoked, err := client.InvokeWebMCPToolWithResponse(ctx, instanceoapi.WebMCPInvokeRequest{
		ToolRef: markRef, Input: map[string]any{}, TimeoutSec: &short,
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, invoked.StatusCode(), "%s", invoked.Body)
	require.NotNil(t, invoked.JSON200)
	require.Equal(t, instanceoapi.WebMCPInvocationResultStatusError, invoked.JSON200.Status)
	require.Contains(t, *invoked.JSON200.ErrorText, "in progress")

	var done string
	executeWebMCPPlaywright(t, ctx, client, `
		const owner = context.pages().find(candidate => candidate.url().endsWith('/target-binding/owner'));
		await owner.waitForFunction(() => document.body.dataset.slowDone !== undefined);
		return owner.evaluate(() => document.body.dataset.slowDone);
	`, &done)
	require.Equal(t, "true", done)
	after, err := client.ExecuteBrowserReplWithResponse(ctx, instanceoapi.ExecuteBrowserReplJSONRequestBody{Code: `repl.write('still alive');`})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, after.StatusCode(), "%s", after.Body)
	require.Equal(t, before.JSON200.ReplId, after.JSON200.ReplId)
	require.True(t, after.JSON200.Success, "%s", after.Body)
}

func testCustomWebMCPInvokesAcrossNavigation(t *testing.T, ctx context.Context, client *instanceoapi.ClientWithResponses) {
	t.Helper()

	executeWebMCPPlaywright(t, ctx, client, `
		await page.route('http://127.0.0.1:10001/fixture/**', route =>
			route.fulfill({contentType: 'text/html', body: '<title>Flight results</title><main>Flight 123</main>'}));
		await page.goto('http://127.0.0.1:10001/fixture/start');
		return page.url();
	`, new(string))

	added, err := client.AddCustomWebMCPToolsWithResponse(ctx, instanceoapi.AddCustomWebMCPToolsJSONRequestBody{
		Namespace: "navigation.test",
		Source: `[{kind:'cdp', match:{url_patterns:['http://127.0.0.1:10001/fixture/*']},
			tool:{name:'search_flights',description:'Search flights.',
				inputSchema:{type:'object',properties:{origin:{type:'string'}},required:['origin']},
				outputSchema:{type:'object',properties:{origin:{type:'string'},url:{type:'string'}},required:['origin','url']}},
			execute:async (input,{matches})=>{
				await switchTab(matches[0].top_target_id);
				await gotoUrl('http://127.0.0.1:10001/fixture/results');
				await waitForLoad();
				return {origin:input.origin,url:await js(() => location.href)};
			}}]`,
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, added.StatusCode(), "%s", added.Body)

	var ref string
	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		tools, err := client.GetWebMCPToolsWithResponse(ctx, &instanceoapi.GetWebMCPToolsParams{})
		if !assert.NoError(collect, err) || !assert.Equal(collect, http.StatusOK, tools.StatusCode()) || tools.JSON200 == nil {
			return
		}
		for _, tool := range tools.JSON200.Tools {
			if tool.Tool.Name == "search_flights" {
				ref = tool.ToolRef
				return
			}
		}
		assert.Fail(collect, "custom tool not discovered")
	}, 10*time.Second, 200*time.Millisecond)

	timeout := 10
	result, err := client.InvokeWebMCPToolWithResponse(ctx, instanceoapi.WebMCPInvokeRequest{
		ToolRef: ref, Input: map[string]any{"origin": "SFO"}, TimeoutSec: &timeout,
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, result.StatusCode(), "%s", result.Body)
	require.NotNil(t, result.JSON200)
	require.Equal(t, instanceoapi.WebMCPInvocationResultStatusCompleted, result.JSON200.Status)
	require.Equal(t, map[string]any{"origin": "SFO", "url": "http://127.0.0.1:10001/fixture/results"}, result.JSON200.Output)

	tools, err := client.GetWebMCPToolsWithResponse(ctx, &instanceoapi.GetWebMCPToolsParams{})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, tools.StatusCode())
	require.NotNil(t, tools.JSON200)
	require.Len(t, tools.JSON200.Tools, 1)
	invalid, err := client.InvokeWebMCPToolWithResponse(ctx, instanceoapi.WebMCPInvokeRequest{
		ToolRef: tools.JSON200.Tools[0].ToolRef, Input: map[string]any{"origin": 123}, TimeoutSec: &timeout,
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, invalid.StatusCode(), "%s", invalid.Body)
	require.NotNil(t, invalid.JSON200)
	require.Equal(t, instanceoapi.WebMCPInvocationResultStatusError, invalid.JSON200.Status)
	require.Contains(t, *invalid.JSON200.ErrorText, "input failed JSON Schema validation")

	replTimeout := 20
	replResult, err := client.ExecuteBrowserReplWithResponse(ctx, instanceoapi.ExecuteBrowserReplJSONRequestBody{
		Code: fmt.Sprintf(`const result = await webmcp.invokeTool(%q, {origin: 'SFO'}, {timeoutSec: 10});
			if (result.status !== 'completed' || result.output.url !== 'http://127.0.0.1:10001/fixture/results')
				throw new Error(JSON.stringify(result));
			repl.write(JSON.stringify(result));`, tools.JSON200.Tools[0].ToolRef),
		TimeoutSec: &replTimeout,
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, replResult.StatusCode(), "%s", replResult.Body)
	require.NotNil(t, replResult.JSON200)
	require.True(t, replResult.JSON200.Success, "%s", replResult.Body)

	oversized, err := client.AddCustomWebMCPToolsWithResponse(ctx, instanceoapi.AddCustomWebMCPToolsJSONRequestBody{
		Namespace: "large-result.test",
		Source: `[{kind:'cdp', match:{url_patterns:['http://127.0.0.1:10001/fixture/*']},
			tool:{name:'large_result',description:'Return a large result.',inputSchema:{type:'object'}},
			execute:async ()=>({payload:'x'.repeat(250*1024)})}]`,
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, oversized.StatusCode(), "%s", oversized.Body)
	var largeRef string
	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		tools, err := client.GetWebMCPToolsWithResponse(ctx, &instanceoapi.GetWebMCPToolsParams{})
		if !assert.NoError(collect, err) || !assert.Equal(collect, http.StatusOK, tools.StatusCode()) || tools.JSON200 == nil {
			return
		}
		for _, tool := range tools.JSON200.Tools {
			if tool.Tool.Name == "large_result" {
				largeRef = tool.ToolRef
				return
			}
		}
		assert.Fail(collect, "large result tool not discovered")
	}, 10*time.Second, 200*time.Millisecond)
	large, err := client.InvokeWebMCPToolWithResponse(ctx, instanceoapi.WebMCPInvokeRequest{
		ToolRef: largeRef, Input: map[string]any{}, TimeoutSec: &timeout,
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, large.StatusCode(), "%s", large.Body)
	require.NotNil(t, large.JSON200)
	require.Equal(t, instanceoapi.WebMCPInvocationResultStatusError, large.JSON200.Status)
	require.Contains(t, *large.JSON200.ErrorText, "output exceeds 240 KiB")

	require.NotNil(t, tools.JSON200.Tools[0].Source.TargetId)
	targetID := *tools.JSON200.Tools[0].Source.TargetId
	patched, err := client.ExecuteBrowserReplWithResponse(ctx, instanceoapi.ExecuteBrowserReplJSONRequestBody{
		Code: fmt.Sprintf(`await js(() => {
			const prototype = Object.getPrototypeOf(document.modelContext);
			const original = prototype.registerTool;
			window.__allowCustomRegistration = false;
			Object.defineProperty(prototype, 'registerTool', {configurable: true, value: function(...args) {
				if (!window.__allowCustomRegistration) throw new Error('registration temporarily unavailable');
				return original.apply(this, args);
			}});
		}, {targetId: %q});`, targetID),
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, patched.StatusCode(), "%s", patched.Body)
	require.True(t, patched.JSON200.Success, "%s", patched.Body)
	failed, err := client.AddCustomWebMCPToolsWithResponse(ctx, instanceoapi.AddCustomWebMCPToolsJSONRequestBody{
		Namespace: "recovery.test",
		Source: `[{kind:'page', match:{url_patterns:['http://127.0.0.1:10001/fixture/*']},
			tool:{name:'recover_registration',description:'Return the input.',inputSchema:{type:'object'}},
			execute:async input=>input}]`,
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, failed.StatusCode(), "%s", failed.Body)
	beforeRecovery, err := client.GetWebMCPToolsWithResponse(ctx, &instanceoapi.GetWebMCPToolsParams{})
	require.NoError(t, err)
	for _, tool := range beforeRecovery.JSON200.Tools {
		require.NotEqual(t, "recover_registration", tool.Tool.Name)
	}
	restored, err := client.ExecuteBrowserReplWithResponse(ctx, instanceoapi.ExecuteBrowserReplJSONRequestBody{
		Code: fmt.Sprintf(`await js(() => { window.__allowCustomRegistration = true; }, {targetId: %q});`, targetID),
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, restored.StatusCode(), "%s", restored.Body)
	require.True(t, restored.JSON200.Success, "%s", restored.Body)
	var recoveredRef string
	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		tools, err := client.GetWebMCPToolsWithResponse(ctx, &instanceoapi.GetWebMCPToolsParams{})
		if !assert.NoError(collect, err) || !assert.Equal(collect, http.StatusOK, tools.StatusCode()) || tools.JSON200 == nil {
			return
		}
		for _, tool := range tools.JSON200.Tools {
			if tool.Tool.Name == "recover_registration" {
				recoveredRef = tool.ToolRef
				return
			}
		}
		assert.Fail(collect, "registration did not recover in the same document")
	}, 10*time.Second, 200*time.Millisecond)
	recovered, err := client.InvokeWebMCPToolWithResponse(ctx, instanceoapi.WebMCPInvokeRequest{
		ToolRef: recoveredRef, Input: map[string]any{"origin": "SFO"}, TimeoutSec: &timeout,
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, recovered.StatusCode(), "%s", recovered.Body)
	require.Equal(t, map[string]any{"origin": "SFO"}, recovered.JSON200.Output)

	pageResult, err := client.ExecuteBrowserReplWithResponse(ctx, instanceoapi.ExecuteBrowserReplJSONRequestBody{
		Code: `const pageTool = (await webmcp.listTools()).find(tool => tool.tool.name === 'recover_registration');
			const nativeResult = await webmcp.invokeTool(pageTool.tool_ref, {origin: 'SFO'});
			if (nativeResult.status !== 'completed' || nativeResult.output.origin !== 'SFO') throw new Error(JSON.stringify(nativeResult));`,
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, pageResult.StatusCode(), "%s", pageResult.Body)
	require.True(t, pageResult.JSON200.Success, "%s", pageResult.Body)

	slow, err := client.AddCustomWebMCPToolsWithResponse(ctx, instanceoapi.AddCustomWebMCPToolsJSONRequestBody{
		Namespace: "deadline.test",
		Source: `[{kind:'cdp', match:{url_patterns:['http://127.0.0.1:10001/fixture/*']},
			tool:{name:'slow_result',description:'Wait before returning.',inputSchema:{type:'object'}},
			execute:async ()=>{await waitMs(3500);return {done:true}}}]`,
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, slow.StatusCode(), "%s", slow.Body)
	var slowRef string
	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		tools, err := client.GetWebMCPToolsWithResponse(ctx, &instanceoapi.GetWebMCPToolsParams{})
		if !assert.NoError(collect, err) || !assert.Equal(collect, http.StatusOK, tools.StatusCode()) || tools.JSON200 == nil {
			return
		}
		for _, tool := range tools.JSON200.Tools {
			if tool.Tool.Name == "slow_result" {
				slowRef = tool.ToolRef
				return
			}
		}
		assert.Fail(collect, "slow result tool not discovered")
	}, 10*time.Second, 200*time.Millisecond)

	shortTimeout := 3
	deadline, err := client.ExecuteBrowserReplWithResponse(ctx, instanceoapi.ExecuteBrowserReplJSONRequestBody{
		Code: fmt.Sprintf(`try {
			await webmcp.invokeTool(%q, {}, {timeoutSec:30});
			throw new Error('expected a deadline error');
		} catch (error) {
			if (error.code !== 'outcome_unknown') throw error;
			repl.write('deadline guarded');
		}`, slowRef),
		TimeoutSec: &shortTimeout,
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, deadline.StatusCode(), "%s", deadline.Body)
	require.NotNil(t, deadline.JSON200)
	require.True(t, deadline.JSON200.Success, "%s", deadline.Body)
	require.Nil(t, deadline.JSON200.ReplTerminated)

	oneSecond := 1
	timed, err := client.InvokeWebMCPToolWithResponse(ctx, instanceoapi.WebMCPInvokeRequest{
		ToolRef: slowRef, Input: map[string]any{}, TimeoutSec: &oneSecond,
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusGatewayTimeout, timed.StatusCode(), "%s", timed.Body)
	require.NotNil(t, timed.JSON504)
	require.Equal(t, instanceoapi.OutcomeUnknown, timed.JSON504.Code)
	listed, err := client.ListCustomWebMCPToolsWithResponse(ctx)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, listed.StatusCode(), "%s", listed.Body)
	require.NotNil(t, listed.JSON200)
	require.NotEmpty(t, listed.JSON200.Tools)
}
