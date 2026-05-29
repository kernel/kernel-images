//go:build !linux

package sysmon

import (
	"errors"
	"log/slog"
)

// errKmsgUnsupported indicates that /dev/kmsg-based OOM telemetry is not
// available on this platform. The production target is Linux; non-Linux
// builds compile so developer machines can run unit tests, but the
// monitor itself is inert there.
var errKmsgUnsupported = errors.New("sysmon: /dev/kmsg OOM monitoring is only supported on Linux")

func openKmsgSource(_ *slog.Logger) (kmsgSource, error) {
	return nil, errKmsgUnsupported
}
