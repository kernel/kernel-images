//go:build linux

package api

import (
	"os/exec"
	"syscall"
)

var (
	termSignal = syscall.SIGTERM
	killSignal = syscall.SIGKILL
)

// configureBrowserReplCmd puts the REPL child in its own process group (so
// the whole group can be signaled on timeout/shutdown) and arranges for the
// kernel to kill the child if the API process dies unexpectedly.
func configureBrowserReplCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:   true,
		Pdeathsig: syscall.SIGKILL,
	}
}

// signalBrowserReplGroup signals every process in the child's process group.
func signalBrowserReplGroup(cmd *exec.Cmd, sig syscall.Signal) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, sig)
}
