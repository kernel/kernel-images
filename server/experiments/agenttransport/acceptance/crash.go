package acceptance

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	transport "github.com/kernel/kernel-images/server/experiments/agenttransport"
)

func RunPermissionCrash(t *testing.T, factory Factory) {
	t.Run("agent-crash-during-permission", func(t *testing.T) {
		fixture := factory(t)
		timeout := fixture.Timeout
		if timeout == 0 {
			timeout = 30 * time.Second
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		command := transport.Command{ID: "permission-crash", Prompt: fixture.Prompt}
		data, _ := json.Marshal(command)
		post(t, ctx, fixture.URL, data)
		waitPermission(t, ctx, fixture.URL, command.ID)
		fixture.Kill()
		waitState(t, ctx, fixture.URL, command.ID, "uncertain")
		if len(snapshot(t, ctx, fixture.URL).Permissions) != 0 {
			t.Fatal("crashed agent left an actionable permission")
		}
		post(t, ctx, fixture.URL, data)
		calls, err := fixture.Probe.Count()
		if err != nil {
			t.Fatal(err)
		}
		if calls != 0 || fixture.Dispatches() != 1 {
			t.Fatalf("crashed permission executed: tools=%d dispatches=%d", calls, fixture.Dispatches())
		}
	})
}

func RunCrash(t *testing.T, factory Factory) {
	t.Run("agent-crash-fences-session", func(t *testing.T) {
		fixture := factory(t)
		timeout := fixture.Timeout
		if timeout == 0 {
			timeout = 30 * time.Second
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		command := transport.Command{ID: "crash-operation", Prompt: fixture.Prompt}
		data, _ := json.Marshal(command)
		post(t, ctx, fixture.URL, data)
		if err := fixture.Probe.Wait(ctx); err != nil {
			t.Fatal(err)
		}
		fixture.Kill()
		waitState(t, ctx, fixture.URL, command.ID, "uncertain")
		post(t, ctx, fixture.URL, data)
		// A new ID cannot bypass the uncertain-outcome fence.
		other, _ := json.Marshal(transport.Command{ID: "new-operation", Prompt: fixture.Prompt})
		request, _ := http.NewRequestWithContext(ctx, "POST", fixture.URL+"/commands", bytes.NewReader(other))
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 409 {
			t.Fatalf("uncertain session accepted new command: %d", response.StatusCode)
		}
		calls, err := fixture.Probe.Count()
		if err != nil {
			t.Fatal(err)
		}
		if calls != 1 || fixture.Dispatches() != 1 {
			t.Fatalf("crashed operation retried: tools=%d dispatches=%d", calls, fixture.Dispatches())
		}
	})
}
