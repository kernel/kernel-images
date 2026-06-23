//go:build linux

package sysmon

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/euank/go-kmsg-parser/v2/kmsgparser"
)

// openKmsgSource opens /dev/kmsg and seeks past the existing ring buffer
// so the scanner only sees events that occur after this call. Without the
// seek, each process restart would replay the entire historical buffer
// and emit stale events with current timestamps.
func openKmsgSource(logger *slog.Logger) (kmsgSource, error) {
	p, err := kmsgparser.NewParser()
	if err != nil {
		return nil, fmt.Errorf("open /dev/kmsg: %w", err)
	}
	p.SetLogger(kmsgLogger{logger: logger})
	if err := p.SeekEnd(); err != nil {
		p.Close()
		return nil, fmt.Errorf("seek to end of /dev/kmsg: %w", err)
	}
	return &kmsgparserSource{p: p}, nil
}

// kmsgparserSource adapts the third-party kmsgparser.Parser to the
// internal kmsgSource interface so the rest of sysmon stays decoupled
// from the library type.
type kmsgparserSource struct {
	p kmsgparser.Parser
}

func (s *kmsgparserSource) Messages() <-chan KmsgMessage {
	in := s.p.Parse()
	out := make(chan KmsgMessage)
	go func() {
		defer close(out)
		for m := range in {
			// Stamp wall-clock read time, not m.Timestamp: the kmsg
			// envelope timestamp is CLOCK_MONOTONIC-derived, which freezes
			// while the VM is suspended (scale-to-zero) and so skews
			// backward by the suspended duration. We only read live records
			// (openKmsgSource seeks to end), so read time is accurate.
			out <- KmsgMessage{Timestamp: time.Now(), Body: m.Message}
		}
	}()
	return out
}

func (s *kmsgparserSource) Close() error { return s.p.Close() }

// kmsgLogger routes the kmsgparser library's diagnostic output through our
// structured logger.
type kmsgLogger struct {
	logger *slog.Logger
}

func (l kmsgLogger) Infof(format string, args ...any) {
	// The library only logs Info on graceful shutdown ("kmsg reader
	// closed, shutting down"). Treat it like sysmon.go's own
	// start/stop signals: useful for debugging but not Info-worthy.
	l.logger.Debug(fmt.Sprintf("sysmon/kmsg: "+format, args...))
}

func (l kmsgLogger) Warningf(format string, args ...any) {
	l.logger.Warn(fmt.Sprintf("sysmon/kmsg: "+format, args...))
}

func (l kmsgLogger) Errorf(format string, args ...any) {
	l.logger.Error(fmt.Sprintf("sysmon/kmsg: "+format, args...))
}
