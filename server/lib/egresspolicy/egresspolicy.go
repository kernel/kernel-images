// Package egresspolicy holds what the VM has been told about the egress
// policy its session runs under.
//
// The policy itself lives outside the VM: a session's allowlist is enforced by
// Kernel's egress proxy, which is the only component that knows which
// destinations are allowed. The VM is told one thing — that an allowlist
// exists — because some of what it can be asked to do would route traffic
// around that proxy, and it cannot refuse those requests without knowing the
// session is filtered at all.
package egresspolicy

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"

	"github.com/kernel/kernel-images/server/lib/statefile"
)

// DefaultStatePath is where the policy is kept so it survives a restart of the
// API process, which supervisord does automatically. Alongside the fork
// identity, since both are per-boot state the control plane hands the VM.
const DefaultStatePath = "/run/kernel/egress-policy.json"

type persisted struct {
	Filtered bool `json:"filtered"`
}

// State is the session's egress policy as the control plane last reported it.
//
// It is held on disk as well as in memory. The API process is restarted
// automatically if it dies, and a restart that came back unfiltered would drop
// the refusal silently while the session's allowlist was still being enforced
// at the proxy — the gap would be invisible from outside the VM.
type State struct {
	path string
	// mu serializes a write with the store that follows it, so the file and
	// the memory copy cannot disagree about which update came last.
	mu       sync.Mutex
	filtered atomic.Bool
}

// Load reads the policy left by a previous run of the process. A session that
// has never been told anything is unfiltered, which is how every session
// behaved before the policy existed.
//
// A file that exists but cannot be read is treated as filtered. It should not
// happen — the file is written atomically — but the alternative is to resolve
// an unreadable security control in the direction that switches it off.
func Load(path string, logger *slog.Logger) *State {
	s := &State{path: path}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s
	}
	if err != nil {
		logger.Error("could not read the egress policy; treating the session as filtered", "path", path, "err", err)
		s.filtered.Store(true)
		return s
	}
	var p persisted
	if err := json.Unmarshal(data, &p); err != nil {
		logger.Error("could not decode the egress policy; treating the session as filtered", "path", path, "err", err)
		s.filtered.Store(true)
		return s
	}
	s.filtered.Store(p.Filtered)
	return s
}

// Filtered reports whether the session's egress is restricted to an allowlist.
// It is read on the CDP proxy's forwarding path, ahead of the message it is
// deciding about, so it is a lock-free load.
func (s *State) Filtered() bool {
	return s != nil && s.filtered.Load()
}

// SetFiltered records the policy the control plane applied, and does not take
// effect unless it was persisted: a caller that is told the policy is applied
// must be able to rely on it surviving a restart.
//
// It is called when the session is set up and again whenever an allowlist is
// added to or removed from a running session, so it is safe to call repeatedly
// with the same value.
func (s *State) SetFiltered(filtered bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.persist(filtered); err != nil {
		return err
	}
	s.filtered.Store(filtered)
	return nil
}

func (s *State) persist(filtered bool) error {
	if err := statefile.WriteJSON(s.path, persisted{Filtered: filtered}); err != nil {
		return fmt.Errorf("persist egress policy: %w", err)
	}
	return nil
}
