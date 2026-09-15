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

// configureBrowserReplCmd puts the REPL child in its own process group so a
// reset or timeout also terminates subprocesses. Parent-death signaling stops
// the daemon if the API process exits unexpectedly.
func configureBrowserReplCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:   true,
		Pdeathsig: syscall.SIGKILL,
	}
}

func signalBrowserReplGroup(cmd *exec.Cmd, sig syscall.Signal) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, sig)
}
