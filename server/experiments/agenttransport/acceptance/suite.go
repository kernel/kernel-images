// Package acceptance is the shared black-box disconnect suite for harness
// adapters. Factories provide a real HTTP endpoint and a counted tool barrier.
package acceptance

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	transport "github.com/kernel/kernel-images/server/experiments/agenttransport"
	"github.com/kernel/kernel-images/server/experiments/agenttransport/probe"
)

type Fixture struct {
	URL         string
	Probe       probe.Probe
	Prompt      string
	Dispatches  func() int
	ForcedStops func() int
	ProcessID   func() int
	Kill        func()
	Timeout     time.Duration
}
type Factory func(*testing.T) Fixture

// Run uses exactly the same assertions for deterministic and real ACP peers.
// A factory must allocate a fresh agent, workspace, and journal per subtest.
func Run(t *testing.T, factory Factory) {
	for _, lostAck := range []bool{false, true} {
		name := "disconnect-mid-tool"
		if lostAck {
			name = "lost-prompt-acknowledgement"
		}
		t.Run(name, func(t *testing.T) {
			fixture := factory(t)
			timeout := fixture.Timeout
			if timeout == 0 {
				timeout = 30 * time.Second
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			processID := fixture.ProcessID()
			response, scanner := stream(t, ctx, fixture.URL, 0)
			command := transport.Command{ID: "acceptance-operation", Prompt: fixture.Prompt}
			data, _ := json.Marshal(command)
			if lostAck {
				endpoint, err := url.Parse(fixture.URL)
				if err != nil {
					t.Fatal(err)
				}
				if endpoint.Scheme != "http" {
					t.Fatal("lost-ack injector requires a loopback HTTP test server")
				}
				conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", endpoint.Host)
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				if deadline, ok := ctx.Deadline(); ok {
					_ = conn.SetDeadline(deadline)
				}
				if _, err := fmt.Fprintf(conn, "POST /commands HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", endpoint.Host, len(data), data); err != nil {
					t.Fatal(err)
				}
				if err := fixture.Probe.Wait(ctx); err != nil {
					t.Fatalf("agent never invoked checkpoint: %v", err)
				}
				conn.Close() // No response was ever read; the client cannot know acceptance.
			} else {
				post(t, ctx, fixture.URL, data)
				if err := fixture.Probe.Wait(ctx); err != nil {
					t.Fatalf("agent never invoked checkpoint: %v", err)
				}
			}
			before := snapshot(t, ctx, fixture.URL)
			prefix := readThrough(t, scanner, 0, before.Sequence)
			response.Body.Close()
			if before.Operations[command.ID].State != "running" {
				t.Fatalf("agent not running at detach: %+v", before)
			}
			post(t, ctx, fixture.URL, data)
			if fixture.Dispatches() != 1 {
				t.Fatalf("prompt dispatched %d times", fixture.Dispatches())
			}
			if err := fixture.Probe.Release(); err != nil {
				t.Fatal(err)
			}
			// Wait for durable completion while no SSE client is attached. This proves
			// offline output replay rather than merely reconnecting before output exists.
			after := waitCompleted(t, ctx, fixture.URL, command.ID)
			if fixture.ProcessID() != processID {
				t.Fatal("detach replaced the ACP process")
			}
			response, resumed := stream(t, ctx, fixture.URL, before.Sequence)
			suffix := readThrough(t, resumed, before.Sequence, after.Sequence)
			response.Body.Close()
			response, full := stream(t, ctx, fixture.URL, 0)
			all := readThrough(t, full, 0, after.Sequence)
			response.Body.Close()
			if !reflect.DeepEqual(append(prefix, suffix...), all) {
				t.Fatal("resumed stream differs from full replay")
			}
			acpEvents := 0
			for _, event := range all {
				if event.Kind == "acp" {
					acpEvents++
					if !json.Valid(event.Payload) {
						t.Fatal("invalid preserved ACP payload")
					}
				}
			}
			if acpEvents == 0 {
				t.Fatal("no ACP updates were preserved")
			}
			if all[len(all)-1].Kind != "completed" {
				t.Fatal("missing terminal completion")
			}
			post(t, ctx, fixture.URL, data)
			final := snapshot(t, ctx, fixture.URL)
			if final.Sequence != after.Sequence {
				t.Fatal("completed retry appended new events")
			}
			calls, err := fixture.Probe.Count()
			if err != nil {
				t.Fatal(err)
			}
			if calls != 1 || fixture.Dispatches() != 1 {
				t.Fatalf("dispatches=%d tool executions=%d", fixture.Dispatches(), calls)
			}
			t.Logf("PASS: dispatches=1 tool_executions=1 pid=%d events=%d replayed=%d", processID, after.Sequence, len(suffix))
		})
	}
}
func post(t *testing.T, ctx context.Context, url string, data []byte) {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, "POST", url+"/commands", bytes.NewReader(data))
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 202 {
		t.Fatalf("submit status %d", response.StatusCode)
	}
	_, _ = io.Copy(io.Discard, response.Body)
}
func snapshot(t *testing.T, ctx context.Context, url string) transport.Snapshot {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, "GET", url+"/snapshot", nil)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("snapshot status %d", response.StatusCode)
	}
	var result transport.Snapshot
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	return result
}
func waitCompleted(t *testing.T, ctx context.Context, url, id string) transport.Snapshot {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		state := snapshot(t, ctx, url)
		switch state.Operations[id].State {
		case "completed":
			return state
		case "failed", "cancelled", "uncertain":
			t.Fatalf("operation ended as %s", state.Operations[id].State)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
}
func stream(t *testing.T, ctx context.Context, url string, cursor int) (*http.Response, *bufio.Scanner) {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, "GET", url+"/events", nil)
	req.Header.Set("Last-Event-ID", strconv.Itoa(cursor))
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { response.Body.Close() })
	if response.StatusCode != 200 {
		t.Fatalf("stream status %d", response.StatusCode)
	}
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 4096), 16<<20)
	return response, scanner
}
func readThrough(t *testing.T, scanner *bufio.Scanner, cursor, end int) []transport.Event {
	t.Helper()
	events := make([]transport.Event, 0, end-cursor)
	id := ""
	for cursor < end && scanner.Scan() {
		if value, ok := strings.CutPrefix(scanner.Text(), "id: "); ok {
			id = value
		}
		if value, ok := strings.CutPrefix(scanner.Text(), "data: "); ok {
			var event transport.Event
			if err := json.Unmarshal([]byte(value), &event); err != nil {
				t.Fatal(err)
			}
			if event.Sequence != cursor+1 || id != strconv.Itoa(event.Sequence) {
				t.Fatalf("gap or duplicate: cursor=%d id=%s event=%+v", cursor, id, event)
			}
			events = append(events, event)
			cursor++
			id = ""
		}
	}
	if cursor != end {
		t.Fatalf("stream ended at %d before %d: %v", cursor, end, scanner.Err())
	}
	return events
}
