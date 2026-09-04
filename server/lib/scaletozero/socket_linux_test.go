//go:build linux

package scaletozero

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestAbortSocketDoesNotReuseDescriptorAfterCloseError(t *testing.T) {
	var lingerCalls atomic.Int32
	var closeCalls atomic.Int32
	var inspectCalls atomic.Int32
	var abortCalls atomic.Int32
	var releaseCalls atomic.Int32
	config := testDrainConfig(0, nil)
	config.setUserTimeout = func(int, time.Duration) error { return nil }
	config.outboundFD = func(int) (int, error) {
		inspectCalls.Add(1)
		return 1, nil
	}
	config.closeState = func(int) (responseCloseState, error) {
		inspectCalls.Add(1)
		return responseClosePending, nil
	}
	config.abortFD = func(int) error {
		abortCalls.Add(1)
		return abortSocketWith(
			func() error {
				lingerCalls.Add(1)
				return nil
			},
			func() error {
				closeCalls.Add(1)
				return unix.EINTR
			},
		)
	}
	config.releaseMonitor = func() { releaseCalls.Add(1) }
	config.terminateGuest = func() { t.Fatal("close EINTR triggered guest termination") }

	monitorClosedResponse(-1, nil, config, false, nil, "")

	assert.Equal(t, int32(1), abortCalls.Load())
	assert.Equal(t, int32(1), lingerCalls.Load())
	assert.Equal(t, int32(1), closeCalls.Load())
	assert.Equal(t, int32(2), inspectCalls.Load())
	require.Equal(t, int32(1), releaseCalls.Load())
}
