//go:build linux

package api

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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

// browserReplSocketOwnerPids returns the pids of processes holding an open
// fd for the unix socket bound at socketPath, discovered via /proc. Used to
// kill orphaned REPL daemons (started outside this API process) before
// removing their socket file. Only processes whose cmdline references the
// browser REPL bundle are returned, so an unrelated process squatting on
// the path is left alone.
func browserReplSocketOwnerPids(socketPath string) ([]int, error) {
	data, err := os.ReadFile("/proc/net/unix")
	if err != nil {
		return nil, err
	}
	inodes := map[string]struct{}{}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		// Num RefCount Protocol Flags Type St Inode Path
		if len(fields) >= 8 && fields[len(fields)-1] == socketPath {
			inodes[fields[6]] = struct{}{}
		}
	}
	if len(inodes) == 0 {
		return nil, nil
	}

	var pids []int
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		fds, err := os.ReadDir(filepath.Join("/proc", entry.Name(), "fd"))
		if err != nil {
			continue
		}
		for _, fd := range fds {
			link, err := os.Readlink(filepath.Join("/proc", entry.Name(), "fd", fd.Name()))
			if err != nil || !strings.HasPrefix(link, "socket:[") {
				continue
			}
			inode := strings.TrimSuffix(strings.TrimPrefix(link, "socket:["), "]")
			if _, ok := inodes[inode]; !ok {
				continue
			}
			cmdline, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
			if err == nil && strings.Contains(string(cmdline), "browser-repl") {
				pids = append(pids, pid)
			}
			break
		}
	}
	return pids, nil
}

// killOrphanedBrowserRepl SIGKILLs any orphaned REPL daemon still listening
// on socketPath (a daemon not spawned by this API process — pdeathsig
// covers children the API itself spawned). The socket file is removed by
// the caller afterwards.
func killOrphanedBrowserRepl(socketPath string) []int {
	pids, err := browserReplSocketOwnerPids(socketPath)
	if err != nil {
		return nil
	}
	killed := make([]int, 0, len(pids))
	for _, pid := range pids {
		if err := syscall.Kill(pid, syscall.SIGKILL); err == nil {
			killed = append(killed, pid)
		}
	}
	return killed
}
