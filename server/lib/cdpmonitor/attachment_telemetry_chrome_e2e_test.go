package cdpmonitor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

func TestTelemetryWaitsForAttachmentChrome(t *testing.T) {
	if os.Getenv("KERNEL_CDPMONITOR_CHROME_E2E") == "" {
		t.Skip("set KERNEL_CDPMONITOR_CHROME_E2E=1")
	}
	for _, toggles := range []int{1, 5} {
		t.Run(fmt.Sprintf("toggles_%d", toggles), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/html")
				fmt.Fprint(w, `<link rel="icon" href="data:,"><button id="go">go</button>`)
			}))
			defer page.Close()
			ws := launchChromium(t, ctx, findChromium(t))
			driver := dialCDP(t, ctx, ws)
			defer driver.close()
			target := driver.call(t, ctx, "", "Target.createTarget", map[string]any{"url": page.URL}).targetID(t)
			session := driver.call(t, ctx, "", "Target.attachToTarget", map[string]any{"targetId": target, "flatten": true}).sessionID(t)
			require.Eventually(t, func() bool {
				return driver.evalBool(ctx, session, `document.readyState === 'complete' && !!document.querySelector('#go')`)
			}, 5*time.Second, 10*time.Millisecond)
			proxy, blocked, release := delayNetworkReplies(t, ctx, ws)
			ec := newEventCollector()
			mon := New(newTestUpstream(proxy), ec.publishFn(), 0, discardLogger, func() bool { return false })
			require.NoError(t, mon.SetTelemetry(false))
			require.NoError(t, mon.Start(ctx))
			defer mon.Stop()
			defer release()
			for waiting := true; waiting; {
				select {
				case sid := <-blocked:
					mon.sessionsMu.RLock()
					waiting = mon.sessions[sid].targetID != target
					mon.sessionsMu.RUnlock()
				case <-ctx.Done():
					t.Fatal("page Network.enable reply was not intercepted")
				}
			}
			for i := 0; i < toggles; i++ {
				require.NoError(t, mon.SetTelemetry(i%2 == 0))
				require.Eventually(t, func() bool {
					return mon.appliedTelemetry.Load() == mon.desiredTelemetry.Load() && !mon.telemetryChanging.Load()
				}, time.Second, time.Millisecond)
			}
			require.False(t, mon.NetworkSnapshot().Up)
			require.Never(t, func() bool {
				return driver.evalBool(ctx, session, `window.__kernelEventInjected === true`)
			}, 250*time.Millisecond, 10*time.Millisecond, "optional injection ran before attachment recovery")
			release()
			waitForTelemetryReconcile(t, mon, true)
			for i := 0; i < 3; i++ {
				require.Eventually(t, func() bool {
					return driver.evalBool(ctx, session, `window.__kernelEventInjected === true`)
				}, time.Second, 10*time.Millisecond)
				checkpoint := ec.checkpoint()
				evaluateNetworkScript(t, ctx, driver, session, `document.querySelector('#go').click(); true`)
				ec.waitForNew(t, EventInteractionClick, checkpoint, time.Second)
				if i < 2 {
					require.NoError(t, mon.SetTelemetry(false))
					waitForTelemetryReconcile(t, mon, false)
					require.False(t, driver.evalBool(ctx, session, `window.__kernelEventInjected === true`))
					require.NoError(t, mon.SetTelemetry(true))
					waitForTelemetryReconcile(t, mon, true)
				}
			}
		})
	}
}

// Only Network.enable responses are held; commands, events, and other replies
// continue to flow. The separate user CDP connection bypasses this proxy.
func delayNetworkReplies(t *testing.T, parent context.Context, upstreamURL string) (string, <-chan string, func()) {
	t.Helper()
	blocked := make(chan string, 16)
	released := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(released) }) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		client, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer client.CloseNow()
		ctx, cancel := context.WithCancel(parent)
		defer cancel()
		upstream, _, err := websocket.Dial(ctx, upstreamURL, nil)
		if err != nil {
			return
		}
		defer upstream.CloseNow()
		var mu sync.Mutex
		network := make(map[int]string)
		var delayed sync.WaitGroup
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer cancel()
			for {
				kind, data, err := upstream.Read(ctx)
				if err != nil {
					return
				}
				var reply struct {
					ID int `json:"id"`
				}
				if json.Unmarshal(data, &reply) != nil {
					return
				}
				mu.Lock()
				sid, hold := network[reply.ID]
				delete(network, reply.ID)
				mu.Unlock()
				if hold {
					delayed.Go(func() {
						select {
						case blocked <- sid:
						case <-ctx.Done():
							return
						}
						select {
						case <-released:
							_ = client.Write(ctx, kind, data)
						case <-ctx.Done():
						}
					})
				} else if client.Write(ctx, kind, data) != nil {
					return
				}
			}
		}()
		defer func() { cancel(); <-done; delayed.Wait() }()
		for {
			kind, data, err := client.Read(ctx)
			if err != nil {
				return
			}
			var command struct {
				ID        int    `json:"id"`
				Method    string `json:"method"`
				SessionID string `json:"sessionId"`
			}
			if json.Unmarshal(data, &command) != nil {
				return
			}
			if command.Method == "Network.enable" {
				mu.Lock()
				network[command.ID] = command.SessionID
				mu.Unlock()
			}
			if upstream.Write(ctx, kind, data) != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	return "ws" + strings.TrimPrefix(server.URL, "http"), blocked, release
}
