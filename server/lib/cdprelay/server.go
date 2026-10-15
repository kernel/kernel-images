package cdprelay

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Prefix is the versioned, private relay API. It must not be exposed without its per-instance token.
const Prefix = "/internal/cdp-relay/v1/"

// Options bounds resources and selects the guest-local CDP proxy.
type Options struct {
	Token          string
	UpstreamURL    string
	MaxSessions    int
	MaxBufferBytes int
	LeaseTTL       time.Duration
	PollWait       time.Duration
}

// Server owns Chrome connections independently of incoming HTTP request lifetimes.
type Server struct {
	createMu sync.Mutex
	mu       sync.Mutex
	ctx      context.Context
	opts     Options
	sessions map[string]*session
}

// New creates a relay that closes all retained sessions when ctx is canceled.
func New(ctx context.Context, opts Options) (*Server, error) {
	if len(opts.Token) < 32 {
		return nil, fmt.Errorf("CDP relay token must contain at least 32 bytes")
	}
	if opts.MaxSessions <= 0 {
		opts.MaxSessions = 8
	}
	if opts.MaxBufferBytes <= 0 {
		opts.MaxBufferBytes = 8 << 20
	}
	if opts.LeaseTTL <= 0 {
		opts.LeaseTTL = 2 * time.Minute
	}
	if opts.PollWait <= 0 {
		opts.PollWait = 15 * time.Second
	}
	s := &Server{ctx: ctx, opts: opts, sessions: make(map[string]*session)}
	go s.reap()
	return s, nil
}

func (s *Server) reap() {
	interval := min(5*time.Second, s.opts.LeaseTTL/2)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
		case <-ticker.C:
		}
		s.mu.Lock()
		for id, sess := range s.sessions {
			sess.mu.Lock()
			expired := (!sess.parked || sess.closed) && time.Since(sess.lastSeen) >= s.opts.LeaseTTL
			sess.mu.Unlock()
			if expired || s.ctx.Err() != nil {
				delete(s.sessions, id)
				sess.close()
			}
		}
		s.mu.Unlock()
		if s.ctx.Err() != nil {
			return
		}
	}
}

func (s *Server) create(id string) (*session, error) {
	s.createMu.Lock()
	defer s.createMu.Unlock()
	s.mu.Lock()
	existing := s.sessions[id]
	full := len(s.sessions) >= s.opts.MaxSessions
	s.mu.Unlock()
	if existing != nil {
		return existing, nil
	}
	if full {
		return nil, errBusy
	}
	ctx, cancel := context.WithCancel(s.ctx)
	dialCtx, dialCancel := context.WithTimeout(ctx, 10*time.Second)
	defer dialCancel()
	conn, _, err := websocket.Dial(dialCtx, s.opts.UpstreamURL, &websocket.DialOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		cancel()
		return nil, errUnavailable
	}
	conn.SetReadLimit(int64(s.opts.MaxBufferBytes))
	sess := &session{conn: conn, ctx: ctx, cancel: cancel, pending: make(map[string]struct{}), limit: s.opts.MaxBufferBytes, lastSeen: time.Now(), notify: make(chan struct{}, 1)}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil {
		sess.close()
		return nil, errGone
	}
	s.sessions[id] = sess
	go sess.read()
	return sess, nil
}

func respond(w http.ResponseWriter, value any, err error) {
	if err != nil {
		status := http.StatusBadRequest
		switch {
		case errors.Is(err, errGone):
			status = http.StatusGone
		case errors.Is(err, errConflict):
			status = http.StatusConflict
		case errors.Is(err, errBusy):
			status = http.StatusLocked
		case errors.Is(err, errUnavailable):
			status = http.StatusServiceUnavailable
		}
		http.Error(w, err.Error(), status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(value)
}

// ServeHTTP implements idempotent creation, ordered commands, event acknowledgments, and fenced park/resume.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+s.opts.Token)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if !strings.HasPrefix(r.URL.Path, Prefix) {
		http.NotFound(w, r)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, Prefix), "/")
	id := parts[0]
	if len(id) != 64 {
		http.NotFound(w, r)
		return
	}
	if _, err := hex.DecodeString(id); err != nil {
		http.NotFound(w, r)
		return
	}
	if len(parts) == 1 && r.Method == http.MethodPut {
		sess, err := s.create(id)
		if err != nil {
			respond(w, nil, err)
			return
		}
		value, err := sess.snapshot()
		respond(w, value, err)
		return
	}
	s.mu.Lock()
	sess := s.sessions[id]
	if len(parts) == 1 && r.Method == http.MethodDelete {
		delete(s.sessions, id)
		s.mu.Unlock()
		if sess != nil {
			sess.close()
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.mu.Unlock()
	if sess == nil {
		http.NotFound(w, r)
		return
	}
	if !sess.touch() {
		respond(w, nil, errGone)
		return
	}
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	if parts[1] == "events" && r.Method == http.MethodGet {
		epoch, err := parseUint(r.URL.Query().Get("epoch"))
		if err != nil {
			respond(w, nil, err)
			return
		}
		ack, err := parseUint(r.URL.Query().Get("ack"))
		if err != nil {
			respond(w, nil, err)
			return
		}
		value, err := sess.events(r.Context(), epoch, ack, s.opts.PollWait)
		respond(w, value, err)
		return
	}
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	var req request
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, int64(s.opts.MaxBufferBytes+1024)))
	if err := decoder.Decode(&req); err != nil {
		http.Error(w, "invalid relay request", http.StatusBadRequest)
		return
	}
	switch parts[1] {
	case "send":
		respond(w, true, sess.send(req))
	case "park", "resume":
		value, err := sess.transition(req, parts[1] == "park")
		respond(w, value, err)
	default:
		http.NotFound(w, r)
	}
}
