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

	srv.sendToMonitor(t, map[string]any{
		"method": "Page.frameNavigated",
		"params": map[string]any{
			"frame": map[string]any{"id": "frame-1", "url": "https://example.com/page"},
		},
	})
	ev := ec.waitFor(t, "navigation", 2*time.Second)
	assert.Equal(t, events.CategoryPage, ev.Category)
	assert.Equal(t, "Page.frameNavigated", ev.Source.Event)
	var data map[string]any
	require.NoError(t, json.Unmarshal(ev.Data, &data))
	assert.Equal(t, "https://example.com/page", data["url"])

	srv.sendToMonitor(t, map[string]any{
		"method": "Page.domContentEventFired",
		"params": map[string]any{"timestamp": 1000.0},
	})
	ev2 := ec.waitFor(t, "dom_content_loaded", 2*time.Second)
	assert.Equal(t, events.CategoryPage, ev2.Category)
	srv.sendToMonitor(t, map[string]any{
		"method": "Page.loadEventFired",
		"params": map[string]any{"timestamp": 1001.0},
	})
	ev3 := ec.waitFor(t, "page_load", 2*time.Second)
	assert.Equal(t, events.CategoryPage, ev3.Category)
}

func TestTargetEvents(t *testing.T) {
	srv := newTestServer(t)
	defer srv.close()

	_, ec, cleanup := startMonitor(t, srv, nil)
	defer cleanup()

	srv.sendToMonitor(t, map[string]any{
		"method": "Target.targetCreated",
		"params": map[string]any{
			"targetInfo": map[string]any{"targetId": "t-1", "type": "page", "url": "https://new.example.com"},
		},
	})
	ev := ec.waitFor(t, "target_created", 2*time.Second)
	assert.Equal(t, events.CategoryPage, ev.Category)
	var data map[string]any
	require.NoError(t, json.Unmarshal(ev.Data, &data))
	assert.Equal(t, "t-1", data["target_id"])

	srv.sendToMonitor(t, map[string]any{
		"method": "Target.targetDestroyed",
		"params": map[string]any{"targetId": "t-1"},
	})
	ev2 := ec.waitFor(t, "target_destroyed", 2*time.Second)
	assert.Equal(t, events.CategoryPage, ev2.Category)
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

	t.Run("scroll_settled", func(t *testing.T) {
		srv.sendToMonitor(t, map[string]any{
			"method": "Runtime.bindingCalled",
			"params": map[string]any{
				"name":    "__kernelEvent",
				"payload": `{"type":"scroll_settled","from_x":0,"from_y":0,"to_x":0,"to_y":500,"target_selector":"body"}`,
			},
		})
		ev := ec.waitFor(t, "scroll_settled", 2*time.Second)
		assert.Equal(t, events.CategoryInteraction, ev.Category)
		var data map[string]any
		require.NoError(t, json.Unmarshal(ev.Data, &data))
		assert.Equal(t, float64(500), data["to_y"])
	})

	t.Run("layout_shift", func(t *testing.T) {
		srv.sendToMonitor(t, map[string]any{
			"method": "PerformanceTimeline.timelineEventAdded",
			"params": map[string]any{
				"event": map[string]any{"type": "layout-shift"},
			},
		})
		ev := ec.waitFor(t, "layout_shift", 2*time.Second)
		assert.Equal(t, events.KindCDP, ev.Source.Kind)
		assert.Equal(t, "PerformanceTimeline.timelineEventAdded", ev.Source.Event)
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
