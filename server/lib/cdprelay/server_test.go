package cdprelay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

const testToken = "0123456789abcdef0123456789abcdef"
const testID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type fixture struct {
	url         string
	conn        chan *websocket.Conn
	commands    atomic.Int32
	connections atomic.Int32
	server      *Server
}

func setup(t *testing.T, opts Options) *fixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	f := &fixture{conn: make(chan *websocket.Conn, 8)}
	chrome := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		f.connections.Add(1)
		f.conn <- c
		for {
			_, data, err := c.Read(r.Context())
			if err != nil {
				return
			}
			f.commands.Add(1)
			var m map[string]any
			if json.Unmarshal(data, &m) != nil {
				return
			}
			if m["method"] == "wait" {
				continue
			}
			delete(m, "method")
			m["result"] = map[string]any{"value": f.commands.Load()}
			response, _ := json.Marshal(m)
			if c.Write(r.Context(), websocket.MessageText, response) != nil {
				return
			}
		}
	}))
	t.Cleanup(chrome.Close)
	opts.Token = testToken
	opts.UpstreamURL = strings.Replace(chrome.URL, "http:", "ws:", 1)
	opts.PollWait = 10 * time.Millisecond
	var err error
	f.server, err = New(ctx, opts)
	require.NoError(t, err)
	s := httptest.NewServer(f.server)
	t.Cleanup(s.Close)
	f.url = s.URL + Prefix + testID
	return f
}

func invoke(t *testing.T, method, url, token string, body any, expected int, out any) {
	t.Helper()
	var b bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&b).Encode(body))
	}
	req, err := http.NewRequest(method, url, &b)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, expected, resp.StatusCode)
	if out != nil {
		require.NoError(t, json.NewDecoder(resp.Body).Decode(out))
	}
}

func TestRetainReplayAndFence(t *testing.T) {
	f := setup(t, Options{})
	var current state
	invoke(t, "PUT", f.url, testToken, nil, 200, &current)
	invoke(t, "PUT", f.url, testToken, nil, 200, &current)
	require.Equal(t, int32(1), f.connections.Load())
	conn := <-f.conn
	command := request{Seq: 1, Data: json.RawMessage(`{"id":1,"sessionId":"page","method":"evaluate"}`)}
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); invoke(t, "POST", f.url+"/send", testToken, command, 200, nil) }()
	}
	wg.Wait()
	var frames []frame
	invoke(t, "GET", f.url+"/events?epoch=0&ack=0", testToken, nil, 200, &frames)
	require.Len(t, frames, 1)
	require.Equal(t, int32(1), f.commands.Load())
	var replay []frame
	invoke(t, "GET", f.url+"/events?epoch=0&ack=0", testToken, nil, 200, &replay)
	require.Equal(t, frames, replay)
	invoke(t, "POST", f.url+"/send", testToken, request{Seq: 1, Data: json.RawMessage(`{"id":2,"method":"different"}`)}, 409, nil)
	invoke(t, "POST", f.url+"/park", testToken, request{Seq: 1}, 423, nil)
	park := request{Seq: 1, Ack: 1}
	invoke(t, "POST", f.url+"/park", testToken, park, 200, &current)
	require.Equal(t, uint64(1), current.Epoch)
	invoke(t, "POST", f.url+"/park", testToken, park, 200, &current)
	invoke(t, "POST", f.url+"/send", testToken, request{Seq: 2, Data: command.Data}, 409, nil)
	invoke(t, "GET", f.url+"/events?epoch=0&ack=1", testToken, nil, 409, nil)
	// An event racing after the park boundary is retained, not dropped.
	require.NoError(t, conn.Write(context.Background(), websocket.MessageText, []byte(`{"method":"late-event"}`)))
	resume := request{Epoch: 1, Seq: 1, Ack: 1}
	invoke(t, "POST", f.url+"/resume", testToken, resume, 200, &current)
	invoke(t, "POST", f.url+"/resume", testToken, resume, 200, &current)
	require.Equal(t, uint64(2), current.Epoch)
	invoke(t, "POST", f.url+"/park", testToken, park, 409, nil)
	invoke(t, "GET", f.url+"/events?epoch=2&ack=1", testToken, nil, 200, &frames)
	require.Len(t, frames, 1)
	require.Equal(t, uint64(2), frames[0].Seq)
	invoke(t, "POST", f.url+"/send", testToken, request{Epoch: 2, Seq: 2, Data: command.Data}, 200, nil)
	require.Eventually(t, func() bool { return f.commands.Load() == 2 }, time.Second, time.Millisecond)
	require.Equal(t, int32(1), f.connections.Load())
	invoke(t, "DELETE", f.url, testToken, nil, 204, nil)
	invoke(t, "DELETE", f.url, testToken, nil, 204, nil)
	invoke(t, "POST", f.url+"/resume", testToken, resume, 404, nil)
}

func TestNestedCDPDisablesParking(t *testing.T) {
	f := setup(t, Options{})
	invoke(t, "PUT", f.url, testToken, nil, 200, nil)
	invoke(t, "POST", f.url+"/send", testToken, request{Seq: 1, Data: json.RawMessage(`{"id":1,"method":"Target.sendMessageToTarget"}`)}, 200, nil)
	var frames []frame
	invoke(t, "GET", f.url+"/events?epoch=0&ack=0", testToken, nil, 200, &frames)
	require.Len(t, frames, 1)
	invoke(t, "POST", f.url+"/park", testToken, request{Seq: 1, Ack: 1}, 423, nil)
}

func TestPendingCommandPreventsPark(t *testing.T) {
	f := setup(t, Options{})
	invoke(t, "PUT", f.url, testToken, nil, 200, nil)
	invoke(t, "POST", f.url+"/send", testToken, request{Seq: 1, Data: json.RawMessage(`{"id":1,"method":"wait"}`)}, 200, nil)
	invoke(t, "POST", f.url+"/park", testToken, request{Seq: 1}, 423, nil)
}

func TestAuthCapacityAndGone(t *testing.T) {
	f := setup(t, Options{MaxSessions: 1})
	invoke(t, "PUT", f.url, "wrong", nil, 401, nil)
	invoke(t, "PUT", f.url, testToken, nil, 200, nil)
	invoke(t, "PUT", strings.TrimSuffix(f.url, testID)+strings.Repeat("b", 64), testToken, nil, 423, nil)
	conn := <-f.conn
	conn.CloseNow()
	require.Eventually(t, func() bool {
		f.server.mu.Lock()
		defer f.server.mu.Unlock()
		sess := f.server.sessions[testID]
		if sess == nil {
			return true
		}
		sess.mu.Lock()
		defer sess.mu.Unlock()
		return sess.closed
	}, time.Second, time.Millisecond)
	// Creation is idempotent, not an instruction to replace a dead Chrome session.
	invoke(t, "PUT", f.url, testToken, nil, 410, nil)
	require.Equal(t, int32(1), f.connections.Load())
	invoke(t, "DELETE", f.url, testToken, nil, 204, nil)
	invoke(t, "GET", f.url+"/events?epoch=0&ack=0", testToken, nil, 404, nil)
}

func TestParkedSessionDoesNotExpireWithTransportSilence(t *testing.T) {
	f := setup(t, Options{LeaseTTL: 50 * time.Millisecond})
	invoke(t, "PUT", f.url, testToken, nil, 200, nil)
	invoke(t, "POST", f.url+"/park", testToken, request{}, 200, nil)
	time.Sleep(150 * time.Millisecond)
	invoke(t, "POST", f.url+"/resume", testToken, request{Epoch: 1}, 200, nil)
	require.Equal(t, int32(1), f.connections.Load())
}

func TestAbandonedActiveSessionExpires(t *testing.T) {
	f := setup(t, Options{LeaseTTL: 50 * time.Millisecond})
	invoke(t, "PUT", f.url, testToken, nil, 200, nil)
	require.Eventually(t, func() bool {
		f.server.mu.Lock()
		defer f.server.mu.Unlock()
		return len(f.server.sessions) == 0
	}, time.Second, 10*time.Millisecond)
	invoke(t, "GET", f.url+"/events?epoch=0&ack=0", testToken, nil, 404, nil)
}

func TestPendingCommandBound(t *testing.T) {
	f := setup(t, Options{})
	invoke(t, "PUT", f.url, testToken, nil, 200, nil)
	f.server.mu.Lock()
	sess := f.server.sessions[testID]
	f.server.mu.Unlock()
	sess.mu.Lock()
	for i := range 1024 {
		sess.pending[fmt.Sprint(i)] = struct{}{}
	}
	sess.mu.Unlock()
	invoke(t, "POST", f.url+"/send", testToken, request{Seq: 1, Data: json.RawMessage(`{"id":1,"method":"evaluate"}`)}, 423, nil)
	require.Zero(t, f.commands.Load())
}

func TestBufferOverflowIsTerminal(t *testing.T) {
	f := setup(t, Options{MaxBufferBytes: 128})
	invoke(t, "PUT", f.url, testToken, nil, 200, nil)
	conn := <-f.conn
	for i := 0; i < 3; i++ {
		_ = conn.Write(context.Background(), websocket.MessageText, []byte(fmt.Sprintf(`{"method":"event","padding":"%s"}`, strings.Repeat("x", 50))))
	}
	require.Eventually(t, func() bool {
		f.server.mu.Lock()
		sess := f.server.sessions[testID]
		f.server.mu.Unlock()
		sess.mu.Lock()
		defer sess.mu.Unlock()
		return sess.closed
	}, time.Second, time.Millisecond)
	invoke(t, "GET", f.url+"/events?epoch=0&ack=0", testToken, nil, 410, nil)
}
