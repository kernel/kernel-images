// Package agenttransport contains an experimental reconnect contract, not a
// production endpoint. State survives client disconnects, not process restarts.
package agenttransport

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
)

type Command struct {
	ID     string `json:"id"`
	Prompt string `json:"prompt"`
}

type Event struct {
	Sequence    int    `json:"sequence"`
	OperationID string `json:"operationId"`
	Kind        string `json:"kind"`
	Text        string `json:"text,omitempty"`
}

// Runner executes one accepted prompt. Its context belongs to the runtime,
// never to the HTTP request. Emit must finish before Run returns.
type Runner interface {
	Run(context.Context, string, func(string)) error
}

type operation struct {
	command Command
	state   string
}

// Reference is a single-session, in-memory transport used to test the contract.
// It deliberately does not implement ACP or authenticate network callers.
type Reference struct {
	ctx        context.Context
	cancel     context.CancelFunc
	runner     Runner
	mu         sync.Mutex
	operations map[string]operation
	events     []Event
	changed    chan struct{}
	workers    sync.WaitGroup
	closed     bool
}

func NewReference(runner Runner) *Reference {
	ctx, cancel := context.WithCancel(context.Background())
	return &Reference{ctx: ctx, cancel: cancel, runner: runner, operations: make(map[string]operation), changed: make(chan struct{})}
}

func (s *Reference) Close() {
	s.mu.Lock()
	s.closed = true
	s.cancel()
	s.mu.Unlock()
	s.workers.Wait()
}

func (s *Reference) appendLocked(id, kind, text string) {
	s.events = append(s.events, Event{len(s.events) + 1, id, kind, text})
	close(s.changed)
	s.changed = make(chan struct{})
}

func (s *Reference) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/commands":
		s.submit(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/events":
		s.stream(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Reference) submit(w http.ResponseWriter, r *http.Request) {
	var c Command
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	if err := decoder.Decode(&c); err != nil || c.ID == "" || c.Prompt == "" {
		http.Error(w, "invalid command", http.StatusBadRequest)
		return
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		http.Error(w, "expected one command", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		http.Error(w, "runtime closed", http.StatusServiceUnavailable)
		return
	}
	if existing, ok := s.operations[c.ID]; ok {
		s.mu.Unlock()
		if existing.command != c {
			http.Error(w, "operation ID reused with different payload", http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": c.ID, "state": existing.state})
		return
	}
	s.operations[c.ID] = operation{c, "running"}
	s.appendLocked(c.ID, "accepted", "")
	s.workers.Add(1)
	go func() {
		defer s.workers.Done()
		err := s.runner.Run(s.ctx, c.Prompt, func(text string) {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.appendLocked(c.ID, "output", text)
		})
		s.mu.Lock()
		defer s.mu.Unlock()
		state, text := "completed", ""
		if err != nil {
			state, text = "failed", err.Error()
		}
		s.operations[c.ID] = operation{c, state}
		s.appendLocked(c.ID, state, text)
	}()
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{"id": c.ID, "state": "running"})
}

func (s *Reference) stream(w http.ResponseWriter, r *http.Request) {
	cursor := 0
	if value := r.Header.Get("Last-Event-ID"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 {
			http.Error(w, "invalid cursor", http.StatusBadRequest)
			return
		}
		cursor = n
	}
	s.mu.Lock()
	future := cursor > len(s.events)
	s.mu.Unlock()
	if future {
		http.Error(w, "cursor ahead of session", http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	controller := http.NewResponseController(w)
	if err := controller.Flush(); err != nil {
		return
	}
	for {
		s.mu.Lock()
		batch := append([]Event(nil), s.events[cursor:]...)
		changed := s.changed
		s.mu.Unlock()
		for _, event := range batch {
			data, _ := json.Marshal(event)
			if _, err := fmt.Fprintf(w, "id: %d\ndata: %s\n\n", event.Sequence, data); err != nil {
				return
			}
			if err := controller.Flush(); err != nil {
				return
			}
			cursor = event.Sequence
		}
		select {
		case <-r.Context().Done():
			return
		case <-s.ctx.Done():
			return
		case <-changed:
		}
	}
}
