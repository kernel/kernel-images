//go:build linux

package scaletozero

import (
	"net"
	"time"

	"golang.org/x/sys/unix"
)

func setTCPUserTimeout(conn *net.TCPConn, timeout time.Duration) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var setErr error
	if err := raw.Control(func(fd uintptr) {
		setErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_USER_TIMEOUT, int(timeout.Milliseconds()))
	}); err != nil {
		return err
	}
	return setErr
}

func closeAcknowledged(fd int) (bool, error) {
	info, err := unix.GetsockoptTCPInfo(fd, unix.IPPROTO_TCP, unix.TCP_INFO)
	if err != nil {
		return false, err
	}
	const (
		tcpFinWait2 = 5
		tcpTimeWait = 6
		tcpClose    = 7
	)
	switch info.State {
	case tcpFinWait2, tcpTimeWait, tcpClose:
		return true, nil
	default:
		return false, nil
	}
}
