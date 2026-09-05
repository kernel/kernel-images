package agenttransport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestContentRetryAndOwnership(t *testing.T) {
	runner, server := setup(t)
	original := json.RawMessage(`[{"type":"text","text":"hello","_meta":{"n":9007199254740993}}]`)
	command := Command{"content", original}
	submit(t, server, command, 202)
	waitStarted(t, runner)
	command.Prompt = json.RawMessage(`[ { "_meta": {"n":9007199254740993}, "text":"hello", "type":"text" } ]`)
	submit(t, server, command, 202)
	command.Prompt = json.RawMessage(`[{"type":"text","text":"hello","_meta":{"n":9007199254740994}}]`)
	submit(t, server, command, 409)
	if runner.executions.Load() != 1 {
		t.Fatal("content retry dispatched twice")
	}
	close(runner.release)

	// Neither the caller nor a runner may mutate the accepted command in memory.
	runtime := NewReference(runnerFunc(func(_ context.Context, prompt json.RawMessage, _ *Turn) error {
		prompt[0] = 'x'
		return nil
	}))
	defer runtime.Close()
	op, err := runtime.Submit(Command{"owned", original})
	if err != nil {
		t.Fatal(err)
	}
	original[0] = 'x'
	op.Command.Prompt[0] = 'x'
	canonical := json.RawMessage(`[{"_meta":{"n":9007199254740993},"text":"hello","type":"text"}]`)
	if _, err := runtime.Submit(Command{"owned", canonical}); err != nil {
		t.Fatalf("accepted command was mutated: %v", err)
	}
	runtime.Close()
	if !bytes.Equal(runtime.operations["owned"].Command.Prompt, canonical) {
		t.Fatal("runner mutated durable command")
	}
}

func TestInvalidPromptContent(t *testing.T) {
	_, server := setup(t)
	for _, prompt := range []string{`"legacy text"`, `null`, `[]`, `{}`, `[null]`, `[1]`, `[{}]`, `[{"type":1}]`} {
		t.Run(prompt, func(t *testing.T) {
			submit(t, server, Command{"invalid", json.RawMessage(prompt)}, http.StatusBadRequest)
		})
	}
	if state := snapshot(t, server.URL); state.Sequence != 0 || len(state.Operations) != 0 {
		t.Fatal("invalid content was accepted")
	}
	if _, err := normalizePrompt(json.RawMessage(`[{"type":"text"}] []`)); !errors.Is(err, ErrInvalidCommand) {
		t.Fatal("trailing JSON accepted")
	}
}

func TestPromptSizeLimit(t *testing.T) {
	runtime := NewReference(runnerFunc(func(context.Context, json.RawMessage, *Turn) error {
		t.Error("oversized prompt dispatched")
		return nil
	}))
	server := httptest.NewServer(runtime)
	defer func() { runtime.Close(); server.Close() }()
	submit(t, server, Command{"oversized", TextPrompt(strings.Repeat("a", MaxPromptBytes))}, 400)
	if state := snapshot(t, server.URL); state.Sequence != 0 {
		t.Fatal("oversized prompt journaled")
	}
	expanded := json.RawMessage(`[{"type":"text","text":"` + strings.Repeat("<", MaxPromptBytes/6) + `"}]`)
	if _, err := normalizePrompt(expanded); !errors.Is(err, ErrInvalidCommand) {
		t.Fatal("canonical JSON exceeded the limit without rejection")
	}
}
