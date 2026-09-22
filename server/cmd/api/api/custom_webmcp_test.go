package api

import (
	"context"
	"testing"

	"github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/require"
)

const customWebMCPTestSource = `[
  {
    kind: "cdp",
    match: {url_patterns: ["https://example.com/*"]},
    tool: {
      name: "read_title",
      description: "Read the current page title.",
      inputSchema: {type: "object", additionalProperties: false},
      outputSchema: {
        type: "object",
        properties: {title: {type: "string"}},
        required: ["title"],
        additionalProperties: false,
      },
      annotations: {readOnlyHint: true},
    },
    execute: async () => ({title: await js(() => document.title)}),
  },
]`

func TestCustomWebMCPToolsUseBrowserReplLifecycle(t *testing.T) {
	svc := newBrowserReplSvc(t)
	ctx := context.Background()

	added, err := svc.AddCustomWebMCPTools(ctx, oapi.AddCustomWebMCPToolsRequestObject{
		Body: &oapi.AddCustomWebMCPToolsJSONRequestBody{
			Namespace: "example.com",
			Source:    customWebMCPTestSource,
		},
	})
	require.NoError(t, err)
	body := added.(oapi.AddCustomWebMCPTools201JSONResponse)
	require.Len(t, body.Tools, 1)
	require.Regexp(t, `^ct_[a-f0-9]{16}$`, body.Tools[0].Id)
	require.Equal(t, "example.com", body.Tools[0].Namespace)
	require.Equal(t, "cdp", body.Tools[0].Kind)
	require.Equal(t, "read_title", body.Tools[0].Tool.Name)
	require.Equal(t, []string{"https://example.com/*"}, body.Tools[0].Match.UrlPatterns)
	require.Equal(t, body.Tools[0], svc.browserRepl.customToolsSnapshot()[body.Tools[0].Id])

	listed, err := svc.ListCustomWebMCPTools(ctx, oapi.ListCustomWebMCPToolsRequestObject{})
	require.NoError(t, err)
	current := listed.(oapi.ListCustomWebMCPTools200JSONResponse)
	require.Equal(t, body.Tools, current.Tools)

	conflict, err := svc.AddCustomWebMCPTools(ctx, oapi.AddCustomWebMCPToolsRequestObject{
		Body: &oapi.AddCustomWebMCPToolsJSONRequestBody{
			Namespace: "example.com",
			Source:    customWebMCPTestSource,
		},
	})
	require.NoError(t, err)
	_, ok := conflict.(oapi.AddCustomWebMCPTools409JSONResponse)
	require.True(t, ok, "expected 409 response, got %T", conflict)

	removed, err := svc.RemoveCustomWebMCPTool(ctx, oapi.RemoveCustomWebMCPToolRequestObject{Id: body.Tools[0].Id})
	require.NoError(t, err)
	_, ok = removed.(oapi.RemoveCustomWebMCPTool204Response)
	require.True(t, ok, "expected 204 response, got %T", removed)

	listed, err = svc.ListCustomWebMCPTools(ctx, oapi.ListCustomWebMCPToolsRequestObject{})
	require.NoError(t, err)
	require.Empty(t, listed.(oapi.ListCustomWebMCPTools200JSONResponse).Tools)

	_, err = svc.AddCustomWebMCPTools(ctx, oapi.AddCustomWebMCPToolsRequestObject{
		Body: &oapi.AddCustomWebMCPToolsJSONRequestBody{
			Namespace: "example.com",
			Source:    customWebMCPTestSource,
		},
	})
	require.NoError(t, err)
	reset := true
	executeBrowserRepl(t, svc, &oapi.ExecuteBrowserReplJSONRequestBody{Code: "", Reset: &reset})
	listed, err = svc.ListCustomWebMCPTools(ctx, oapi.ListCustomWebMCPToolsRequestObject{})
	require.NoError(t, err)
	require.Empty(t, listed.(oapi.ListCustomWebMCPTools200JSONResponse).Tools)
}

func TestCustomWebMCPAllowsSameNameAcrossNamespaces(t *testing.T) {
	svc := newBrowserReplSvc(t)
	ctx := context.Background()

	for _, namespace := range []string{"one.example", "two.example"} {
		response, err := svc.AddCustomWebMCPTools(ctx, oapi.AddCustomWebMCPToolsRequestObject{
			Body: &oapi.AddCustomWebMCPToolsJSONRequestBody{
				Namespace: namespace,
				Source:    customWebMCPTestSource,
			},
		})
		require.NoError(t, err)
		added, ok := response.(oapi.AddCustomWebMCPTools201JSONResponse)
		require.True(t, ok, "expected 201 response, got %T", response)
		require.Equal(t, namespace, added.Tools[0].Namespace)
	}

	listed, err := svc.ListCustomWebMCPTools(ctx, oapi.ListCustomWebMCPToolsRequestObject{})
	require.NoError(t, err)
	require.Len(t, listed.(oapi.ListCustomWebMCPTools200JSONResponse).Tools, 2)
}

func TestCustomWebMCPInvalidAdditionPreservesTools(t *testing.T) {
	svc := newBrowserReplSvc(t)
	ctx := context.Background()

	_, err := svc.AddCustomWebMCPTools(ctx, oapi.AddCustomWebMCPToolsRequestObject{
		Body: &oapi.AddCustomWebMCPToolsJSONRequestBody{
			Namespace: "example.com",
			Source:    customWebMCPTestSource,
		},
	})
	require.NoError(t, err)

	response, err := svc.AddCustomWebMCPTools(ctx, oapi.AddCustomWebMCPToolsRequestObject{
		Body: &oapi.AddCustomWebMCPToolsJSONRequestBody{
			Namespace: "broken.example",
			Source:    `[{kind: "cdp"}]`,
		},
	})
	require.NoError(t, err)
	_, ok := response.(oapi.AddCustomWebMCPTools400JSONResponse)
	require.True(t, ok, "expected 400 response, got %T", response)

	listed, err := svc.ListCustomWebMCPTools(ctx, oapi.ListCustomWebMCPToolsRequestObject{})
	require.NoError(t, err)
	current := listed.(oapi.ListCustomWebMCPTools200JSONResponse)
	require.Len(t, current.Tools, 1)
	require.Equal(t, "read_title", current.Tools[0].Tool.Name)
}
