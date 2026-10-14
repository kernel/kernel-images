package livefocus

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// noBrowser has no DevTools URL, so the watcher runs without connecting.
type noBrowser struct{}

func (noBrowser) Current() string { return "" }

func running(w *Watcher) (context.Context, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.runCtx, w.runCtx != nil
}

func receive(t *testing.T, states <-chan []byte) State {
	t.Helper()
	select {
	case data := <-states:
		var state State
		require.NoError(t, json.Unmarshal(data, &state))
		return state
	case <-time.After(time.Second):
		t.Fatal("no state")
		return State{}
	}
}

func TestSubscribersGetTheCurrentStateThenTheNewest(t *testing.T) {
	w := New(noBrowser{}, slog.Default())
	defer w.Close()
	states, unsubscribe := w.Subscribe()
	defer unsubscribe()
	require.Equal(t, emptyState(), receive(t, states))

	ctx, ok := running(w)
	require.True(t, ok)
	w.publish(ctx, State{Focus: &Field{Type: "text"}, Fields: []Field{}})
	w.publish(ctx, State{Focus: &Field{Type: "email"}, Fields: []Field{}})
	require.Equal(t, "email", receive(t, states).Focus.Type, "a slow subscriber gets only the newest state")

	late, unsubscribeLate := w.Subscribe()
	defer unsubscribeLate()
	require.Equal(t, "email", receive(t, late).Focus.Type)
}

func TestWatcherStopsAfterTheLastSubscriberLingers(t *testing.T) {
	w := New(noBrowser{}, slog.Default())
	w.linger = 20 * time.Millisecond
	defer w.Close()

	_, unsubscribe := w.Subscribe()
	unsubscribe()
	_, ok := running(w)
	require.True(t, ok, "keeps running for a live view that reconnects")
	require.Eventually(t, func() bool {
		_, ok := running(w)
		return !ok
	}, time.Second, 5*time.Millisecond)

	// a stopped run cannot publish into the next one
	w.publish(cancelled(), State{Focus: &Field{Type: "text"}, Fields: []Field{}})
	states, unsubscribe := w.Subscribe()
	defer unsubscribe()
	require.Nil(t, receive(t, states).Focus)
}

func TestResubscribingCancelsTheLinger(t *testing.T) {
	w := New(noBrowser{}, slog.Default())
	w.linger = 20 * time.Millisecond
	defer w.Close()

	_, unsubscribe := w.Subscribe()
	unsubscribe()
	_, unsubscribeAgain := w.Subscribe()
	defer unsubscribeAgain()
	time.Sleep(60 * time.Millisecond)
	_, ok := running(w)
	require.True(t, ok)
}

func cancelled() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func TestServeHTTPStreamsStates(t *testing.T) {
	w := New(noBrowser{}, slog.Default())
	defer w.Close()
	server := httptest.NewServer(w)
	defer server.Close()

	resp, err := http.Get(server.URL)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	lines := bufio.NewReader(resp.Body)
	line, err := lines.ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, `data: {"focus":null,"fields":[],"viewport":null,"pageFocused":false,"screen":null}`, strings.TrimSpace(line))

	ctx, ok := running(w)
	require.True(t, ok)
	w.publish(ctx, State{Focus: &Field{Type: "password"}, Fields: []Field{}})
	_, err = lines.ReadString('\n')
	require.NoError(t, err)
	line, err = lines.ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, `data: {"focus":{"type":"password"},"fields":[],"viewport":null,"pageFocused":false,"screen":null}`, strings.TrimSpace(line))
}
