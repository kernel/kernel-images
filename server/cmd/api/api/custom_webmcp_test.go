package api

import (
	"context"
	"testing"

	"github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/require"
)

const customWebMCPTestSource = `
customTools.register({
  id: "test/read-title",
  kind: "cdp",
  match: {url_patterns: ["https://example.com/*"]},
  tool: {
    name: "read_title",
    description: "Read the current page title.",
    inputSchema: {type: "object", additionalProperties: false},
    annotations: {readOnlyHint: true},
  },
  outputSchema: {
    type: "object",
    properties: {title: {type: "string"}},
    required: ["title"],
    additionalProperties: false,
  },
  execute: async () => ({title: await js(() => document.title)}),
});
`

func TestCustomWebMCPRegistryUsesBrowserReplLifecycle(t *testing.T) {
	svc := newBrowserReplSvc(t)
	ctx := context.Background()

	replaced, err := svc.ReplaceCustomWebMCPTools(ctx, oapi.ReplaceCustomWebMCPToolsRequestObject{
		Body: &oapi.ReplaceCustomWebMCPToolsJSONRequestBody{Source: customWebMCPTestSource},
	})
	require.NoError(t, err)
	body := replaced.(oapi.ReplaceCustomWebMCPTools200JSONResponse)
	require.NotEmpty(t, body.ReplId)
	require.Equal(t, 1, body.Revision)
	require.Equal(t, customWebMCPTestSource, body.Source)
	require.Len(t, body.Tools, 1)
	require.Equal(t, "test/read-title", body.Tools[0].Id)
	require.Equal(t, "cdp", body.Tools[0].Kind)
	require.Equal(t, []string{"https://example.com/*"}, body.Tools[0].Match.UrlPatterns)
	require.Empty(t, body.Installations)

	got, err := svc.GetCustomWebMCPTools(ctx, oapi.GetCustomWebMCPToolsRequestObject{})
	require.NoError(t, err)
	current := got.(oapi.GetCustomWebMCPTools200JSONResponse)
	require.Equal(t, body.ReplId, current.ReplId)
	require.Equal(t, body.Revision, current.Revision)
	require.Equal(t, body.Tools, current.Tools)

	replaced, err = svc.ReplaceCustomWebMCPTools(ctx, oapi.ReplaceCustomWebMCPToolsRequestObject{
		Body: &oapi.ReplaceCustomWebMCPToolsJSONRequestBody{Source: customWebMCPTestSource},
	})
	require.NoError(t, err)
	second := replaced.(oapi.ReplaceCustomWebMCPTools200JSONResponse)
	require.Equal(t, 2, second.Revision)
	require.Equal(t, 2, second.Tools[0].Revision)

	reset := true
	resetResponse := executeBrowserRepl(t, svc, &oapi.ExecuteBrowserReplJSONRequestBody{Code: "", Reset: &reset})
	require.NotEqual(t, body.ReplId, resetResponse.ReplId)
	got, err = svc.GetCustomWebMCPTools(ctx, oapi.GetCustomWebMCPToolsRequestObject{})
	require.NoError(t, err)
	fresh := got.(oapi.GetCustomWebMCPTools200JSONResponse)
	require.Equal(t, resetResponse.ReplId, fresh.ReplId)
	require.Zero(t, fresh.Revision)
	require.Empty(t, fresh.Tools)
}

func TestCustomWebMCPInvalidReplacementPreservesRegistry(t *testing.T) {
	svc := newBrowserReplSvc(t)
	ctx := context.Background()

	_, err := svc.ReplaceCustomWebMCPTools(ctx, oapi.ReplaceCustomWebMCPToolsRequestObject{
		Body: &oapi.ReplaceCustomWebMCPToolsJSONRequestBody{Source: customWebMCPTestSource},
	})
	require.NoError(t, err)

	response, err := svc.ReplaceCustomWebMCPTools(ctx, oapi.ReplaceCustomWebMCPToolsRequestObject{
		Body: &oapi.ReplaceCustomWebMCPToolsJSONRequestBody{Source: `customTools.register({id: "broken"})`},
	})
	require.NoError(t, err)
	_, ok := response.(oapi.ReplaceCustomWebMCPTools400JSONResponse)
	require.True(t, ok, "expected 400 response, got %T", response)

	got, err := svc.GetCustomWebMCPTools(ctx, oapi.GetCustomWebMCPToolsRequestObject{})
	require.NoError(t, err)
	current := got.(oapi.GetCustomWebMCPTools200JSONResponse)
	require.Len(t, current.Tools, 1)
	require.Equal(t, "test/read-title", current.Tools[0].Id)
}
