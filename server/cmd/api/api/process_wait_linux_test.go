//go:build linux

package api

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestProcessWaitPreservesExitedLeader(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 37")
	require.NoError(t, cmd.Start())
	defer cmd.Wait()
	require.Eventually(t, func() bool {
		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", cmd.Process.Pid))
		return err == nil && strings.Contains(string(stat), ") Z ")
	}, time.Second, time.Millisecond)
	require.NoError(t, waitForProcessExit(cmd))
	require.NoError(t, waitForProcessExit(cmd), "observing exit must not reap the leader")
	reapProcess(cmd)
	require.NotNil(t, cmd.ProcessState)
	require.Equal(t, 37, cmd.ProcessState.ExitCode())
}
