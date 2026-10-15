//go:build !linux

package api

import "os/exec"

func waitForProcessExit(cmd *exec.Cmd) error {
	_ = cmd.Wait()
	return nil
}

func reapProcess(cmd *exec.Cmd) {}
