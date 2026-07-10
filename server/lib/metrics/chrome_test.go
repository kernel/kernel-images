package metrics

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type staticUpstream string

func (s staticUpstream) Current() string { return string(s) }

// fakeCDP answers the browser-level commands the chrome collector issues.
func fakeCDP(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
		require.NoError(t, err)
		defer conn.Close(websocket.StatusNormalClosure, "")

		ctx := r.Context()
		for {
			_, msg, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var req struct {
				ID     int64  `json:"id"`
				Method string `json:"method"`
				Params struct {
					Query string `json:"query"`
				} `json:"params"`
			}
			require.NoError(t, json.Unmarshal(msg, &req))

			var result any
			switch req.Method {
			case "Browser.getVersion":
				result = map[string]any{"product": "Chrome/145.0.7632.75"}
			case "Target.getTargets":
				result = map[string]any{"targetInfos": []map[string]any{
					{"targetId": "A", "type": "page"},
					{"targetId": "B", "type": "page"},
					{"targetId": "C", "type": "service_worker"},
				}}
			case "Browser.getHistograms":
				if req.Params.Query == "Fail.Me" {
					resp, err := json.Marshal(map[string]any{"id": req.ID, "error": map[string]any{"code": -32000, "message": "boom"}})
					require.NoError(t, err)
					require.NoError(t, conn.Write(ctx, websocket.MessageText, resp))
					continue
				}
				histograms := []map[string]any{}
				if strings.Contains("PageLoad.PaintTiming.NavigationToFirstContentfulPaint", req.Params.Query) {
					histograms = append(histograms,
						map[string]any{
							"name": "PageLoad.PaintTiming.NavigationToFirstContentfulPaint", "sum": 21921, "count": 4,
							"buckets": []map[string]any{
								{"low": 606, "high": 638, "count": 2},
								{"low": 819, "high": 862, "count": 1},
								{"low": 10000, "high": 2147483647, "count": 1},
							},
						},
						// Suffixed variant matched by the substring query;
						// must be filtered out by the collector.
						map[string]any{
							"name": "PageLoad.PaintTiming.NavigationToFirstContentfulPaint.UserInitiated", "sum": 1, "count": 1,
							"buckets": []map[string]any{{"low": 0, "high": 2, "count": 1}},
						},
					)
				}
				result = map[string]any{"histograms": histograms}
			default:
				result = map[string]any{}
			}
			resp, err := json.Marshal(map[string]any{"id": req.ID, "result": result})
			require.NoError(t, err)
			require.NoError(t, conn.Write(ctx, websocket.MessageText, resp))
		}
	}))
}

func TestChromeCollector(t *testing.T) {
	srv := fakeCDP(t)
	defer srv.Close()

	c := NewChromeCollector(staticUpstream("ws" + strings.TrimPrefix(srv.URL, "http")))
	w := &Writer{}
	require.NoError(t, c.Collect(context.Background(), w))
	out := string(w.Bytes())

	assert.Contains(t, out, "kernel_chromium_up 1\n")
	assert.Contains(t, out, `kernel_chromium_info{product="Chrome/145.0.7632.75"} 1`)
	assert.Contains(t, out, "kernel_chromium_pages 2\n")

	// Chrome's buckets ([606,638):2, [819,862):1, overflow:1) re-bucket
	// onto the fixed grid: both finite buckets land at le=1000, the
	// overflow lands in +Inf, and every fixed bound is emitted so all VMs
	// produce identical le sets.
	fcp := `kernel_chromium_uma_bucket{histogram="PageLoad.PaintTiming.NavigationToFirstContentfulPaint",le=`
	assert.Contains(t, out, fcp+`"100"} 0`)
	assert.Contains(t, out, fcp+`"500"} 0`)
	assert.Contains(t, out, fcp+`"1000"} 3`)
	assert.Contains(t, out, fcp+`"60000"} 3`)
	assert.Contains(t, out, fcp+`"+Inf"} 4`)
	assert.NotContains(t, out, `le="638"`)
	assert.Contains(t, out, `kernel_chromium_uma_sum{histogram="PageLoad.PaintTiming.NavigationToFirstContentfulPaint"} 21921`)
	assert.Contains(t, out, `kernel_chromium_uma_count{histogram="PageLoad.PaintTiming.NavigationToFirstContentfulPaint"} 4`)
	assert.NotContains(t, out, "UserInitiated")
}

func TestChromeCollectorDown(t *testing.T) {
	// An unreachable browser is not a collector error: the up sample must
	// survive the handler's discard-on-error policy.
	c := NewChromeCollector(staticUpstream(""))
	w := &Writer{}
	require.NoError(t, c.Collect(context.Background(), w))
	assert.Contains(t, string(w.Bytes()), "kernel_chromium_up 0\n")
}

func TestChromeCollectorHistogramFailure(t *testing.T) {
	srv := fakeCDP(t)
	defer srv.Close()

	c := NewChromeCollector(staticUpstream("ws" + strings.TrimPrefix(srv.URL, "http")))
	c.histograms = []UMAHistogram{{Name: "Fail.Me", Bounds: boundsPageLoadMs}}
	w := &Writer{}
	require.Error(t, c.Collect(context.Background(), w))
}
