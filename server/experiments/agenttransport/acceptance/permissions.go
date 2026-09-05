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

// RunPermissions requires a harness configured to ask permission before the
// checkpoint. Never enable approval bypasses for this suite.
func RunPermissions(t *testing.T, factory Factory) {
	for _, cancelWork := range []bool{false, true} {
		name := "permission-detach-and-retry"
		if cancelWork {
			name = "cancel-pending-permission"
		}
		t.Run(name, func(t *testing.T) {
			fixture := factory(t)
			timeout := fixture.Timeout
			if timeout == 0 {
				timeout = 30 * time.Second
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			pid := fixture.ProcessID()
			response, scanner := stream(t, ctx, fixture.URL, 0)
			command := transport.Command{ID: "permission-operation", Prompt: fixture.Prompt}
			data, _ := json.Marshal(command)
			post(t, ctx, fixture.URL, data)
			state := waitPermission(t, ctx, fixture.URL, command.ID)
			readThrough(t, scanner, 0, state.Sequence)
			response.Body.Close()
			var pending transport.PendingPermission
			for _, request := range state.Permissions {
				pending = request
			}
			calls, err := fixture.Probe.Count()
			if err != nil {
				t.Fatal(err)
			}
			if calls != 0 {
				t.Fatal("tool ran before permission")
			}
			reattached := snapshot(t, ctx, fixture.URL)
			if reattached.Permissions[pending.RequestID].RequestID != pending.RequestID {
				t.Fatal("permission disappeared on detach")
			}
			if fixture.ProcessID() != pid {
				t.Fatal("agent process changed on detach")
			}
			if cancelWork {
				control := transport.Control{ID: "cancel-1", OperationID: command.ID}
				controlPost(t, ctx, fixture.URL, "/cancel", control, 202)
				waitState(t, ctx, fixture.URL, command.ID, "cancelled")
				controlPost(t, ctx, fixture.URL, "/cancel", control, 202)
				calls, err := fixture.Probe.Count()
				if err != nil {
					t.Fatal(err)
				}
				if calls != 0 {
					t.Fatal("cancelled tool ran")
				}
				end := snapshot(t, ctx, fixture.URL)
				if len(end.Permissions) != 0 {
					t.Fatal("cancelled permission remains pending")
				}
				response, resumed := stream(t, ctx, fixture.URL, state.Sequence)
				events := readThrough(t, resumed, state.Sequence, end.Sequence)
				response.Body.Close()
				if len(events) == 0 || events[len(events)-1].Kind != "cancelled" {
					t.Fatal("missing replayed cancellation")
				}
				return
			}
			var request struct {
				Options []struct {
					OptionID string `json:"optionId"`
					Kind     string `json:"kind"`
				} `json:"options"`
			}
			if err := json.Unmarshal(pending.Payload, &request); err != nil {
				t.Fatal(err)
			}
			option := ""
			for _, choice := range request.Options {
				if choice.Kind == "allow_once" {
					option = choice.OptionID
					break
				}
			}
			if option == "" {
				t.Fatal("harness did not offer allow_once; do not silently grant broader permission")
			}
			if err := fixture.Probe.Release(); err != nil {
				t.Fatal(err)
			}
			decision := transport.Control{ID: "decision-1", OperationID: command.ID, RequestID: pending.RequestID, OptionID: option}
			controlPost(t, ctx, fixture.URL, "/permissions", decision, 202)
			controlPost(t, ctx, fixture.URL, "/permissions", decision, 202)
			conflicting := decision
			conflicting.OptionID = "invalid-choice"
			controlPost(t, ctx, fixture.URL, "/permissions", conflicting, 409)
			end := waitCompleted(t, ctx, fixture.URL, command.ID)
			response, scanner = stream(t, ctx, fixture.URL, state.Sequence)
			events := readThrough(t, scanner, state.Sequence, end.Sequence)
			response.Body.Close()
			resolutions := 0
			for _, event := range events {
				if event.Kind == "permission_resolved" {
					resolutions++
				}
			}
			calls, err = fixture.Probe.Count()
			if err != nil {
				t.Fatal(err)
			}
			if calls != 1 || resolutions != 1 || fixture.Dispatches() != 1 {
				t.Fatalf("tools=%d decisions=%d dispatches=%d", calls, resolutions, fixture.Dispatches())
			}
		})
	}
}
func controlPost(t *testing.T, ctx context.Context, url, path string, c transport.Control, status int) {
	t.Helper()
	data, _ := json.Marshal(c)
	request, _ := http.NewRequestWithContext(ctx, "POST", url+path, bytes.NewReader(data))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != status {
		t.Fatalf("%s: got %d want %d", path, response.StatusCode, status)
	}
}
func waitPermission(t *testing.T, ctx context.Context, url, id string) transport.Snapshot {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		state := snapshot(t, ctx, url)
		if len(state.Permissions) == 1 {
			return state
		}
		if state.Operations[id].State != "running" {
			t.Fatalf("operation ended without permission: %+v", state.Operations[id])
		}
		select {
		case <-ctx.Done():
			t.Fatal("harness did not request permission before timeout")
		case <-ticker.C:
		}
	}
}
func waitState(t *testing.T, ctx context.Context, url, id, want string) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		state := snapshot(t, ctx, url).Operations[id].State
		if state == want {
			return
		}
		if state != "running" && state != "cancelling" {
			t.Fatalf("got state %s want %s", state, want)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
}
