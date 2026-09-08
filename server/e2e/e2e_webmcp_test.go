package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	instanceoapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testWebMCPDeclarative(t *testing.T, ctx context.Context, client *instanceoapi.ClientWithResponses) {
	t.Helper()

	// Like the audio fixture, use file:// inside the instance to provide a secure
	// context without external URLs or a Docker-only host-loopback bridge.
	for _, name := range []string{"reservation.html", "embedded.html"} {
		fixture, err := os.ReadFile("testdata/webmcp/" + name)
		require.NoError(t, err)
		rsp, err := client.WriteFileWithBodyWithResponse(ctx,
			&instanceoapi.WriteFileParams{Path: "/tmp/" + name}, "text/html", bytes.NewReader(fixture))
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, rsp.StatusCode(), "%s", rsp.Body)
	}

	for _, test := range []struct {
		name  string
		file  string
		title string
		frame bool
	}{
		{name: "top_level", file: "reservation.html", title: "Declarative reservation"},
		{name: "embedded", file: "embedded.html", title: "Embedded declarative reservation", frame: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			pageURL := "file:///tmp/" + test.file
			var secureContext bool
			executeWebMCPPlaywright(t, ctx, client, fmt.Sprintf(`
				await page.goto(%q, { waitUntil: 'load' });
				const frame = page.frames().find(frame => frame.url() === 'file:///tmp/reservation.html');
				await frame.locator('form[toolname="reserve_table"]').waitFor();
				return frame.evaluate(() => isSecureContext);
			`, pageURL), &secureContext)
			require.True(t, secureContext)

			var tool instanceoapi.WebMCPTool
			require.EventuallyWithT(t, func(collect *assert.CollectT) {
				rsp, err := client.GetWebMCPToolsWithResponse(ctx)
				if !assert.NoError(collect, err) {
					return
				}
				if !assert.Equal(collect, http.StatusOK, rsp.StatusCode(), "%s", rsp.Body) || !assert.NotNil(collect, rsp.JSON200) {
					return
				}
				for _, candidate := range rsp.JSON200.Tools {
					if candidate.Name == "reserve_table" && candidate.Source.PageUrl == pageURL {
						tool = candidate
						return
					}
				}
				assert.Fail(collect, "declarative tool not discovered", "%s", rsp.Body)
			}, 10*time.Second, 200*time.Millisecond)
			toolJSON, err := json.Marshal(tool)
			require.NoError(t, err)
			t.Logf("GET /webmcp/tools: %s", toolJSON)
			require.NotEmpty(t, tool.ToolRef)
			require.Equal(t, "Reserve a table with guest details and seating preferences.", tool.Description)
			require.Equal(t, "object", tool.InputSchema["type"])
			properties, ok := tool.InputSchema["properties"].(map[string]any)
			require.True(t, ok, "schema properties: %#v", tool.InputSchema)
			require.Len(t, properties, 4)
			for _, field := range []struct {
				name, kind, description string
			}{
				{"name", "string", "Name for the reservation"},
				{"date", "string", "Date of the reservation"},
				{"party_size", "number", "Number of guests"},
				{"seating", "string", "Preferred seating area"},
			} {
				property, ok := properties[field.name].(map[string]any)
				require.True(t, ok, "missing property %s", field.name)
				require.Equal(t, field.kind, property["type"], field.name)
				description, ok := property["description"].(string)
				require.True(t, ok, "missing description for %s", field.name)
				// Chromium appends a format hint to date descriptions.
				require.True(t, strings.HasPrefix(description, field.description), "%s: %s", field.name, description)
			}
			require.Equal(t, "date", properties["date"].(map[string]any)["format"])
			partySize := properties["party_size"].(map[string]any)
			require.EqualValues(t, 1, partySize["minimum"])
			require.EqualValues(t, 8, partySize["maximum"])
			require.ElementsMatch(t, []string{"name", "date", "party_size", "seating"}, tool.InputSchema["required"])
			require.ElementsMatch(t, []string{"dining_room", "terrace", "booth"}, properties["seating"].(map[string]any)["enum"])
			require.Equal(t, test.title, tool.Source.PageTitle)
			require.Positive(t, tool.Source.TabId)
			require.Positive(t, tool.Source.WindowId)
			if test.frame {
				require.NotNil(t, tool.Source.Frame)
				require.Positive(t, tool.Source.Frame.FrameId)
				require.Equal(t, "file:///tmp/reservation.html", tool.Source.Frame.Url)
			} else {
				require.Nil(t, tool.Source.Frame)
			}
			require.NotNil(t, tool.Annotations)
			require.True(t, tool.Annotations.Autosubmit)

			timeout := 10
			rsp, err := client.InvokeWebMCPToolWithResponse(ctx, instanceoapi.WebMCPInvokeRequest{
				ToolRef:    tool.ToolRef,
				Input:      map[string]any{"name": "Test Guest", "date": "2030-06-15", "party_size": 4, "seating": "terrace"},
				TimeoutSec: &timeout,
			})
			require.NoError(t, err)
			t.Logf("POST /webmcp/invoke: %s", rsp.Body)
			require.Equal(t, http.StatusOK, rsp.StatusCode(), "%s", rsp.Body)
			require.NotNil(t, rsp.JSON200)
			require.Equal(t, instanceoapi.WebMCPInvocationResultStatusCompleted, rsp.JSON200.Status)
			require.NotEmpty(t, rsp.JSON200.InvocationId)
			require.Nil(t, rsp.JSON200.ErrorText)
			values := map[string]any{"name": "Test Guest", "date": "2030-06-15", "party_size": "4", "seating": "terrace"}
			require.Equal(t, values, rsp.JSON200.Output)

			var state struct {
				Values       map[string]any `json:"values"`
				Confirmation string         `json:"confirmation"`
				AgentInvoked string         `json:"agentInvoked"`
				Submissions  string         `json:"submissions"`
				Visible      bool           `json:"visible"`
			}
			executeWebMCPPlaywright(t, ctx, client, `
				const frame = page.frames().find(frame => frame.url() === 'file:///tmp/reservation.html');
				const state = await frame.evaluate(() => {
					const confirmation = document.querySelector('#confirmation');
					return {
						values: Object.fromEntries(new FormData(document.querySelector('form'))),
						confirmation: confirmation.textContent,
						agentInvoked: confirmation.dataset.agentInvoked,
						submissions: confirmation.dataset.submissions,
					};
				});
				return { ...state, visible: await frame.locator('#confirmation').isVisible() };
			`, &state)
			require.Equal(t, values, state.Values)
			require.Equal(t, "Reserved for Test Guest on 2030-06-15: 4 guests, terrace.", state.Confirmation)
			require.Equal(t, "true", state.AgentInvoked)
			require.Equal(t, "1", state.Submissions)
			require.True(t, state.Visible)
		})
	}
}

func executeWebMCPPlaywright(t *testing.T, ctx context.Context, client *instanceoapi.ClientWithResponses, code string, result any) {
	t.Helper()
	rsp, err := client.ExecutePlaywrightCodeWithResponse(ctx, instanceoapi.ExecutePlaywrightCodeJSONRequestBody{Code: code})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rsp.StatusCode(), "%s", rsp.Body)
	require.NotNil(t, rsp.JSON200)
	require.True(t, rsp.JSON200.Success, "%s", rsp.Body)
	data, err := json.Marshal(rsp.JSON200.Result)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, result))
}
