package e2e

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"testing"
	"time"

	instanceoapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCustomWebMCPInvokesAcrossNavigation(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker not available: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	c := NewTestContainer(t, headfulImage)
	require.NoError(t, c.Start(ctx, ContainerConfig{Env: map[string]string{
		"CHROMIUM_FLAGS": "--enable-features=WebMCPTesting,DevToolsWebMCPSupport",
	}}))
	defer c.Stop(ctx)
	require.NoError(t, c.WaitReady(ctx))
	client, err := c.APIClient()
	require.NoError(t, err)

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
	tools, err = client.GetWebMCPToolsWithResponse(ctx, &instanceoapi.GetWebMCPToolsParams{})
	require.NoError(t, err)
	var largeRef string
	for _, tool := range tools.JSON200.Tools {
		if tool.Tool.Name == "large_result" {
			largeRef = tool.ToolRef
		}
	}
	require.NotEmpty(t, largeRef)
	large, err := client.InvokeWebMCPToolWithResponse(ctx, instanceoapi.WebMCPInvokeRequest{
		ToolRef: largeRef, Input: map[string]any{}, TimeoutSec: &timeout,
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, large.StatusCode(), "%s", large.Body)
	require.NotNil(t, large.JSON200)
	require.Equal(t, instanceoapi.WebMCPInvocationResultStatusError, large.JSON200.Status)
	require.Contains(t, *large.JSON200.ErrorText, "output exceeds 240 KiB")
}
