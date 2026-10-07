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

import "sync/atomic"

// State is the session's egress policy as the control plane last reported it.
// The zero value is an unfiltered session, which is what a session is until
// the control plane says otherwise, so a VM that is never told anything
// behaves exactly as it did before.
type State struct {
	filtered atomic.Bool
}

func New() *State {
	return &State{}
}

// Filtered reports whether the session's egress is restricted to an allowlist.
// It is read on the CDP proxy's forwarding path, ahead of the message it is
// deciding about, so it is a lock-free load.
func (s *State) Filtered() bool {
	return s != nil && s.filtered.Load()
}

// SetFiltered records the policy the control plane applied. It is called when
// the session is set up and again whenever an allowlist is added to or removed
// from a running session, so it must be safe to call repeatedly with the same
// value.
func (s *State) SetFiltered(filtered bool) {
	s.filtered.Store(filtered)
}
