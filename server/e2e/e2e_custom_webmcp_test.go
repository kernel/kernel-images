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
}
