package cdpmonitor

import (
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kernel/kernel-images/server/lib/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConsoleEvents(t *testing.T) {
	srv := newTestServer(t)
	defer srv.close()

	_, ec, cleanup := startMonitor(t, srv, nil)
	defer cleanup()

	t.Run("console_log", func(t *testing.T) {
		srv.sendToMonitor(t, map[string]any{
			"method": "Runtime.consoleAPICalled",
			"params": map[string]any{
				"type": "log",
				"args": []any{map[string]any{"type": "string", "value": "hello world"}},
			},
		})
		ev := ec.waitFor(t, "console_log", 2*time.Second)
		assert.Equal(t, events.CategoryConsole, ev.Category)
		assert.Equal(t, events.KindCDP, ev.Source.Kind)
		assert.Equal(t, "Runtime.consoleAPICalled", ev.Source.Event)
		var data map[string]any
		require.NoError(t, json.Unmarshal(ev.Data, &data))
		assert.Equal(t, "log", data["level"])
		assert.Equal(t, "hello world", data["text"])
	})

	t.Run("exception_thrown", func(t *testing.T) {
		srv.sendToMonitor(t, map[string]any{
			"method": "Runtime.exceptionThrown",
			"params": map[string]any{
				"timestamp": 1234.5,
				"exceptionDetails": map[string]any{
					"text":         "Uncaught TypeError",
					"lineNumber":   42,
					"columnNumber": 7,
					"url":          "https://example.com/app.js",
				},
			},
		})
		ev := ec.waitFor(t, "console_error", 2*time.Second)
		assert.Equal(t, events.CategoryConsole, ev.Category)
		var data map[string]any
		require.NoError(t, json.Unmarshal(ev.Data, &data))
		assert.Equal(t, "Uncaught TypeError", data["text"])
		assert.Equal(t, float64(42), data["line"])
	})

	t.Run("non_string_args", func(t *testing.T) {
		srv.sendToMonitor(t, map[string]any{
			"method": "Runtime.consoleAPICalled",
			"params": map[string]any{
				"type": "log",
				"args": []any{
					map[string]any{"type": "number", "value": 42},
					map[string]any{"type": "object", "value": map[string]any{"key": "val"}},
					map[string]any{"type": "undefined"},
				},
			},
		})
		ev := ec.waitForNew(t, "console_log", 2*time.Second)
		var data map[string]any
		require.NoError(t, json.Unmarshal(ev.Data, &data))
		args := data["args"].([]any)
		assert.Equal(t, "42", args[0])
		assert.Contains(t, args[1], "key")
		assert.Equal(t, "undefined", args[2])
	})
}

func TestNetworkEvents(t *testing.T) {
	srv := newTestServer(t)
	defer srv.close()

	var getBodyCalled atomic.Bool
	responder := func(msg cdpMessage) any {
		if msg.Method == "Network.getResponseBody" {
			getBodyCalled.Store(true)
			return map[string]any{
				"id":     msg.ID,
				"result": map[string]any{"body": `{"ok":true}`, "base64Encoded": false},
			}
		}
		return nil
	}
	_, ec, cleanup := startMonitor(t, srv, responder)
	defer cleanup()

	t.Run("request_and_response", func(t *testing.T) {
		srv.sendToMonitor(t, map[string]any{
			"method": "Network.requestWillBeSent",
			"params": map[string]any{
				"requestId": "req-001",
				"type":      "XHR",
				"request": map[string]any{
					"method":  "POST",
					"url":     "https://api.example.com/data",
					"headers": map[string]any{"Content-Type": "application/json"},
				},
				"initiator": map[string]any{"type": "script"},
			},
		})
		ev := ec.waitFor(t, "network_request", 2*time.Second)
		assert.Equal(t, events.CategoryNetwork, ev.Category)
		assert.Equal(t, "Network.requestWillBeSent", ev.Source.Event)

		var data map[string]any
		require.NoError(t, json.Unmarshal(ev.Data, &data))
		assert.Equal(t, "POST", data["method"])
		assert.Equal(t, "https://api.example.com/data", data["url"])
		assert.Equal(t, "XHR", data["resource_type"], "resource_type must be populated from PDL 'type' wire field")

		srv.sendToMonitor(t, map[string]any{
			"method": "Network.responseReceived",
			"params": map[string]any{
				"requestId": "req-001",
				"response": map[string]any{
					"status": 200, "statusText": "OK",
					"headers": map[string]any{"Content-Type": "application/json"}, "mimeType": "application/json",
				},
			},
		})
		srv.sendToMonitor(t, map[string]any{
			"method": "Network.loadingFinished",
			"params": map[string]any{"requestId": "req-001"},
		})

		ev2 := ec.waitFor(t, "network_response", 3*time.Second)
		assert.Equal(t, "Network.loadingFinished", ev2.Source.Event)
		var data2 map[string]any
		require.NoError(t, json.Unmarshal(ev2.Data, &data2))
		assert.Equal(t, float64(200), data2["status"])
		assert.NotEmpty(t, data2["body"])
	})

	t.Run("loading_failed", func(t *testing.T) {
		srv.sendToMonitor(t, map[string]any{
			"method": "Network.requestWillBeSent",
			"params": map[string]any{
				"requestId": "req-002",
				"request":   map[string]any{"method": "GET", "url": "https://fail.example.com/"},
			},
		})
		ec.waitForNew(t, "network_request", 2*time.Second)

		srv.sendToMonitor(t, map[string]any{
			"method": "Network.loadingFailed",
			"params": map[string]any{
				"requestId": "req-002",
				"errorText": "net::ERR_CONNECTION_REFUSED",
				"canceled":  false,
			},
		})
		ev := ec.waitFor(t, "network_loading_failed", 2*time.Second)
		assert.Equal(t, events.CategoryNetwork, ev.Category)
		var data map[string]any
		require.NoError(t, json.Unmarshal(ev.Data, &data))
		assert.Equal(t, "net::ERR_CONNECTION_REFUSED", data["error_text"])
	})

	t.Run("binary_resource_skips_body", func(t *testing.T) {
		getBodyCalled.Store(false)
		// Use PDL wire key "type" (not "resourceType") — Chrome emits ResourceType
		// under "type" for Network.requestWillBeSent.
		srv.sendToMonitor(t, map[string]any{
			"method": "Network.requestWillBeSent",
			"params": map[string]any{
				"requestId": "img-001",
				"type":      "Image",
				"request":   map[string]any{"method": "GET", "url": "https://example.com/photo.png"},
			},
		})
		srv.sendToMonitor(t, map[string]any{
			"method": "Network.responseReceived",
			"params": map[string]any{
				"requestId": "img-001",
				"response":  map[string]any{"status": 200, "statusText": "OK", "headers": map[string]any{}, "mimeType": "image/png"},
			},
		})
		srv.sendToMonitor(t, map[string]any{
			"method": "Network.loadingFinished",
			"params": map[string]any{"requestId": "img-001"},
		})

		ev := ec.waitForNew(t, "network_response", 3*time.Second)
		var data map[string]any
		require.NoError(t, json.Unmarshal(ev.Data, &data))
		assert.Nil(t, data["body"], "binary resource should not have body field")
		assert.False(t, getBodyCalled.Load(), "should not call getResponseBody for images")
	})
}

func TestPageEvents(t *testing.T) {
	srv := newTestServer(t)
	defer srv.close()

	_, ec, cleanup := startMonitor(t, srv, nil)
	defer cleanup()

	// Attach a page target first so computedState exists for nav context.
	srv.sendToMonitor(t, map[string]any{
		"method": "Target.attachedToTarget",
		"params": map[string]any{
			"sessionId": "sess-page",
			"targetInfo": map[string]any{
				"targetId": "target-page", "type": "page",
				"url": "about:blank", "attached": true,
			},
			"waitingForDebugger": false,
		},
	})
	ec.waitFor(t, "page_tab_opened", 2*time.Second)

	srv.sendToMonitor(t, map[string]any{
		"method": "Page.frameNavigated", "sessionId": "sess-page",
		"params": map[string]any{
			"frame": map[string]any{
				"id": "frame-1", "url": "https://example.com/page",
				"loaderId": "loader-1",
			},
		},
	})
	ev := ec.waitFor(t, "page_navigation", 2*time.Second)
	assert.Equal(t, events.CategoryPage, ev.Category)
	assert.Equal(t, "Page.frameNavigated", ev.Source.Event)
	var data map[string]any
	require.NoError(t, json.Unmarshal(ev.Data, &data))
	assert.Equal(t, "https://example.com/page", data["url"])

	srv.sendToMonitor(t, map[string]any{
		"method": "Page.domContentEventFired", "sessionId": "sess-page",
		"params": map[string]any{"timestamp": 1000.0},
	})
	ev2 := ec.waitFor(t, "page_dom_content_loaded", 2*time.Second)
	assert.Equal(t, events.CategoryPage, ev2.Category)
	var data2 map[string]any
	require.NoError(t, json.Unmarshal(ev2.Data, &data2))
	assert.Equal(t, float64(1000.0), data2["cdp_timestamp"])
	assert.Equal(t, "loader-1", data2["loader_id"])
	assert.Equal(t, "https://example.com/page", data2["url"])

	srv.sendToMonitor(t, map[string]any{
		"method": "Page.loadEventFired", "sessionId": "sess-page",
		"params": map[string]any{"timestamp": 1001.0},
	})
	ev3 := ec.waitFor(t, "page_load", 2*time.Second)
	assert.Equal(t, events.CategoryPage, ev3.Category)
	var data3 map[string]any
	require.NoError(t, json.Unmarshal(ev3.Data, &data3))
	assert.Equal(t, float64(1001.0), data3["cdp_timestamp"])
	assert.Equal(t, "loader-1", data3["loader_id"])
}

func TestTabOpened(t *testing.T) {
	srv := newTestServer(t)
	defer srv.close()

	_, ec, cleanup := startMonitor(t, srv, nil)
	defer cleanup()

	t.Run("page_target_emits_tab_opened", func(t *testing.T) {
		srv.sendToMonitor(t, map[string]any{
			"method": "Target.attachedToTarget",
			"params": map[string]any{
				"sessionId": "sess-tab",
				"targetInfo": map[string]any{
					"targetId": "target-tab", "type": "page",
					"url": "https://example.com", "attached": true,
					"title": "Example",
				},
				"waitingForDebugger": false,
			},
		})
		ev := ec.waitFor(t, "page_tab_opened", 2*time.Second)
		assert.Equal(t, events.CategoryPage, ev.Category)
		assert.Equal(t, "Target.attachedToTarget", ev.Source.Event)
		var data map[string]any
		require.NoError(t, json.Unmarshal(ev.Data, &data))
		assert.Equal(t, "target-tab", data["target_id"])
		assert.Equal(t, "page", data["target_type"])
		assert.Equal(t, "https://example.com", data["url"])
		assert.Equal(t, "Example", data["title"])
	})

	t.Run("iframe_target_no_tab_opened", func(t *testing.T) {
		srv.sendToMonitor(t, map[string]any{
			"method": "Target.attachedToTarget",
			"params": map[string]any{
				"sessionId": "sess-iframe",
				"targetInfo": map[string]any{
					"targetId": "target-iframe", "type": "iframe",
					"url": "https://iframe.example.com", "attached": true,
				},
				"waitingForDebugger": false,
			},
		})
		ec.assertNone(t, "page_tab_opened", 200*time.Millisecond)
	})
}

func TestBindingAndTimeline(t *testing.T) {
	srv := newTestServer(t)
	defer srv.close()

	_, ec, cleanup := startMonitor(t, srv, nil)
	defer cleanup()

	t.Run("interaction_click", func(t *testing.T) {
		srv.sendToMonitor(t, map[string]any{
			"method": "Runtime.bindingCalled",
			"params": map[string]any{
				"name":    "__kernelEvent",
				"payload": `{"type":"interaction_click","x":10,"y":20,"selector":"button","tag":"BUTTON","text":"OK"}`,
			},
		})
		ev := ec.waitFor(t, "interaction_click", 2*time.Second)
		assert.Equal(t, events.CategoryInteraction, ev.Category)
		assert.Equal(t, "Runtime.bindingCalled", ev.Source.Event)
	})

	t.Run("interaction_scroll_settled", func(t *testing.T) {
		srv.sendToMonitor(t, map[string]any{
			"method": "Runtime.bindingCalled",
			"params": map[string]any{
				"name":    "__kernelEvent",
				"payload": `{"type":"interaction_scroll_settled","from_x":0,"from_y":0,"to_x":0,"to_y":500,"target_selector":"body"}`,
			},
		})
		ev := ec.waitFor(t, "interaction_scroll_settled", 2*time.Second)
		assert.Equal(t, events.CategoryInteraction, ev.Category)
		var data map[string]any
		require.NoError(t, json.Unmarshal(ev.Data, &data))
		assert.Equal(t, float64(500), data["to_y"])
	})

	t.Run("layout_shift", func(t *testing.T) {
		srv.sendToMonitor(t, map[string]any{
			"method": "PerformanceTimeline.timelineEventAdded",
			"params": map[string]any{
				"event": map[string]any{
					"type":     "layout-shift",
					"frameId":  "frame-ls",
					"time":     1.5,
					"duration": 0.0,
					"layoutShiftDetails": map[string]any{
						"value":          0.12,
						"hadRecentInput": true,
					},
				},
			},
		})
		ev := ec.waitFor(t, "page_layout_shift", 2*time.Second)
		assert.Equal(t, events.KindCDP, ev.Source.Kind)
		assert.Equal(t, "PerformanceTimeline.timelineEventAdded", ev.Source.Event)
		var data map[string]any
		require.NoError(t, json.Unmarshal(ev.Data, &data))
		assert.Equal(t, "frame-ls", data["source_frame_id"])
		assert.Equal(t, float64(1.5), data["time"])
		shift := data["layout_shift_details"].(map[string]any)
		assert.Equal(t, 0.12, shift["value"])
		assert.Equal(t, true, shift["had_recent_input"])
		_, hasEvent := data["event"]
		assert.False(t, hasEvent, "raw CDP event wrapper must not appear in payload")
	})

	t.Run("unknown_binding_ignored", func(t *testing.T) {
		srv.sendToMonitor(t, map[string]any{
			"method": "Runtime.bindingCalled",
			"params": map[string]any{
				"name":    "someOtherBinding",
				"payload": `{"type":"interaction_click"}`,
			},
		})
		ec.assertNone(t, "interaction_click", 100*time.Millisecond)
	})

	t.Run("rate_limited_per_session", func(t *testing.T) {
		// Send two binding events back-to-back within the 50ms window.
		// Only the first should produce a published event.
		before := func() int {
			ec.mu.Lock()
			defer ec.mu.Unlock()
			count := 0
			for _, ev := range ec.events {
				if ev.Type == EventInteractionClick {
					count++
				}
			}
			return count
		}
		countBefore := before()

		for range 3 {
			srv.sendToMonitor(t, map[string]any{
				"method": "Runtime.bindingCalled",
				"params": map[string]any{
					"name":    "__kernelEvent",
					"payload": `{"type":"interaction_click","x":1,"y":1,"selector":"a","tag":"A","text":"x"}`,
				},
			})
		}

		// Wait a bit for async delivery, then check only 1 new event was published.
		time.Sleep(200 * time.Millisecond)
		ec.mu.Lock()
		countAfter := 0
		for _, ev := range ec.events {
			if ev.Type == EventInteractionClick {
				countAfter++
			}
		}
		ec.mu.Unlock()
		assert.Equal(t, countBefore+1, countAfter, "rate limiter should have dropped the 2nd and 3rd events")
	})
}

func TestPerTargetStateMachines(t *testing.T) {
	// attachTarget sends a Target.attachedToTarget message for a page session.
	attachTarget := func(srv *testServer, t *testing.T, sessionID, targetID string) {
		t.Helper()
		srv.sendToMonitor(t, map[string]any{
			"method": "Target.attachedToTarget",
			"params": map[string]any{
				"sessionId": sessionID,
				"targetInfo": map[string]any{
					"targetId": targetID, "type": "page",
					"url": "about:blank", "attached": true,
				},
				"waitingForDebugger": false,
			},
		})
	}

	t.Run("two_tabs_independent", func(t *testing.T) {
		srv := newTestServer(t)
		defer srv.close()
		_, ec, cleanup := startMonitor(t, srv, nil)
		defer cleanup()

		attachTarget(srv, t, "sess-a", "target-a")
		attachTarget(srv, t, "sess-b", "target-b")

		// Navigate sess-a and start a request.
		srv.sendToMonitor(t, map[string]any{
			"method": "Page.frameNavigated", "sessionId": "sess-a",
			"params": map[string]any{"frame": map[string]any{
				"id": "f-a", "url": "https://a.example.com", "loaderId": "l-a",
			}},
		})
		ec.waitFor(t, "page_navigation", 2*time.Second)

		srv.sendToMonitor(t, map[string]any{
			"method": "Network.requestWillBeSent", "sessionId": "sess-a",
			"params": map[string]any{
				"requestId": "req-a", "type": "Document", "loaderId": "l-a",
				"documentURL": "https://a.example.com/",
				"request":     map[string]any{"method": "GET", "url": "https://a.example.com/"},
				"initiator":   map[string]any{"type": "other"},
			},
		})
		ec.waitFor(t, "network_request", 2*time.Second)

		// Navigate sess-b — must not reset sess-a's state machine.
		// With per-session state machines, sess-b starts fresh (netPending=0) and
		// fires its own network_idle after the 500 ms debounce, independently.
		srv.sendToMonitor(t, map[string]any{
			"method": "Page.frameNavigated", "sessionId": "sess-b",
			"params": map[string]any{"frame": map[string]any{
				"id": "f-b", "url": "https://b.example.com", "loaderId": "l-b",
			}},
		})

		// Wait past sess-b's 500 ms debounce so its network_idle fires before we
		// set our checkpoint. The next new network_idle will then come from sess-a.
		time.Sleep(700 * time.Millisecond)

		// Finish sess-a's request; waitForNew captures the current event count so
		// sess-b's already-fired network_idle is excluded from the result.
		srv.sendToMonitor(t, map[string]any{
			"method": "Network.responseReceived", "sessionId": "sess-a",
			"params": map[string]any{
				"requestId": "req-a", "type": "Document",
				"response": map[string]any{"status": 200, "statusText": "OK", "headers": map[string]any{}, "mimeType": "text/html"},
			},
		})
		srv.sendToMonitor(t, map[string]any{
			"method": "Network.loadingFinished", "sessionId": "sess-a",
			"params": map[string]any{"requestId": "req-a"},
		})

		ev := ec.waitForNew(t, "network_idle", 2*time.Second)
		var data map[string]any
		require.NoError(t, json.Unmarshal(ev.Data, &data))
		assert.Equal(t, "sess-a", data["session_id"], "network_idle must be attributed to sess-a")
		assert.Equal(t, "l-a", data["loader_id"])
	})

	t.Run("detach_stops_timer", func(t *testing.T) {
		srv := newTestServer(t)
		defer srv.close()
		_, ec, cleanup := startMonitor(t, srv, nil)
		defer cleanup()

		attachTarget(srv, t, "sess-c", "target-c")

		srv.sendToMonitor(t, map[string]any{
			"method": "Page.frameNavigated", "sessionId": "sess-c",
			"params": map[string]any{"frame": map[string]any{
				"id": "f-c", "url": "https://c.example.com", "loaderId": "l-c",
			}},
		})
		ec.waitFor(t, "page_navigation", 2*time.Second)

		// Start a request, then finish it (arms the 500 ms network_idle timer).
		srv.sendToMonitor(t, map[string]any{
			"method": "Network.requestWillBeSent", "sessionId": "sess-c",
			"params": map[string]any{
				"requestId": "req-c", "type": "Document", "loaderId": "l-c",
				"documentURL": "https://c.example.com/",
				"request":     map[string]any{"method": "GET", "url": "https://c.example.com/"},
				"initiator":   map[string]any{"type": "other"},
			},
		})
		ec.waitFor(t, "network_request", 2*time.Second)
		srv.sendToMonitor(t, map[string]any{
			"method": "Network.loadingFinished", "sessionId": "sess-c",
			"params": map[string]any{"requestId": "req-c"},
		})

		// Detach before the 500 ms timer fires; readLoop processes messages in
		// order so the stop() call lands well within the debounce window.
		srv.sendToMonitor(t, map[string]any{
			"method": "Target.detachedFromTarget",
			"params": map[string]any{"sessionId": "sess-c"},
		})

		ec.assertNone(t, "network_idle", 1200*time.Millisecond)
	})
}
