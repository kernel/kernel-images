// Package agenttransport implements an experimental, single-session reconnect
// contract. It is not registered in the image server's production router.
package agenttransport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

type Command struct {
	ID     string `json:"id"`
	Prompt string `json:"prompt"`
}
type Control struct {
	ID          string `json:"id"`
	OperationID string `json:"operationId"`
	RequestID   string `json:"requestId,omitempty"`
	OptionID    string `json:"optionId,omitempty"`
}
type Event struct {
	Sequence    int             `json:"sequence"`
	OperationID string          `json:"operationId"`
	Kind        string          `json:"kind"`
	Text        string          `json:"text,omitempty"`
	Payload     json.RawMessage `json:"payload,omitempty"`
	Command     *Command        `json:"command,omitempty"`
	Control     *Control        `json:"control,omitempty"`
	RequestID   string          `json:"requestId,omitempty"`
}
type Runner interface {
	Run(context.Context, string, *Turn) error
}
type Operation struct {
	Command Command `json:"command"`
	State   string  `json:"state"`
}
type PendingPermission struct {
	OperationID string          `json:"operationId"`
	RequestID   string          `json:"requestId"`
	Payload     json.RawMessage `json:"payload"`
}
type Snapshot struct {
	Sequence    int                          `json:"sequence"`
	Operations  map[string]Operation         `json:"operations"`
	Permissions map[string]PendingPermission `json:"permissions"`
}

type Reference struct {
	runner      Runner
	store       Store
	mu          sync.Mutex
	operations  map[string]Operation
	controls    map[string]Control
	permissions map[string]PendingPermission
	decisions   map[string]string
	events      []Event
	changed     chan struct{}
	workers     sync.WaitGroup
	cancels     map[string]context.CancelFunc
	closed      bool
	fault       error
}

func NewReference(runner Runner) *Reference {
	runtime, err := NewRuntime(runner, &MemoryStore{})
	if err != nil {
		panic(err)
	}
	return runtime
}

// NewRuntime takes ownership of store on success. In-flight journal entries are
// uncertain after restart; they are never automatically sent to the agent again.
func NewRuntime(runner Runner, store Store) (*Reference, error) {
	s := &Reference{runner: runner, store: store, operations: make(map[string]Operation), controls: make(map[string]Control), permissions: make(map[string]PendingPermission), decisions: make(map[string]string), changed: make(chan struct{}), cancels: make(map[string]context.CancelFunc)}
	events, err := store.Load()
	if err != nil {
		return nil, err
	}
	for _, event := range events {
		if err := s.apply(event); err != nil {
			return nil, err
		}
		s.events = append(s.events, event)
	}
	for id, op := range s.operations {
		if op.State == "running" || op.State == "cancelling" {
			if err := s.record(Event{OperationID: id, Kind: "uncertain", Text: "runtime restarted before a terminal result was committed"}); err != nil {
				return nil, err
			}
		}
	}
	return s, nil
}

func (s *Reference) Close() {
	s.mu.Lock()
	s.closed = true
	for _, cancel := range s.cancels {
		cancel()
	}
	close(s.changed)
	s.changed = make(chan struct{})
	s.mu.Unlock()
	s.workers.Wait()
	_ = s.store.Close()
}

// record is called under mu (or during construction). No event becomes visible
// and no side effect is released until the journal accepts the record.
func (s *Reference) record(event Event) error {
	if s.fault != nil {
		return s.fault
	}
	event.Sequence = len(s.events) + 1
	// Copy payload ownership: an adapter can reuse its decode buffers.
	event.Payload = append(json.RawMessage(nil), event.Payload...)
	if err := s.store.Append(event); err != nil {
		s.fault = err
		for _, cancel := range s.cancels {
			cancel()
		}
		close(s.changed)
		s.changed = make(chan struct{})
		return err
	}
	if err := s.apply(event); err != nil {
		s.fault = err
		return err
	}
	s.events = append(s.events, event)
	close(s.changed)
	s.changed = make(chan struct{})
	return nil
}

func (s *Reference) apply(e Event) error {
	switch e.Kind {
	case "accepted":
		if e.Command == nil || e.Command.ID != e.OperationID {
			return errors.New("invalid accepted event")
		}
		s.operations[e.OperationID] = Operation{*e.Command, "running"}
	case "completed", "failed", "cancelled", "uncertain":
		op, ok := s.operations[e.OperationID]
		if !ok {
			return errors.New("terminal event without operation")
		}
		op.State = e.Kind
		s.operations[e.OperationID] = op
		for id, p := range s.permissions {
			if p.OperationID == e.OperationID {
				delete(s.permissions, id)
			}
		}
	case "permission_request":
		s.permissions[e.RequestID] = PendingPermission{e.OperationID, e.RequestID, e.Payload}
	case "permission_resolved":
		if e.Control == nil {
			return errors.New("permission decision without control")
		}
		s.controls[e.Control.ID] = *e.Control
		s.decisions[e.RequestID] = e.Control.OptionID
		delete(s.permissions, e.RequestID)
	case "cancel_requested":
		if e.Control == nil {
			return errors.New("cancellation without control")
		}
		s.controls[e.Control.ID] = *e.Control
		op := s.operations[e.OperationID]
		op.State = "cancelling"
		s.operations[e.OperationID] = op
	case "output", "acp":
	default:
		return fmt.Errorf("unknown journal event %q", e.Kind)
	}
	return nil
}

var ErrConflict = errors.New("operation conflict")
var ErrUnavailable = errors.New("runtime unavailable")
var ErrUncertain = errors.New("agent outcome uncertain")

func (s *Reference) Submit(c Command) (Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.fault != nil {
		return Operation{}, ErrUnavailable
	}
	if op, ok := s.operations[c.ID]; ok {
		if op.Command != c {
			return Operation{}, ErrConflict
		}
		return op, nil
	}
	for _, op := range s.operations {
		if op.State == "running" || op.State == "cancelling" || op.State == "uncertain" {
			return Operation{}, ErrConflict
		}
	}
	if err := s.record(Event{OperationID: c.ID, Kind: "accepted", Command: &c}); err != nil {
		return Operation{}, ErrUnavailable
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancels[c.ID] = cancel
	s.workers.Add(1)
	go func() {
		defer s.workers.Done()
		defer cancel()
		err := s.runner.Run(ctx, c.Prompt, &Turn{s: s, id: c.ID, ctx: ctx})
		s.mu.Lock()
		defer s.mu.Unlock()
		state, text := "completed", ""
		if ctx.Err() != nil {
			state = "cancelled"
		} else if errors.Is(err, ErrUncertain) {
			state, text = "uncertain", err.Error()
		} else if err != nil {
			state, text = "failed", err.Error()
		}
		_ = s.record(Event{OperationID: c.ID, Kind: state, Text: text})
		delete(s.cancels, c.ID)
	}()
	return s.operations[c.ID], nil
}

func (s *Reference) Control(c Control, permission bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.fault != nil {
		return ErrUnavailable
	}
	if prior, ok := s.controls[c.ID]; ok {
		if prior != c {
			return ErrConflict
		}
		return nil
	}
	op, ok := s.operations[c.OperationID]
	if !ok || op.State != "running" {
		return ErrConflict
	}
	kind := "cancel_requested"
	if permission {
		pending, ok := s.permissions[c.RequestID]
		if !ok || pending.OperationID != c.OperationID {
			return ErrConflict
		}
		// ACP options are opaque IDs. Only choices actually offered may be selected.
		var request struct {
			Options []struct {
				OptionID string `json:"optionId"`
			} `json:"options"`
		}
		if err := json.Unmarshal(pending.Payload, &request); err != nil {
			return ErrConflict
		}
		valid := false
		for _, option := range request.Options {
			if option.OptionID == c.OptionID {
				valid = true
			}
		}
		if !valid {
			return ErrConflict
		}
		kind = "permission_resolved"
	}
	if err := s.record(Event{OperationID: c.OperationID, Kind: kind, RequestID: c.RequestID, Control: &c}); err != nil {
		return ErrUnavailable
	}
	if !permission {
		s.cancels[c.OperationID]()
	}
	return nil
}

type Turn struct {
	s   *Reference
	id  string
	ctx context.Context
}

func (t *Turn) Emit(kind string, payload json.RawMessage) error {
	if kind != "acp" {
		return errors.New("only ACP payloads may be emitted")
	}
	t.s.mu.Lock()
	defer t.s.mu.Unlock()
	state := t.s.operations[t.id].State
	if state != "running" && state != "cancelling" {
		return ErrConflict
	}
	return t.s.record(Event{OperationID: t.id, Kind: kind, Payload: payload})
}
func (t *Turn) Output(text string) error {
	t.s.mu.Lock()
	defer t.s.mu.Unlock()
	if err := t.ctx.Err(); err != nil {
		return err
	}
	return t.s.record(Event{OperationID: t.id, Kind: "output", Text: text})
}
func (t *Turn) Permission(id string, payload json.RawMessage) (string, error) {
	encoded, _ := json.Marshal([]string{t.id, id})
	key := string(encoded)
	t.s.mu.Lock()
	if _, exists := t.s.permissions[key]; exists {
		t.s.mu.Unlock()
		return "", ErrConflict
	}
	if _, exists := t.s.decisions[key]; exists {
		t.s.mu.Unlock()
		return "", ErrConflict
	}
	err := t.ctx.Err()
	if err == nil {
		err = t.s.record(Event{OperationID: t.id, Kind: "permission_request", RequestID: key, Payload: payload})
	}
	t.s.mu.Unlock()
	if err != nil {
		return "", err
	}
	for {
		t.s.mu.Lock()
		decision, ok := t.s.decisions[key]
		changed := t.s.changed
		t.s.mu.Unlock()
		if ok {
			return decision, nil
		}
		select {
		case <-t.ctx.Done():
			return "", t.ctx.Err()
		case <-changed:
		}
	}
}
