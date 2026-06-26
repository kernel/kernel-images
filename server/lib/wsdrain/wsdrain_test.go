package wsdrain

import (
	"testing"

	"github.com/coder/websocket"
)

type fakeConn struct {
	closes []websocket.StatusCode
}

func (c *fakeConn) Close(code websocket.StatusCode, _ string) error {
	c.closes = append(c.closes, code)
	return nil
}

func TestCloseAllClosesTrackedConns(t *testing.T) {
	r := New()
	a, b := &fakeConn{}, &fakeConn{}
	r.Track(a)
	r.Track(b)

	if n := r.CloseAll(websocket.StatusGoingAway, "bye"); n != 2 {
		t.Fatalf("CloseAll returned %d, want 2", n)
	}
	for _, c := range []*fakeConn{a, b} {
		if len(c.closes) != 1 || c.closes[0] != websocket.StatusGoingAway {
			t.Fatalf("conn closed with %v, want one StatusGoingAway", c.closes)
		}
	}
}

func TestUntrackRemovesConn(t *testing.T) {
	r := New()
	c := &fakeConn{}
	untrack := r.Track(c)
	untrack()
	untrack() // idempotent

	if n := r.CloseAll(websocket.StatusGoingAway, "bye"); n != 0 {
		t.Fatalf("CloseAll returned %d after untrack, want 0", n)
	}
	if len(c.closes) != 0 {
		t.Fatalf("untracked conn was closed: %v", c.closes)
	}
}

func TestNilRegistryIsNoop(t *testing.T) {
	var r *Registry
	untrack := r.Track(&fakeConn{})
	untrack()
	if n := r.CloseAll(websocket.StatusGoingAway, "bye"); n != 0 {
		t.Fatalf("nil registry CloseAll returned %d, want 0", n)
	}
}
