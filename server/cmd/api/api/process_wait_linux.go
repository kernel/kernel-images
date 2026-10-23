//go:build linux

package api

import (
	"errors"
	"os/exec"

	"golang.org/x/sys/unix"
)

// Leave the exited leader waitable until capture ends. Its PID cannot be reused
// while we may still need to signal descendants in its process group.
func waitForProcessExit(cmd *exec.Cmd) error {
	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, cmd.Process.Pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}

func reapProcess(cmd *exec.Cmd) { _ = cmd.Wait() }
