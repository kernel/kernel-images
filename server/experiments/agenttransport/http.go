package agenttransport

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

func decode(w http.ResponseWriter, r *http.Request, value any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		http.Error(w, "invalid request", 400)
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		http.Error(w, "expected one request", 400)
		return false
	}
	return true
}
func sendError(w http.ResponseWriter, err error) {
	status := http.StatusServiceUnavailable
	if errors.Is(err, ErrConflict) {
		status = http.StatusConflict
	}
	http.Error(w, http.StatusText(status), status)
}
func (s *Reference) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/commands":
		var c Command
		if !decode(w, r, &c) {
			return
		}
		if c.ID == "" || c.Prompt == "" {
			http.Error(w, "id and prompt required", 400)
			return
		}
		op, err := s.Submit(c)
		if err != nil {
			sendError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(202)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": op.Command.ID, "state": op.State})
	case r.Method == http.MethodPost && (r.URL.Path == "/permissions" || r.URL.Path == "/cancel"):
		var c Control
		if !decode(w, r, &c) {
			return
		}
		permission := r.URL.Path == "/permissions"
		if c.ID == "" || c.OperationID == "" || (permission && (c.RequestID == "" || c.OptionID == "")) || (!permission && (c.RequestID != "" || c.OptionID != "")) {
			http.Error(w, "invalid control", 400)
			return
		}
		if err := s.Control(c, permission); err != nil {
			sendError(w, err)
			return
		}
		w.WriteHeader(202)
	case r.Method == http.MethodGet && r.URL.Path == "/snapshot":
		s.mu.Lock()
		if s.fault != nil || s.closed {
			s.mu.Unlock()
			sendError(w, ErrUnavailable)
			return
		}
		data, err := json.Marshal(Snapshot{len(s.events), s.operations, s.permissions})
		s.mu.Unlock()
		if err != nil {
			sendError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	case r.Method == http.MethodGet && r.URL.Path == "/events":
		s.stream(w, r)
	default:
		http.NotFound(w, r)
	}
}
func (s *Reference) stream(w http.ResponseWriter, r *http.Request) {
	cursor := 0
	if value := r.Header.Get("Last-Event-ID"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 {
			http.Error(w, "invalid cursor", 400)
			return
		}
		cursor = n
	}
	s.mu.Lock()
	future := cursor > len(s.events)
	unavailable := s.closed || s.fault != nil
	s.mu.Unlock()
	if unavailable {
		sendError(w, ErrUnavailable)
		return
	}
	if future {
		http.Error(w, "cursor ahead of session", 409)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	controller := http.NewResponseController(w)
	flush := func() error { return controller.Flush() }
	if err := controller.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return
	}
	if err := flush(); err != nil {
		return
	}
	heartbeat := time.NewTicker(time.Second)
	defer heartbeat.Stop()
	for {
		s.mu.Lock()
		batch := append([]Event(nil), s.events[cursor:]...)
		changed := s.changed
		stop := s.closed || s.fault != nil
		s.mu.Unlock()
		if stop {
			return
		}
		for _, event := range batch {
			data, _ := json.Marshal(event)
			if err := controller.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
				return
			}
			if _, err := fmt.Fprintf(w, "id: %d\ndata: %s\n\n", event.Sequence, data); err != nil {
				return
			}
			if err := flush(); err != nil {
				return
			}
			cursor = event.Sequence
		}
		select {
		case <-r.Context().Done():
			return
		case <-changed:
		case <-heartbeat.C:
			if err := controller.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
				return
			}
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			if err := flush(); err != nil {
				return
			}
		}
	}
}
