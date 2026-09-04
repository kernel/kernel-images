//go:build !linux

package scaletozero

import (
	"errors"
	"net"
	"time"
)

func setTCPUserTimeout(*net.TCPConn, time.Duration) error {
	return errors.New("TCP_USER_TIMEOUT is unavailable")
}

func closeAcknowledged(int) (bool, error) {
	return false, errors.New("TCP close acknowledgement inspection is unavailable")
}
