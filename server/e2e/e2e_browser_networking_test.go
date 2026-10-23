package e2e

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	instanceoapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/require"
)

func TestBrowserNetworkingWithoutLocalProxy(t *testing.T) {
	for _, image := range []struct {
		name string
		ref  string
	}{
		{"headful", headfulImage},
		{"headless", headlessImage},
	} {
		for _, externalProxy := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/external_proxy=%t", image.name, externalProxy), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
				defer cancel()

				fixture := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodGet || r.URL.Path != "/network-check" ||
						(externalProxy && (!r.URL.IsAbs() || r.URL.Host != "network.test")) {
						http.NotFound(w, r)
						return
					}
					w.Header().Set("Content-Type", "text/html")
					fmt.Fprint(w, "<title>network ready</title>")
				}))
				listener, err := net.Listen("tcp", "0.0.0.0:0")
				require.NoError(t, err)
				require.NoError(t, fixture.Listener.Close())
				fixture.Listener = listener
				fixture.Start()
				defer fixture.Close()
				_, port, err := net.SplitHostPort(listener.Addr().String())
				require.NoError(t, err)
				fixtureURL := "http://host.docker.internal:" + port

				flags := "--no-proxy-server"
				url := fixtureURL + "/network-check"
				if externalProxy {
					flags = "--proxy-server=" + fixtureURL
					url = "http://network.test/network-check"
				}

				c := NewTestContainer(t, image.ref)
				defer c.Stop(context.Background())
				require.NoError(t, c.Start(ctx, ContainerConfig{
					HostAccess: true,
					Env: map[string]string{
						"CHROMIUM_FLAGS":      flags,
						"INST_NAME":           "test-instance",
						"METRO_NAME":          "test-metro",
						"KERNEL_INSTANCE_JWT": "test-jwt",
						// Legacy identity must not start an in-guest proxy.
						"XDS_SERVER": "unused.invalid",
					},
				}))
				require.NoError(t, c.WaitBrowser(ctx))
				require.NoError(t, c.WaitChromeDriver(ctx))

				code, output, err := c.Exec(ctx, []string{"sh", "-ec", `
if command -v envoy; then exit 1; fi
for path in /etc/envoy /etc/supervisor/conf.d/services/envoy.conf /usr/local/bin/init-envoy.sh /kernel-envoy-proxy.crt /usr/local/share/ca-certificates/kernel-envoy-proxy.crt; do
    test ! -e "$path"
done
`})
				require.NoError(t, err)
				require.Zero(t, code, output)

				client, err := c.APIClient()
				require.NoError(t, err)
				response, err := client.ExecutePlaywrightCodeWithResponse(ctx, instanceoapi.ExecutePlaywrightCodeJSONRequestBody{
					Code: fmt.Sprintf("await page.goto(%q); return await page.title();", url),
				})
				require.NoError(t, err)
				require.Equal(t, http.StatusOK, response.StatusCode(), "%s", response.Body)
				require.NotNil(t, response.JSON200)
				require.True(t, response.JSON200.Success, "%s", response.Body)
				require.Equal(t, "network ready", response.JSON200.Result)
			})
		}
	}
}
