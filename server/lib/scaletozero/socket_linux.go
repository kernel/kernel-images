//go:build linux

package scaletozero

import (
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

func setTCPUserTimeout(fd int, timeout time.Duration) error {
	return unix.SetsockoptInt(fd, unix.IPPROTO_TCP, unix.TCP_USER_TIMEOUT, int(timeout.Milliseconds()))
}

func outboundQueueFD(fd int) (int, error) {
	return unix.IoctlGetInt(fd, unix.TIOCOUTQ)
}

func abortSocket(fd int) error {
	return abortSocketWith(
		func() error {
			return unix.SetsockoptLinger(fd, unix.SOL_SOCKET, unix.SO_LINGER, &unix.Linger{Onoff: 1})
		},
		func() error { return unix.Close(fd) },
	)
}

func abortSocketWith(setLinger, closeFD func() error) error {
	if err := setLinger(); err != nil {
		return err
	}
	// Linux releases the descriptor even when close returns an error such as
	// EINTR. Never let recovery operate on a descriptor number that may be reused.
	_ = closeFD()
	return nil
}

func closeSocket(fd int) error {
	return unix.Close(fd)
}

// PID 1 is the image wrapper; it exits the guest after stopping supervisord.
func terminateGuest() {
	if err := unix.Kill(1, unix.SIGTERM); err != nil {
		panic(fmt.Sprintf("failed to terminate guest: %v", err))
	}
	time.Sleep(30 * time.Second)
	if err := unix.Kill(1, unix.SIGKILL); err != nil {
		panic(fmt.Sprintf("failed to force guest termination: %v", err))
	}
	select {}
}

func inspectResponseClose(fd int) (responseCloseState, error) {
	info, err := unix.GetsockoptTCPInfo(fd, unix.IPPROTO_TCP, unix.TCP_INFO)
	if err != nil {
		return responseClosePending, err
	}
	const (
		tcpFinWait2 = 5
		tcpTimeWait = 6
		tcpClose    = 7
	)
	switch info.State {
	case tcpFinWait2, tcpTimeWait:
		return responseCloseAcknowledged, nil
	case tcpClose:
		return responseCloseTerminated, nil
	default:
		return responseClosePending, nil
	}
}
