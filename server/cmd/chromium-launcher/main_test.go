package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExecLookPath(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "mybin")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write bin: %v", err)
	}
	oldPath := os.Getenv("PATH")
	defer func() { _ = os.Setenv("PATH", oldPath) }()
	if err := os.Setenv("PATH", dir); err != nil {
		t.Fatalf("set PATH: %v", err)
	}

	// lookPath should find by PATH
	if p, err := exec.LookPath("mybin"); err != nil || p != bin {
		t.Fatalf("lookPath failed: p=%q err=%v", p, err)
	}

	// execLookPath should return input when absolute
	if p, err := execLookPath(bin); err != nil || p != bin {
		t.Fatalf("execLookPath absolute failed: p=%q err=%v", p, err)
	}

	// execLookPath should resolve by PATH for bare names
	if p, err := execLookPath("mybin"); err != nil || p != bin {
		t.Fatalf("execLookPath PATH search failed: p=%q err=%v", p, err)
	}
}

func writeFixture(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "fixture")
	require.NoError(t, os.WriteFile(p, []byte(contents), 0o644))
	return p
}

func TestParseSocketStates(t *testing.T) {
	// Real /proc/net/tcp format. Port 9223 = 0x2407, port 8888 = 0x22B8.
	// state column (4th): 01=ESTABLISHED, 06=TIME_WAIT, 0A=LISTEN.
	fixture := "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n" +
		"   0: 0100007F:2407 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1 0 0\n" +
		"   1: 0100007F:2407 0100007F:E1A4 01 00000000:00000000 00:00000000 00000000     0        0 2 0 0\n" +
		"   2: 0100007F:2407 0100007F:E1A5 01 00000000:00000000 00:00000000 00000000     0        0 3 0 0\n" +
		"   3: 0100007F:2407 0100007F:E1A6 06 00000000:00000000 00:00000000 00000000     0        0 4 0 0\n" +
		"   4: 0100007F:22B8 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 5 0 0\n"
	path := writeFixture(t, fixture)

	estab, timewait, listen := parseSocketStates(path, 9223)
	assert.Equal(t, 2, estab, "ESTABLISHED count for port 9223")
	assert.Equal(t, 1, timewait, "TIME_WAIT count for port 9223")
	assert.Equal(t, 1, listen, "LISTEN count for port 9223")

	// Port 8888 only has one LISTEN socket.
	estab, timewait, listen = parseSocketStates(path, 8888)
	assert.Equal(t, 0, estab)
	assert.Equal(t, 0, timewait)
	assert.Equal(t, 1, listen)

	// Unknown port returns zeros.
	estab, timewait, listen = parseSocketStates(path, 1234)
	assert.Equal(t, 0, estab)
	assert.Equal(t, 0, timewait)
	assert.Equal(t, 0, listen)
}

func TestParseSocketStatesMissingFile(t *testing.T) {
	estab, timewait, listen := parseSocketStates("/does/not/exist", 9223)
	assert.Equal(t, 0, estab)
	assert.Equal(t, 0, timewait)
	assert.Equal(t, 0, listen)
}

func TestParseMemInfo(t *testing.T) {
	fixture := "MemTotal:        2048000 kB\n" +
		"MemFree:          512000 kB\n" +
		"MemAvailable:    1024000 kB\n" +
		"Buffers:           20000 kB\n" +
		"Cached:           400000 kB\n" +
		"Dirty:              5000 kB\n" +
		"Writeback:             0 kB\n"
	path := writeFixture(t, fixture)

	avail, cached, dirty := parseMemInfo(path)
	assert.Equal(t, int64(1024000), avail)
	assert.Equal(t, int64(400000), cached)
	assert.Equal(t, int64(5000), dirty)
}

func TestParseMemInfoMissingFile(t *testing.T) {
	avail, cached, dirty := parseMemInfo("/does/not/exist")
	assert.Equal(t, int64(0), avail)
	assert.Equal(t, int64(0), cached)
	assert.Equal(t, int64(0), dirty)
}

func TestParseMemInfoMissingFields(t *testing.T) {
	fixture := "MemTotal:        2048000 kB\n" +
		"SomeOtherField:    12345 kB\n"
	path := writeFixture(t, fixture)

	avail, cached, dirty := parseMemInfo(path)
	assert.Equal(t, int64(0), avail)
	assert.Equal(t, int64(0), cached)
	assert.Equal(t, int64(0), dirty)
}

func TestReadVMUptimeMsAndLoadAvg(t *testing.T) {
	// These read real /proc on Linux test runners. On non-Linux they return 0.
	// We just exercise the code path; values may be 0 in CI containers without /proc.
	_ = readVMUptimeMs()
	_ = readLoadAvg1m()
}
