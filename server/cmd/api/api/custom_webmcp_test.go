package api

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/kernel/kernel-images/server/lib/webmcpclient"
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

func requireCustomToolsSnapshot(t *testing.T, manager *browserReplManager) map[string]oapi.CustomWebMCPDefinition {
	t.Helper()
	tools, err := manager.customToolsSnapshot()
	require.NoError(t, err)
	return tools
}

func TestCustomCDPInvocationNotDispatchedWhileReplIsBusy(t *testing.T) {
	manager := newBrowserReplManager()
	require.NoError(t, manager.acquire(context.Background()))
	defer manager.release()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := manager.invokeCustomCDPTool(ctx, "ct_test", "target", map[string]any{}, time.Second)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NotErrorIs(t, err, webmcpclient.ErrOutcomeUnknown)
}

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
	require.Regexp(t, `^ct_[a-z][a-z0-9]{23}$`, body.Tools[0].Id)
	require.Equal(t, "example.com", body.Tools[0].Namespace)
	require.Equal(t, "cdp", body.Tools[0].Kind)
	require.Equal(t, "read_title", body.Tools[0].Tool.Name)
	require.Equal(t, []string{"https://example.com/*"}, body.Tools[0].Match.UrlPatterns)
	require.Equal(t, body.Tools[0], requireCustomToolsSnapshot(t, svc.browserRepl)[body.Tools[0].Id])

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

func TestCustomWebMCPSnapshotRejectsDifferentRepl(t *testing.T) {
	svc := newBrowserReplSvc(t)
	ctx := context.Background()
	added, err := svc.AddCustomWebMCPTools(ctx, oapi.AddCustomWebMCPToolsRequestObject{
		Body: &oapi.AddCustomWebMCPToolsJSONRequestBody{Namespace: "example.com", Source: customWebMCPTestSource},
	})
	require.NoError(t, err)
	tool := added.(oapi.AddCustomWebMCPTools201JSONResponse).Tools[0]
	require.Contains(t, requireCustomToolsSnapshot(t, svc.browserRepl), tool.Id)

	require.NoError(t, os.WriteFile(
		browserReplCustomToolsPath(),
		[]byte(`{"repl_id":"different-repl","tools":[]}`),
		0o600,
	))
	_, err = svc.browserRepl.customToolsSnapshot()
	require.ErrorContains(t, err, "another REPL")

	require.NoError(t, os.WriteFile(browserReplCustomToolsPath(), []byte(`{invalid`), 0o600))
	_, err = svc.browserRepl.customToolsSnapshot()
	require.ErrorContains(t, err, "decode custom WebMCP discovery snapshot")

	require.NoError(t, os.WriteFile(browserReplCustomToolsPath(), []byte(`{"repl_id":"`+svc.browserRepl.customToolsReplID+`"}`), 0o600))
	_, err = svc.browserRepl.customToolsSnapshot()
	require.ErrorContains(t, err, "no tools list")

	require.NoError(t, os.Remove(browserReplCustomToolsPath()))
	_, err = svc.browserRepl.customToolsSnapshot()
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestCustomWebMCPMetadataIsVisibleBeforeReplCellCompletes(t *testing.T) {
	svc := newBrowserReplSvc(t)
	code := fmt.Sprintf(`
		await webmcp.addCustomTools({namespace: "midcell.example", tools: %s});
		await waitMs(1500);
	`, customWebMCPTestSource)
	done := make(chan error, 1)
	go func() {
		response, err := svc.ExecuteBrowserRepl(context.Background(), oapi.ExecuteBrowserReplRequestObject{
			Body: &oapi.ExecuteBrowserReplJSONRequestBody{Code: code},
		})
		if err == nil {
			result, ok := response.(oapi.ExecuteBrowserRepl200JSONResponse)
			if !ok || !result.Success {
				err = fmt.Errorf("unexpected response: %#v", response)
			}
		}
		done <- err
	}()

	require.Eventually(t, func() bool {
		tools, err := svc.browserRepl.customToolsSnapshot()
		if err != nil {
			return false
		}
		for _, tool := range tools {
			if tool.Namespace == "midcell.example" {
				return true
			}
		}
		return false
	}, 5*time.Second, 10*time.Millisecond)
	select {
	case err := <-done:
		require.FailNow(t, "REPL cell completed before metadata became visible", "%v", err)
	default:
	}
	require.NoError(t, <-done)
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

func TestCustomWebMCPForceOverwriteNamespace(t *testing.T) {
	svc := newBrowserReplSvc(t)
	ctx := context.Background()
	add := func(namespace, source string, force bool) oapi.AddCustomWebMCPToolsResponseObject {
		response, err := svc.AddCustomWebMCPTools(ctx, oapi.AddCustomWebMCPToolsRequestObject{
			Body: &oapi.AddCustomWebMCPToolsJSONRequestBody{
				Namespace: namespace, Source: source, ForceOverwriteNamespace: &force,
			},
		})
		require.NoError(t, err)
		return response
	}
	original := add("example.com", customWebMCPTestSource, false).(oapi.AddCustomWebMCPTools201JSONResponse).Tools[0]
	second := add("example.com", strings.Replace(customWebMCPTestSource, "read_title", "read_other", 1), false).(oapi.AddCustomWebMCPTools201JSONResponse).Tools[0]
	other := add("other.example", customWebMCPTestSource, false).(oapi.AddCustomWebMCPTools201JSONResponse).Tools[0]

	// Invalid replacement must not delete either namespace.
	_, ok := add("example.com", `[{kind: "cdp"}]`, true).(oapi.AddCustomWebMCPTools400JSONResponse)
	require.True(t, ok)
	require.Contains(t, requireCustomToolsSnapshot(t, svc.browserRepl), original.Id)
	require.Contains(t, requireCustomToolsSnapshot(t, svc.browserRepl), second.Id)

	const noOutputSchema = `[{kind: "cdp", match: {url_patterns: ["https://example.com/*"]},
		tool: {name: "read_title", description: "Read title", inputSchema: {type: "object"}},
		execute: async () => ({title: "example"})}]`
	_, ok = add("example.com", noOutputSchema, false).(oapi.AddCustomWebMCPTools409JSONResponse)
	require.True(t, ok)
	replacement := add("example.com", noOutputSchema, true).(oapi.AddCustomWebMCPTools201JSONResponse).Tools[0]
	require.Regexp(t, `^ct_[a-z][a-z0-9]{23}$`, replacement.Id)
	require.NotEqual(t, original.Id, replacement.Id)
	require.Nil(t, replacement.Tool.OutputSchema)

	listed, err := svc.ListCustomWebMCPTools(ctx, oapi.ListCustomWebMCPToolsRequestObject{})
	require.NoError(t, err)
	require.ElementsMatch(t, []string{replacement.Id, other.Id}, []string{
		listed.(oapi.ListCustomWebMCPTools200JSONResponse).Tools[0].Id,
		listed.(oapi.ListCustomWebMCPTools200JSONResponse).Tools[1].Id,
	})
	require.NotContains(t, requireCustomToolsSnapshot(t, svc.browserRepl), original.Id)
	require.NotContains(t, requireCustomToolsSnapshot(t, svc.browserRepl), second.Id)
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
