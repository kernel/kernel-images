//go:build linux

package sysmon

import (
	"testing"
	"time"

	"github.com/euank/go-kmsg-parser/v2/kmsgparser"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeKmsgParser is a kmsgparser.Parser that replays a fixed set of
// messages. It lets us assert how kmsgparserSource maps the library's
// records onto KmsgMessage without touching /dev/kmsg.
type fakeKmsgParser struct{ msgs []kmsgparser.Message }

func (f *fakeKmsgParser) SeekEnd() error              { return nil }
func (f *fakeKmsgParser) SetLogger(kmsgparser.Logger) {}
func (f *fakeKmsgParser) Close() error                { return nil }
func (f *fakeKmsgParser) Parse() <-chan kmsgparser.Message {
	ch := make(chan kmsgparser.Message, len(f.msgs))
	for _, m := range f.msgs {
		ch <- m
	}
	close(ch)
	return ch
}

// TestKmsgparserSourceStampsObservationTime verifies the production
// source ignores the kmsg envelope's (monotonic-derived) timestamp and
// stamps the wall-clock observation time instead. This is the fix for
// OOM events landing minutes in the past on scale-to-zero VMs, where
// CLOCK_MONOTONIC freezes during suspend.
func TestKmsgparserSourceStampsObservationTime(t *testing.T) {
	// A timestamp the kmsg envelope might carry on a long-suspended VM:
	// far in the past relative to real wall-clock.
	envelopeTime := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	src := &kmsgparserSource{p: &fakeKmsgParser{msgs: []kmsgparser.Message{
		{Timestamp: envelopeTime, Message: "chromium invoked oom-killer: order=0"},
	}}}

	before := time.Now()
	msg, ok := <-src.Messages()
	after := time.Now()

	require.True(t, ok, "expected one forwarded message")
	assert.Equal(t, "chromium invoked oom-killer: order=0", msg.Body)
	assert.False(t, msg.Timestamp.Before(before), "stamp must be >= observation start, got %s", msg.Timestamp)
	assert.False(t, msg.Timestamp.After(after), "stamp must be <= observation end, got %s", msg.Timestamp)
	assert.False(t, msg.Timestamp.Equal(envelopeTime), "stamp must not be the kmsg envelope (monotonic) time")
}
