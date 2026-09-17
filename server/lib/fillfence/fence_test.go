package fillfence

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

func TestFenceCompletionAndQuarantine(t *testing.T) {
	for _, loss := range []bool{false, true} {
		t.Run(map[bool]string{false: "release", true: "lost-response"}[loss], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			entered, resume, completed := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var writes atomic.Int32
			chrome := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer c.CloseNow()
				_, data, err := c.Read(ctx)
				if err != nil {
					return
				}
				var m message
				require.NoError(t, json.Unmarshal(data, &m))
				writes.Add(1)
				close(entered)
				<-resume
				reply(ctx, c, m.ID, struct{}{})
				close(completed)
				_, _, _ = c.Read(ctx)
			}))
			defer chrome.Close()
			owner := httptest.NewServer(New("ws"+strings.TrimPrefix(chrome.URL, "http"), func() string { return "browser" }))
			defer owner.Close()
			a := claim(t, ctx, owner.URL, "browser", true)
			defer a.CloseNow()
			require.NoError(t, a.Write(ctx, websocket.MessageText, []byte(`{"id":2,"method":"Runtime.evaluate","params":{}}`)))
			<-entered
			b := claim(t, ctx, owner.URL, "browser", false)
			if b != nil {
				b.CloseNow()
			}
			if loss {
				a.CloseNow()
			}
			close(resume)
			<-completed
			if loss {
				b = claim(t, ctx, owner.URL, "browser", false)
				if b != nil {
					b.CloseNow()
				}
			} else {
				_, _, err := a.Read(ctx)
				require.NoError(t, err)
				require.NoError(t, a.Write(ctx, websocket.MessageText, []byte(`{"id":3,"method":"Kernel.vaultFill.end"}`)))
				_, _, err = a.Read(ctx)
				require.NoError(t, err)
				// A released channel is terminal, not a reusable lease.
				_ = a.Write(ctx, websocket.MessageText, []byte(`{"id":4,"method":"Runtime.evaluate","params":{}}`))
				_, _, err = a.Read(ctx)
				require.Error(t, err)
				b = claim(t, ctx, owner.URL, "browser", true)
				b.CloseNow()
			}
			require.EqualValues(t, 1, writes.Load())
		})
	}
}

func claim(t *testing.T, ctx context.Context, base, instance string, ready bool) *websocket.Conn {
	t.Helper()
	c, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(base, "http"), nil)
	if err != nil {
		require.False(t, ready)
		require.NotNil(t, response)
		require.Equal(t, http.StatusConflict, response.StatusCode)
		return nil
	}
	raw, err := json.Marshal(map[string]any{"id": 1, "method": "Kernel.vaultFill.begin", "params": map[string]string{"protocol": Protocol, "nonce": strings.Repeat("a", 32), "instance": instance}})
	require.NoError(t, err)
	require.NoError(t, c.Write(ctx, websocket.MessageText, raw))
	_, data, err := c.Read(ctx)
	if ready {
		require.NoError(t, err)
		require.Contains(t, string(data), Protocol)
	} else {
		require.Error(t, err)
	}
	return c
}

func TestIdentityCannotBeAdoptedInPlace(t *testing.T) {
	for _, born := range []string{"", "template"} {
		t.Run("born-"+born, func(t *testing.T) {
			var identity atomic.Value
			identity.Store(born)
			owner := httptest.NewServer(New("ws://127.0.0.1:1", func() string { return identity.Load().(string) }))
			defer owner.Close()
			identity.Store("fork")
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			c := claim(t, ctx, owner.URL, "fork", false)
			if c != nil {
				c.CloseNow()
			}
		})
	}
}
