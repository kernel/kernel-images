//go:build !linux

package api

import (
	"os/exec"
	"syscall"
)

// Fallback process management for non-Linux builds (development only; the
// production images run Linux). There is no parent-death signaling here, and
// "group" signaling degrades to signaling the child process itself.
var (
	termSignal = syscall.SIGTERM
	killSignal = syscall.SIGKILL
)

func configureBrowserReplCmd(cmd *exec.Cmd) {}

func signalBrowserReplGroup(cmd *exec.Cmd, sig syscall.Signal) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if sig == termSignal {
		// Best effort graceful stop; escalated by the caller if it fails.
		_ = cmd.Process.Signal(sig)
		return nil
	}
	return cmd.Process.Kill()
}
