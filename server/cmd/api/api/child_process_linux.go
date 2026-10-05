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

// configureChildProcessCmd puts an API-owned child (Browser REPL or Playwright
// daemon) in its own process group so a reset or timeout also terminates its
// subprocesses. Parent-death signaling stops the child if the API process
// exits unexpectedly.
func configureChildProcessCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:   true,
		Pdeathsig: syscall.SIGKILL,
	}
}

func signalChildProcessGroup(cmd *exec.Cmd, sig syscall.Signal) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, sig)
}
