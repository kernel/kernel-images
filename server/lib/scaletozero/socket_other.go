//go:build !linux

package scaletozero

import (
	"errors"
	"time"
)

func setTCPUserTimeout(int, time.Duration) error {
	return errors.New("TCP_USER_TIMEOUT is unavailable")
}

func outboundQueueFD(int) (int, error) {
	return 0, errors.New("TCP outbound queue inspection is unavailable")
}

func abortSocket(int) error {
	return errors.New("abortive socket close is unavailable")
}

func closeSocket(int) error {
	return errors.New("socket close is unavailable")
}

func terminateGuest() {
	panic("guest termination is unavailable")
}

func inspectResponseClose(int) (responseCloseState, error) {
	return responseClosePending, errors.New("TCP close acknowledgement inspection is unavailable")
}
