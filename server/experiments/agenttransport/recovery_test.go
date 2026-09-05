package agenttransport

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

type runnerFunc func(context.Context, json.RawMessage, *Turn) error

func (f runnerFunc) Run(ctx context.Context, prompt json.RawMessage, turn *Turn) error {
	return f(ctx, prompt, turn)
}

func snapshot(t *testing.T, url string) Snapshot {
	t.Helper()
	response, err := (&http.Client{Timeout: 5 * time.Second}).Get(url + "/snapshot")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("snapshot: %d", response.StatusCode)
	}
	var result Snapshot
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	return result
}
func await(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		if condition() {
			return
		}
		select {
		case <-deadline:
			t.Fatal("condition timed out")
		case <-tick.C:
		}
	}
}
func postControl(t *testing.T, url, path string, c Control, status int) {
	t.Helper()
	data, _ := json.Marshal(c)
	req, err := http.NewRequest("POST", url+path, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != status {
		t.Fatalf("%s: got %d want %d", path, response.StatusCode, status)
	}
}

func TestPermissionDetachAndDecisionRetry(t *testing.T) {
	var effects atomic.Int32
	runtime := NewReference(runnerFunc(func(ctx context.Context, prompt json.RawMessage, turn *Turn) error {
		option, err := turn.Permission(ctx, "approval", json.RawMessage(`{"options":[{"optionId":"yes","kind":"allow_once","name":"Allow"},{"optionId":"no","kind":"reject_once","name":"Reject"}]}`))
		if err != nil {
			return err
		}
		if option == "yes" {
			effects.Add(1)
		}
		return nil
	}))
	server := httptest.NewServer(runtime)
	t.Cleanup(func() { runtime.Close(); server.Close() })
	response, scanner := subscribe(t, server, 0)
	submit(t, server, Command{"permission", TextPrompt("use a tool")}, 202)
	next(t, scanner)
	request := next(t, scanner)
	if request.Kind != "permission_request" {
		t.Fatalf("unexpected request %+v", request)
	}
	response.Body.Close()
	if effects.Load() != 0 {
		t.Fatal("permission was implicitly approved")
	}
	if len(snapshot(t, server.URL).Permissions) != 1 {
		t.Fatal("pending permission missing")
	}
	_, scanner = subscribe(t, server, 1)
	replay := next(t, scanner)
	if replay.RequestID != request.RequestID {
		t.Fatal("request identity changed")
	}
	decision := Control{"decision-1", "permission", request.RequestID, "yes"}
	invalid := decision
	invalid.OptionID = "invented"
	postControl(t, server.URL, "/permissions", invalid, 409)
	postControl(t, server.URL, "/permissions", decision, 202)
	postControl(t, server.URL, "/permissions", decision, 202)
	invalid = decision
	invalid.OptionID = "no"
	postControl(t, server.URL, "/permissions", invalid, 409)
	await(t, func() bool { return snapshot(t, server.URL).Operations["permission"].State == "completed" })
	if effects.Load() != 1 {
		t.Fatalf("effects: %d", effects.Load())
	}
	if len(snapshot(t, server.URL).Permissions) != 0 {
		t.Fatal("resolved permission retained")
	}
}

func TestCancelPendingPermissionAndRetry(t *testing.T) {
	var effects atomic.Int32
	runtime := NewReference(runnerFunc(func(ctx context.Context, prompt json.RawMessage, turn *Turn) error {
		_, err := turn.Permission(ctx, "approval", json.RawMessage(`{"options":[{"optionId":"yes"}]}`))
		if err == nil {
			effects.Add(1)
		}
		return err
	}))
	server := httptest.NewServer(runtime)
	t.Cleanup(func() { runtime.Close(); server.Close() })
	submit(t, server, Command{"cancel-me", TextPrompt("use a tool")}, 202)
	await(t, func() bool { return len(snapshot(t, server.URL).Permissions) == 1 })
	cancel := Control{ID: "cancel-1", OperationID: "cancel-me"}
	postControl(t, server.URL, "/cancel", cancel, 202)
	await(t, func() bool { return snapshot(t, server.URL).Operations["cancel-me"].State == "cancelled" })
	postControl(t, server.URL, "/cancel", cancel, 202)
	postControl(t, server.URL, "/permissions", Control{"late-decision", "cancel-me", `["cancel-me","approval"]`, "yes"}, 409)
	if effects.Load() != 0 {
		t.Fatal("cancelled permission executed")
	}
	_, scanner := subscribe(t, server, 0)
	for _, kind := range []string{"accepted", "permission_request", "cancel_requested", "cancelled"} {
		if e := next(t, scanner); e.Kind != kind {
			t.Fatalf("got %+v want %s", e, kind)
		}
	}
}

type failingStore struct {
	MemoryStore
	failAt int
	writes int
}

func (s *failingStore) Append(e Event) error {
	s.writes++
	if s.writes == s.failAt {
		return errors.New("injected disk failure")
	}
	return s.MemoryStore.Append(e)
}
func TestPersistenceFailurePreventsDispatch(t *testing.T) {
	var calls atomic.Int32
	store := &failingStore{failAt: 1}
	runtime, err := NewRuntime(runnerFunc(func(context.Context, json.RawMessage, *Turn) error { calls.Add(1); return nil }), store)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if _, err := runtime.Submit(Command{"op", TextPrompt("prompt")}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("got %v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("dispatched an uncommitted command")
	}
	if len(store.events) != 0 {
		t.Fatal("uncommitted event visible")
	}
}
func TestPermissionPersistenceFailureNeverApproves(t *testing.T) {
	var effects atomic.Int32
	store := &failingStore{failAt: 3}
	runtime, err := NewRuntime(runnerFunc(func(ctx context.Context, prompt json.RawMessage, turn *Turn) error {
		_, err := turn.Permission(ctx, "p", json.RawMessage(`{"options":[{"optionId":"yes"}]}`))
		if err == nil {
			effects.Add(1)
		}
		return err
	}), store)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(runtime)
	t.Cleanup(func() { runtime.Close(); server.Close() })
	submit(t, server, Command{"op", TextPrompt("prompt")}, 202)
	await(t, func() bool { return len(snapshot(t, server.URL).Permissions) == 1 })
	postControl(t, server.URL, "/permissions", Control{"decision", "op", `["op","p"]`, "yes"}, 503)
	runtime.Close()
	if effects.Load() != 0 {
		t.Fatal("approved an uncommitted decision")
	}
	if len(store.events) != 2 {
		t.Fatalf("uncommitted events visible: %d", len(store.events))
	}
}

// The helper is killed, not gracefully closed, so no shutdown event can mask an
// ambiguous dispatch. The journal is reopened by an entirely different process.
func TestCrashHelper(t *testing.T) {
	path, stage := os.Getenv("AGENT_JOURNAL_HELPER"), os.Getenv("AGENT_JOURNAL_STAGE")
	if path == "" {
		return
	}
	store, err := OpenFileStore(path)
	if err != nil {
		panic(err)
	}
	if stage == "accepted" {
		c := Command{"crash-op", TextPrompt("prompt")}
		if err := store.Append(Event{Sequence: 1, OperationID: c.ID, Kind: "accepted", Command: &c}); err != nil {
			panic(err)
		}
	} else {
		runtime, err := NewRuntime(runnerFunc(func(ctx context.Context, prompt json.RawMessage, turn *Turn) error {
			if err := os.WriteFile(path+".effect", []byte("executed"), 0600); err != nil {
				return err
			}
			if stage == "completed" {
				return nil
			}
			if stage == "permission" {
				_, err := turn.Permission(ctx, "p", json.RawMessage(`{"options":[{"optionId":"yes"}]}`))
				return err
			}
			if err := turn.Output("started"); err != nil {
				return err
			}
			<-ctx.Done()
			return ctx.Err()
		}), store)
		if err != nil {
			panic(err)
		}
		if _, err := runtime.Submit(Command{"crash-op", TextPrompt("prompt")}); err != nil {
			panic(err)
		}
		for {
			runtime.mu.Lock()
			ready := (stage == "completed" && runtime.operations["crash-op"].State == "completed") || (stage == "permission" && len(runtime.permissions) == 1) || (stage == "tool-start" && len(runtime.events) == 2)
			runtime.mu.Unlock()
			if ready {
				break
			}
			time.Sleep(time.Millisecond)
		}
	}
	fmt.Println("READY")
	select {}
}
func TestJournalRecoveryAfterProcessKill(t *testing.T) {
	for _, stage := range []string{"accepted", "tool-start", "permission", "completed"} {
		t.Run(stage, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "events.jsonl")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCrashHelper$")
			cmd.Env = append(os.Environ(), "AGENT_JOURNAL_HELPER="+path, "AGENT_JOURNAL_STAGE="+stage)
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			cmd.Stderr = os.Stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cmd.Process.Kill() })
			scanner := bufio.NewScanner(stdout)
			if !scanner.Scan() || scanner.Text() != "READY" {
				t.Fatalf("helper not ready: %v", scanner.Err())
			}
			if err := cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			_ = cmd.Wait()
			store, err := OpenFileStore(path)
			if err != nil {
				t.Fatal(err)
			}
			var replayed atomic.Int32
			runtime, err := NewRuntime(runnerFunc(func(context.Context, json.RawMessage, *Turn) error { replayed.Add(1); return nil }), store)
			if err != nil {
				store.Close()
				t.Fatal(err)
			}
			defer runtime.Close()
			op, err := runtime.Submit(Command{"crash-op", TextPrompt("prompt")})
			if err != nil {
				t.Fatal(err)
			}
			expected := "uncertain"
			if stage == "completed" {
				expected = "completed"
			}
			if op.State != expected {
				t.Fatalf("got %s want %s", op.State, expected)
			}
			if replayed.Load() != 0 {
				t.Fatal("automatically replayed a recovered operation")
			}
			if len(runtime.permissions) != 0 {
				t.Fatal("stale permission remains actionable after restart")
			}
			if expected == "uncertain" {
				if _, err := runtime.Submit(Command{"new", TextPrompt("new prompt")}); !errors.Is(err, ErrConflict) {
					t.Fatal("uncertain session accepted new work")
				}
			}
		})
	}
}

func TestJournalLockAndTornTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	store, err := OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if other, err := OpenFileStore(path); err == nil {
		other.Close()
		t.Fatal("second journal owner accepted")
	}
	c := Command{"op", TextPrompt("prompt")}
	if err := store.Append(Event{Sequence: 1, OperationID: "op", Kind: "accepted", Command: &c}); err != nil {
		t.Fatal(err)
	}
	store.Close()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteString(`{"sequence":2`)
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	store, err = OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	events, err := store.Load()
	store.Close()
	if err != nil || len(events) != 1 {
		t.Fatalf("torn tail recovery: %v %d", err, len(events))
	}
	if err := os.WriteFile(path, []byte("not-json\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if store, err := OpenFileStore(path); err == nil {
		store.Close()
		t.Fatal("complete corrupt record accepted")
	}
}
