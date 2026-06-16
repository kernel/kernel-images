package events

import (
	"testing"

	"github.com/s2-streamstore/s2-sdk-go/s2"
	"github.com/stretchr/testify/assert"
)

// TestNextCursor covers the pagination-termination decision (the subtlest bit of
// the S2 read path) without needing a live S2 stream.
func TestNextCursor(t *testing.T) {
	t.Parallel()
	pos := func(seq uint64) *s2.StreamPosition { return &s2.StreamPosition{SeqNum: seq} }

	cases := []struct {
		name     string
		next     *s2.StreamPosition
		tail     *s2.StreamPosition
		wantSeq  uint64
		wantMore bool
	}{
		{"more records remain", pos(5), pos(10), 5, true},
		{"caught up to tail terminates", pos(10), pos(10), 0, false},
		{"past tail terminates", pos(11), pos(10), 0, false},
		{"nil next (empty/exhausted read) terminates", nil, pos(10), 0, false},
		{"nil tail terminates", pos(5), nil, 0, false},
		{"both nil terminates", nil, nil, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seq, more := nextCursor(tc.next, tc.tail)
			assert.Equal(t, tc.wantMore, more)
			assert.Equal(t, tc.wantSeq, seq)
		})
	}
}
