package main

import (
	"bufio"
	"reflect"
	"strings"
	"testing"
)

func TestParseFields(t *testing.T) {
	in := "processname:mutter groupname:mutter from_state:RUNNING expected:0 pid:1234"
	got := parseFields(in)
	want := map[string]string{
		"processname": "mutter",
		"groupname":   "mutter",
		"from_state":  "RUNNING",
		"expected":    "0",
		"pid":         "1234",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseFields = %v, want %v", got, want)
	}
}

func TestReadEvent(t *testing.T) {
	payload := "processname:cat groupname:cat from_state:RUNNING expected:0 pid:2766"
	header := "ver:3.0 server:supervisor serial:21 pool:listener poolserial:10 eventname:PROCESS_STATE_EXITED len:" +
		itoa(len(payload)) + "\n"
	in := bufio.NewReader(strings.NewReader(header + payload))

	hdr, pl, err := readEvent(in)
	if err != nil {
		t.Fatalf("readEvent: %v", err)
	}
	if hdr["eventname"] != "PROCESS_STATE_EXITED" {
		t.Errorf("eventname = %q", hdr["eventname"])
	}
	if pl["pid"] != "2766" || pl["processname"] != "cat" || pl["expected"] != "0" {
		t.Errorf("payload = %v", pl)
	}
}

func TestMapEventExitedUnexpected(t *testing.T) {
	hdr := map[string]string{"eventname": "PROCESS_STATE_EXITED"}
	pl := map[string]string{
		"processname": "mutter",
		"from_state":  "RUNNING",
		"expected":    "0",
		"pid":         "1234",
	}
	body, ok := mapEvent(hdr, pl)
	if !ok {
		t.Fatal("expected publish")
	}
	if body.Type != "service_crashed" {
		t.Errorf("Type = %q", body.Type)
	}
	if body.Category != "system" {
		t.Errorf("Category = %q", body.Category)
	}
	if body.Source.Kind != "local_process" {
		t.Errorf("Source.Kind = %q", body.Source.Kind)
	}
	if body.Source.Event != "supervisord.process_exited" {
		t.Errorf("Source.Event = %q", body.Source.Event)
	}
	if body.Data.ServiceName != "mutter" || body.Data.FromState != "RUNNING" {
		t.Errorf("Data = %+v", body.Data)
	}
	if body.Data.Pid == nil || *body.Data.Pid != 1234 {
		t.Errorf("Pid = %v", body.Data.Pid)
	}
}

func TestMapEventExitedExpectedSkipped(t *testing.T) {
	hdr := map[string]string{"eventname": "PROCESS_STATE_EXITED"}
	pl := map[string]string{
		"processname": "mutter",
		"from_state":  "RUNNING",
		"expected":    "1",
		"pid":         "1234",
	}
	if _, ok := mapEvent(hdr, pl); ok {
		t.Fatal("expected skip for expected=1")
	}
}

func TestMapEventFatal(t *testing.T) {
	hdr := map[string]string{"eventname": "PROCESS_STATE_FATAL"}
	pl := map[string]string{
		"processname": "chromium",
		"from_state":  "BACKOFF",
	}
	body, ok := mapEvent(hdr, pl)
	if !ok {
		t.Fatal("expected publish")
	}
	if body.Source.Event != "supervisord.process_fatal" {
		t.Errorf("Source.Event = %q", body.Source.Event)
	}
	if body.Data.ServiceName != "chromium" || body.Data.FromState != "BACKOFF" {
		t.Errorf("Data = %+v", body.Data)
	}
	if body.Data.Pid != nil {
		t.Errorf("Pid should be nil for FATAL, got %v", *body.Data.Pid)
	}
}

func TestMapEventUnrelatedSkipped(t *testing.T) {
	hdr := map[string]string{"eventname": "PROCESS_STATE_STARTING"}
	if _, ok := mapEvent(hdr, map[string]string{"processname": "x", "from_state": "STOPPED"}); ok {
		t.Fatal("expected skip for non-crash event")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
