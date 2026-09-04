package agenttransport

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type controlledRunner struct {
	executions atomic.Int32
	started    chan struct{}
	release    chan struct{}
	once       sync.Once
}

func (r *controlledRunner) Run(ctx context.Context, prompt string, emit func(string)) error {
	r.executions.Add(1)
	r.once.Do(func() { close(r.started) })
	emit("before disconnect")
	select {
	case <-r.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	emit("after disconnect")
	return nil
}

func setup(t *testing.T) (*controlledRunner, *httptest.Server) {
	t.Helper()
	runner := &controlledRunner{started: make(chan struct{}), release: make(chan struct{})}
	runtime := NewReference(runner)
	server := httptest.NewServer(runtime)
	t.Cleanup(func() { runtime.Close(); server.Close() })
	return runner, server
}

func submit(t *testing.T, server *httptest.Server, command Command, status int) {
	t.Helper()
	body, _ := json.Marshal(command)
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Post(server.URL+"/commands", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != status {
		t.Fatalf("submit: got %d, want %d", response.StatusCode, status)
	}
}

func subscribe(t *testing.T, server *httptest.Server, cursor int) (*http.Response, *bufio.Scanner) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/events", nil)
	req.Header.Set("Last-Event-ID", fmt.Sprint(cursor))
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { response.Body.Close() })
	if response.StatusCode != http.StatusOK {
		t.Fatalf("subscribe: %d", response.StatusCode)
	}
	return response, bufio.NewScanner(response.Body)
}

func next(t *testing.T, scanner *bufio.Scanner) Event {
	t.Helper()
	for scanner.Scan() {
		if data, ok := strings.CutPrefix(scanner.Text(), "data: "); ok {
			var event Event
			if err := json.Unmarshal([]byte(data), &event); err != nil {
				t.Fatal(err)
			}
			return event
		}
	}
	t.Fatalf("stream ended before expected event: %v", scanner.Err())
	return Event{}
}

func waitStarted(t *testing.T, runner *controlledRunner) {
	t.Helper()
	select {
	case <-runner.started:
	case <-time.After(5 * time.Second):
		t.Fatal("runner did not start")
	}
}

func TestDisconnectRetryAndReplay(t *testing.T) {
	runner, server := setup(t)
	response, scanner := subscribe(t, server, 0)
	command := Command{"op-1", "perform a counted operation"}
	submit(t, server, command, http.StatusAccepted)
	first := next(t, scanner)
	second := next(t, scanner)
	if first.Sequence != 1 || first.Kind != "accepted" || second.Sequence != 2 || second.Text != "before disconnect" {
		t.Fatalf("unexpected initial events: %+v %+v", first, second)
	}
	// Close the real HTTP stream while the runner remains blocked mid-turn.
	response.Body.Close()
	close(runner.release)
	submit(t, server, command, http.StatusAccepted)
	_, reconnected := subscribe(t, server, second.Sequence)
	output, done := next(t, reconnected), next(t, reconnected)
	if output.Sequence != 3 || output.Text != "after disconnect" || done.Sequence != 4 || done.Kind != "completed" {
		t.Fatalf("unexpected replay: %+v %+v", output, done)
	}
	if runner.executions.Load() != 1 {
		t.Fatalf("executions: %d", runner.executions.Load())
	}
	// A retry after completion must also be harmless; full replay has one acceptance.
	submit(t, server, command, http.StatusAccepted)
	_, full := subscribe(t, server, 0)
	for sequence := 1; sequence <= 4; sequence++ {
		if event := next(t, full); event.Sequence != sequence {
			t.Fatalf("sequence: %+v", event)
		}
	}
	if runner.executions.Load() != 1 {
		t.Fatal("completed operation executed again")
	}
}

func TestLostSubmitResponseDoesNotRepeatExecution(t *testing.T) {
	runner, server := setup(t)
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(server.URL, "http://"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	command := Command{"lost-ack", "perform a counted operation"}
	body, _ := json.Marshal(command)
	// Submit over TCP and deliberately never read the acknowledgement.
	if _, err := fmt.Fprintf(conn, "POST /commands HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body); err != nil {
		t.Fatal(err)
	}
	waitStarted(t, runner)
	conn.Close()
	submit(t, server, command, http.StatusAccepted)
	close(runner.release)
	_, scanner := subscribe(t, server, 0)
	for sequence := 1; sequence <= 4; sequence++ {
		event := next(t, scanner)
		if event.Sequence != sequence || event.OperationID != command.ID {
			t.Fatalf("unexpected event: %+v", event)
		}
		if sequence == 4 && event.Kind != "completed" {
			t.Fatalf("not completed: %+v", event)
		}
	}
	if runner.executions.Load() != 1 {
		t.Fatalf("executions: %d", runner.executions.Load())
	}
}

func TestConcurrentRetriesAndPayloadConflict(t *testing.T) {
	runner, server := setup(t)
	command := Command{"same-id", "original"}
	var group sync.WaitGroup
	for i := 0; i < 20; i++ {
		group.Add(1)
		go func() { defer group.Done(); submit(t, server, command, http.StatusAccepted) }()
	}
	group.Wait()
	waitStarted(t, runner)
	submit(t, server, Command{"same-id", "different"}, http.StatusConflict)
	if runner.executions.Load() != 1 {
		t.Fatalf("executions: %d", runner.executions.Load())
	}
	close(runner.release)
}

func TestInvalidReplayCursor(t *testing.T) {
	_, server := setup(t)
	for _, test := range []struct {
		cursor string
		status int
	}{{"-1", 400}, {"invalid", 400}, {"1", 409}} {
		t.Run(test.cursor, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, server.URL+"/events", nil)
			req.Header.Set("Last-Event-ID", test.cursor)
			response, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != test.status {
				t.Fatalf("got %d, want %d", response.StatusCode, test.status)
			}
		})
	}
}
