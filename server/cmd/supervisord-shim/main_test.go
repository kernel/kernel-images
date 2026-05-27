package main

import (
	"bufio"
	"bytes"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	oapi "github.com/kernel/kernel-images/server/lib/oapi"
)

func TestWriteResultOKHasNoTrailingNewline(t *testing.T) {
	// Regression: supervisord's eventlistener protocol reads exactly the
	// declared byte count after the header newline. A trailing newline
	// here misaligns the following READY frame and deadlocks the
	// listener after the first event.
	var buf bytes.Buffer
	bw := bufio.NewWriter(&buf)
	require.NoError(t, writeResultOK(bw))
	assert.Equal(t, "RESULT 2\nOK", buf.String())
}

func TestParseFields(t *testing.T) {
	got := parseFields("processname:mutter groupname:mutter from_state:RUNNING expected:0 pid:1234")
	assert.Equal(t, map[string]string{
		"processname": "mutter",
		"groupname":   "mutter",
		"from_state":  "RUNNING",
		"expected":    "0",
		"pid":         "1234",
	}, got)
}

func TestReadEvent(t *testing.T) {
	payload := "processname:cat groupname:cat from_state:RUNNING expected:0 pid:2766"
	header := "ver:3.0 server:supervisor serial:21 pool:listener poolserial:10 eventname:PROCESS_STATE_EXITED len:" +
		strconv.Itoa(len(payload)) + "\n"
	in := bufio.NewReader(strings.NewReader(header + payload))

	hdr, pl, err := readEvent(in)
	require.NoError(t, err)
	assert.Equal(t, "PROCESS_STATE_EXITED", hdr["eventname"])
	assert.Equal(t, "2766", pl["pid"])
	assert.Equal(t, "cat", pl["processname"])
	assert.Equal(t, "0", pl["expected"])
}

// crashedData unwraps the BrowserServiceCrashedEventData payload from a
// mapped PublishEventRequest. The shim builds the request with Data set
// to a concrete struct (not interface{}); this helper keeps the test
// assertions short.
func crashedData(t *testing.T, body oapi.PublishEventRequest) oapi.BrowserServiceCrashedEventData {
	t.Helper()
	data, ok := body.Data.(oapi.BrowserServiceCrashedEventData)
	require.True(t, ok, "Data is %T, want BrowserServiceCrashedEventData", body.Data)
	return data
}

func TestMapEventExitedUnexpectedFromRunning(t *testing.T) {
	body, ok := mapEvent(
		map[string]string{"eventname": "PROCESS_STATE_EXITED"},
		map[string]string{
			"processname": "mutter",
			"from_state":  "RUNNING",
			"expected":    "0",
			"pid":         "1234",
		},
	)
	require.True(t, ok)
	assert.Equal(t, string(oapi.ServiceCrashed), body.Type)
	require.NotNil(t, body.Category)
	assert.Equal(t, oapi.PublishEventRequestCategory("system"), *body.Category)
	require.NotNil(t, body.Source)
	assert.Equal(t, oapi.LocalProcess, body.Source.Kind)
	require.NotNil(t, body.Source.Event)
	assert.Equal(t, "service.crashed", *body.Source.Event)

	data := crashedData(t, body)
	assert.Equal(t, "mutter", data.ServiceName)
	assert.Equal(t, oapi.BrowserServiceCrashedEventDataPhaseRunning, data.Phase)
	require.NotNil(t, data.Pid)
	assert.Equal(t, 1234, *data.Pid)
}

func TestMapEventExitedUnexpectedFromStarting(t *testing.T) {
	// A crash during startup must surface as the "startup" phase, not
	// "running" — operators triage these differently (config bug vs
	// runtime bug).
	body, ok := mapEvent(
		map[string]string{"eventname": "PROCESS_STATE_EXITED"},
		map[string]string{
			"processname": "envoy",
			"from_state":  "STARTING",
			"expected":    "0",
			"pid":         "55",
		},
	)
	require.True(t, ok)
	assert.Equal(t, oapi.BrowserServiceCrashedEventDataPhaseStartup, crashedData(t, body).Phase)
}

func TestMapEventExitedExpectedSkipped(t *testing.T) {
	_, ok := mapEvent(
		map[string]string{"eventname": "PROCESS_STATE_EXITED"},
		map[string]string{
			"processname": "mutter",
			"from_state":  "RUNNING",
			"expected":    "1",
			"pid":         "1234",
		},
	)
	assert.False(t, ok, "expected=1 (clean exit) must not produce an event")
}

func TestMapEventExitedFromBackoffSkipped(t *testing.T) {
	// supervisord's state machine does not normally produce EXITED out
	// of BACKOFF — the BACKOFF→FATAL edge fires instead once
	// startretries is exhausted. If a future supervisord version routes
	// it differently, we must not silently invent a phase: skip and let
	// the caller log.
	_, ok := mapEvent(
		map[string]string{"eventname": "PROCESS_STATE_EXITED"},
		map[string]string{
			"processname": "chromium",
			"from_state":  "BACKOFF",
			"expected":    "0",
		},
	)
	assert.False(t, ok)
}

func TestMapEventFatalFromBackoff(t *testing.T) {
	body, ok := mapEvent(
		map[string]string{"eventname": "PROCESS_STATE_FATAL"},
		map[string]string{
			"processname": "chromium",
			"from_state":  "BACKOFF",
		},
	)
	require.True(t, ok)
	data := crashedData(t, body)
	assert.Equal(t, oapi.BrowserServiceCrashedEventDataPhaseGaveUp, data.Phase)
	assert.Nil(t, data.Pid, "FATAL transitions do not carry a live PID")
}

func TestMapEventFatalIgnoresFromState(t *testing.T) {
	// FATAL is reached exclusively via the BACKOFF→FATAL edge per
	// supervisord docs, so the from_state lookup is intentionally not
	// consulted for FATAL events. This test pins that behavior so a
	// future refactor doesn't reintroduce a silent drop if supervisord
	// ever omits from_state.
	body, ok := mapEvent(
		map[string]string{"eventname": "PROCESS_STATE_FATAL"},
		map[string]string{"processname": "chromium"},
	)
	require.True(t, ok)
	assert.Equal(t, oapi.BrowserServiceCrashedEventDataPhaseGaveUp, crashedData(t, body).Phase)
}

func TestMapEventUnrelatedSkipped(t *testing.T) {
	_, ok := mapEvent(
		map[string]string{"eventname": "PROCESS_STATE_STARTING"},
		map[string]string{"processname": "x", "from_state": "STOPPED"},
	)
	assert.False(t, ok)
}

func TestIsCrashEvent(t *testing.T) {
	assert.True(t, isCrashEvent("PROCESS_STATE_EXITED"))
	assert.True(t, isCrashEvent("PROCESS_STATE_FATAL"))
	assert.False(t, isCrashEvent("PROCESS_STATE_STARTING"))
	assert.False(t, isCrashEvent("PROCESS_STATE_RUNNING"))
	assert.False(t, isCrashEvent(""))
}

func TestMapEventUnknownFromStateSkipped(t *testing.T) {
	// If supervisord emits a crash transition out of a state we have no
	// public mapping for (e.g. STOPPED, which shouldn't happen with the
	// events we subscribe to), drop the event rather than invent a phase.
	_, ok := mapEvent(
		map[string]string{"eventname": "PROCESS_STATE_EXITED"},
		map[string]string{
			"processname": "x",
			"from_state":  "STOPPED",
			"expected":    "0",
		},
	)
	assert.False(t, ok)
}
