package cdpmonitor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kernel/kernel-images/server/lib/events"
	"github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/require"
)

// These tests use real cross-site frames rather than injecting attachment events:
// they must pass regardless of how the collector discovers and attaches targets.
// Run with KERNEL_CDPMONITOR_CHROME_E2E=1 and Chromium on PATH.
func TestNetworkCaptureAcrossFrames(t *testing.T) {
	if os.Getenv("KERNEL_CDPMONITOR_CHROME_E2E") == "" {
		t.Skip("set KERNEL_CDPMONITOR_CHROME_E2E=1 to run real-Chromium network tests")
	}
	chrome := findChromium(t)
	for _, isolated := range []bool{false, true} {
		contextName := "default"
		if isolated {
			contextName = "isolated"
		}
		for _, existing := range []bool{false, true} {
			timing := "new_frames"
			if existing {
				timing = "existing_frames"
			}
			t.Run(contextName+"/"+timing, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
				defer cancel()

				type receivedPost struct{ path, body string }
				received := make(chan receivedPost, 16)
				var origin, crossOrigin string
				stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodPost {
						body, err := io.ReadAll(r.Body)
						if err != nil {
							http.Error(w, err.Error(), http.StatusBadRequest)
							return
						}
						received <- receivedPost{r.URL.RequestURI(), string(body)}
						if r.URL.Path == "/abort" {
							select {
							case <-r.Context().Done():
							case <-time.After(5 * time.Second):
							}
							return
						}
						w.Header().Set("Content-Type", "application/json")
						_, _ = w.Write(body)
						return
					}
					w.Header().Set("Content-Type", "text/html")
					switch r.URL.Path {
					case "/":
						fmt.Fprintf(w, `<iframe id="same" src="/same"></iframe><iframe src="%s/cross"></iframe>`, crossOrigin)
					case "/cross":
						fmt.Fprintf(w, `<iframe src="%s/nested"></iframe>`, origin)
					default:
						fmt.Fprint(w, "<html><body>frame</body></html>")
					}
				}))
				defer stub.Close()
				origin = stub.URL
				crossOrigin = strings.Replace(origin, "127.0.0.1", "localhost", 1)

				browserWS := launchChromium(t, ctx, chrome, "--site-per-process")
				cdp := dialCDP(t, ctx, browserWS)
				defer cdp.close()
				params := map[string]any{"url": "about:blank"}
				if isolated {
					raw := cdp.call(t, ctx, "", "Target.createBrowserContext", map[string]any{"disposeOnDetach": true})
					var result struct {
						Result struct {
							BrowserContextID string `json:"browserContextId"`
						} `json:"result"`
					}
					require.NoError(t, json.Unmarshal(raw.raw, &result))
					require.NotEmpty(t, result.Result.BrowserContextID)
					params["browserContextId"] = result.Result.BrowserContextID
				}
				targetID := cdp.call(t, ctx, "", "Target.createTarget", params).targetID(t)
				sessionID := cdp.call(t, ctx, "", "Target.attachToTarget", map[string]any{"targetId": targetID, "flatten": true}).sessionID(t)
				ec := newEventCollector()
				m := New(&staticUpstream{url: browserWS}, ec.publishFn(), 99, discardLogger, nil)
				defer m.Stop()
				if !existing {
					require.NoError(t, m.Start(ctx))
				}
				cdp.call(t, ctx, sessionID, "Page.navigate", map[string]any{"url": origin})
				require.Eventually(t, func() bool {
					return cdp.evalBool(ctx, sessionID, fmt.Sprintf(`location.href === %q && document.readyState === "complete"`, origin+"/"))
				}, 10*time.Second, 50*time.Millisecond, "frame documents did not finish loading")

				// Driver attachment must not substitute for the monitor's own attachment.
				crossTarget := findNetworkFrameTarget(t, ctx, cdp, crossOrigin+"/cross")
				nestedTarget := findNetworkFrameTarget(t, ctx, cdp, origin+"/nested")
				crossSession := cdp.call(t, ctx, "", "Target.attachToTarget", map[string]any{"targetId": crossTarget, "flatten": true}).sessionID(t)
				nestedSession := cdp.call(t, ctx, "", "Target.attachToTarget", map[string]any{"targetId": nestedTarget, "flatten": true}).sessionID(t)
				if existing {
					require.NoError(t, m.Start(ctx))
				}
				require.Eventually(t, func() bool {
					return cdp.evalBool(ctx, sessionID, "window.__kernelEventInjected === true")
				}, 10*time.Second, 50*time.Millisecond, "monitor did not initialize the top-level page")
				// Exercise settled documents, not requests racing initial attachment.
				time.Sleep(500 * time.Millisecond)

				for _, frame := range []struct {
					name, session, target, targetType, origin, realm string
				}{
					{"top", sessionID, targetID, "page", origin, "window"},
					{"same_origin", sessionID, targetID, "page", origin, `document.querySelector("#same").contentWindow`},
					{"cross_origin", crossSession, crossTarget, "iframe", crossOrigin, "window"},
					{"nested_cross_origin", nestedSession, nestedTarget, "iframe", origin, "window"},
				} {
					for _, aborted := range []bool{false, true} {
						outcome := "sent"
						if aborted {
							outcome = "aborted"
						}
						t.Run(frame.name+"/"+outcome, func(t *testing.T) {
							marker := strings.ReplaceAll(t.Name(), "/", "_")
							body, err := json.Marshal(map[string]string{"marker": marker})
							require.NoError(t, err)
							path := "/post?marker=" + marker
							if aborted {
								path = "/abort?marker=" + marker
							}
							expression := fmt.Sprintf(`(async () => {
								const realm = %s;
								const controller = new AbortController();
								const timer = %t ? setTimeout(() => controller.abort(), 500) : null;
								try {
									const response = await realm.fetch(%q, {
										method: "POST", headers: {"Content-Type": "application/json"},
										body: %q, signal: controller.signal
									});
									return {status: response.status, body: await response.text()};
								} catch (error) {
									return {error: error.name};
								} finally { clearTimeout(timer); }
							})()`, frame.realm, aborted, path, string(body))
							raw := cdp.call(t, ctx, frame.session, "Runtime.evaluate", map[string]any{
								"expression": expression, "awaitPromise": true, "returnByValue": true,
							})
							var result struct {
								Error  json.RawMessage `json:"error"`
								Result struct {
									ExceptionDetails json.RawMessage `json:"exceptionDetails"`
									Result           struct {
										Value struct {
											Status int    `json:"status"`
											Body   string `json:"body"`
											Error  string `json:"error"`
										} `json:"value"`
									} `json:"result"`
								} `json:"result"`
							}
							require.NoError(t, json.Unmarshal(raw.raw, &result))
							require.Empty(t, result.Error)
							require.Empty(t, result.Result.ExceptionDetails)
							value := result.Result.Result.Value
							if aborted {
								require.Equal(t, "AbortError", value.Error)
							} else {
								require.Empty(t, value.Error)
								require.Equal(t, http.StatusOK, value.Status)
								require.JSONEq(t, string(body), value.Body)
							}
							select {
							case request := <-received:
								require.Equal(t, path, request.path)
								require.JSONEq(t, string(body), request.body)
							case <-time.After(time.Second):
								t.Fatal("the HTTP server did not receive the POST")
							}

							requestEvent := waitForNetworkURL(t, ec, EventNetworkRequest, frame.origin+path)
							var request oapi.BrowserNetworkRequestEventData
							require.NoError(t, json.Unmarshal(requestEvent.Data, &request))
							require.Equal(t, http.MethodPost, request.Method)
							require.NotNil(t, request.PostData)
							require.JSONEq(t, string(body), *request.PostData)
							require.Equal(t, frame.target, request.TargetId)
							require.Equal(t, frame.targetType, string(request.TargetType))
							require.NotEmpty(t, request.SessionId)
							require.NotNil(t, request.FrameId)
							require.NotEmpty(t, *request.FrameId)
							if aborted {
								event := waitForNetworkURL(t, ec, EventNetworkLoadingFailed, frame.origin+path)
								var failure oapi.BrowserNetworkLoadingFailedEventData
								require.NoError(t, json.Unmarshal(event.Data, &failure))
								require.Equal(t, request.RequestId, failure.RequestId)
								require.Equal(t, request.SessionId, failure.SessionId)
								require.True(t, failure.Canceled)
							} else {
								event := waitForNetworkURL(t, ec, EventNetworkResponse, frame.origin+path)
								var response oapi.BrowserNetworkResponseEventData
								require.NoError(t, json.Unmarshal(event.Data, &response))
								require.Equal(t, request.RequestId, response.RequestId)
								require.Equal(t, request.SessionId, response.SessionId)
								require.EqualValues(t, http.StatusOK, response.Status)
								require.NotNil(t, response.Body)
								require.JSONEq(t, string(body), *response.Body)
							}
						})
					}
				}
			})
		}
	}
}

func findNetworkFrameTarget(t *testing.T, ctx context.Context, cdp *cdpConn, url string) string {
	t.Helper()
	var targetID string
	require.Eventually(t, func() bool {
		raw, err := cdp.roundtrip(ctx, "", "Target.getTargets", nil)
		if err != nil {
			return false
		}
		var result struct {
			Result struct {
				TargetInfos []cdpTargetTargetInfo `json:"targetInfos"`
			} `json:"result"`
		}
		if json.Unmarshal(raw, &result) != nil {
			return false
		}
		for _, target := range result.Result.TargetInfos {
			if target.URL == url && target.Type == "iframe" {
				targetID = target.TargetID
				return true
			}
		}
		return false
	}, 5*time.Second, 50*time.Millisecond, "expected a real out-of-process iframe target for %s", url)
	return targetID
}

func waitForNetworkURL(t *testing.T, ec *eventCollector, eventType, url string) events.Event {
	t.Helper()
	var found events.Event
	require.Eventually(t, func() bool {
		ec.mu.Lock()
		defer ec.mu.Unlock()
		for _, event := range ec.events {
			if event.Type != eventType || event.Category != events.Network {
				continue
			}
			var data struct {
				URL string `json:"url"`
			}
			if json.Unmarshal(event.Data, &data) == nil && data.URL == url {
				found = event
				return true
			}
		}
		return false
	}, 3*time.Second, 25*time.Millisecond, "missing %s telemetry for completed browser operation %s", eventType, url)
	return found
}
