package sysmon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kernel/kernel-images/server/lib/events"
	oapi "github.com/kernel/kernel-images/server/lib/oapi"
)

// oomKillRe matches the canonical kernel OOM-killer line. Example:
//
//	Out of memory: Killed process 1234 (chromium) total-vm:5234572kB, anon-rss:4823900kB, file-rss:0kB, shmem-rss:0kB, UID:0 pgtables:8000kB oom_score_adj:0
//
// The comm is bounded to 15 chars by the kernel (TASK_COMM_LEN-1) but may
// contain parens internally for a few cases (e.g. `(sd-pam)`); we deliberately
// match a lazy non-paren-aware group since the more-permissive form has bitten
// other parsers — a comm with `)` would be exceptional and the line still
// parses for everything except the comm field.
var oomKillRe = regexp.MustCompile(
	`Out of memory: Killed process (\d+) \(([^)]+)\) ` +
		`total-vm:(\d+)kB, anon-rss:(\d+)kB, file-rss:(\d+)kB, shmem-rss:(\d+)kB` +
		`.*?oom_score_adj:(-?\d+)`,
)

// parseKmsgLine strips the kmsg envelope `<priority,seq,timestamp,flags>;`
// and returns the message portion. Returns the original line if no envelope
// is found (which can happen on truncated reads, though that's rare).
func parseKmsgLine(line string) string {
	if i := strings.IndexByte(line, ';'); i >= 0 {
		return line[i+1:]
	}
	return line
}

// parseOomKill extracts OOM-kill data from a kmsg message body. Returns nil if
// the line is not an OOM-kill record.
func parseOomKill(msg string) *oapi.BrowserSystemOomKillEventData {
	m := oomKillRe.FindStringSubmatch(msg)
	if m == nil {
		return nil
	}
	pid, _ := strconv.Atoi(m[1])
	totalVM, _ := strconv.Atoi(m[3])
	anonRSS, _ := strconv.Atoi(m[4])
	fileRSS, _ := strconv.Atoi(m[5])
	shmemRSS, _ := strconv.Atoi(m[6])
	scoreAdj, _ := strconv.Atoi(m[7])
	rss := anonRSS + fileRSS + shmemRSS
	return &oapi.BrowserSystemOomKillEventData{
		ProcessName:  m[2],
		Pid:          pid,
		TotalVmKb:    &totalVM,
		RssKb:        rss,
		OomScoreAdj:  &scoreAdj,
	}
}

func (m *Monitor) runKmsg(ctx context.Context) {
	f, err := os.OpenFile(m.kmsgPath, os.O_RDONLY, 0)
	if err != nil {
		m.logger.Warn("sysmon: failed to open kmsg, OOM events disabled", "err", err, "path", m.kmsgPath)
		return
	}

	// Closing f unblocks the read loop on shutdown.
	go func() {
		<-ctx.Done()
		f.Close()
	}()

	m.logger.Info("sysmon: kmsg reader started", "path", m.kmsgPath)

	// Each /dev/kmsg read() returns at most one record. The kernel guarantees
	// EINVAL if the buffer is too small, so use a generous fixed buffer.
	buf := make([]byte, 8192)
	for {
		n, err := f.Read(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) {
				return
			}
			// EPIPE can occur if a process between us and the ring buffer
			// dies; we keep going.
			m.logger.Warn("sysmon: kmsg read error", "err", err)
			continue
		}
		line := string(buf[:n])
		msg := parseKmsgLine(line)
		data := parseOomKill(msg)
		if data == nil {
			continue
		}
		m.publishOomKill(*data)
	}
}

func (m *Monitor) publishOomKill(data oapi.BrowserSystemOomKillEventData) {
	payload, err := json.Marshal(data)
	if err != nil {
		m.logger.Warn("sysmon: marshal oom kill payload", "err", err)
		return
	}
	ev := events.Event{
		Ts:       time.Now().UnixMicro(),
		Type:     string(oapi.SystemOomKill),
		Category: events.System,
		Source: oapi.BrowserEventSource{
			Kind:  oapi.LocalProcess,
			Event: stringPtr("linux.oom_kill"),
		},
		Data: json.RawMessage(payload),
	}
	m.es.Publish(events.Envelope{Event: ev})
	m.logger.Info("sysmon: oom kill",
		"process", data.ProcessName,
		"pid", data.Pid,
		"rss_kb", data.RssKb,
	)
}

func stringPtr(s string) *string { return &s }
