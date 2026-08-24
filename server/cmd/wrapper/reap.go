package main

import (
	"bytes"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The wrapper is pid 1 in both the container (ENTRYPOINT) and the unikernel
// (Kraftfile cmd), so any process whose parent dies first is reparented onto
// it — chromium's crashpad handlers are the common case, one per relaunch.
// Nothing waits on those, so each stays a zombie holding a pid slot until the
// instance dies, and every /proc walk on the host slows down with them.
//
// Reaping can't just be wait4(-1) on SIGCHLD: os/exec waits on a specific pid,
// and if the reaper collects one of our children first that Cmd.Wait fails
// with ECHILD and loses its exit status. So we track the commands we start and
// only collect pids we don't own. Ownership is keyed by command identity
// because Cmd.Wait frees the pid before releaseOwned runs, and a late release
// must not evict whatever took that pid next.
//
// Only commands whose exit status we act on need this — supervisord, whose
// exit ends the instance, and runStream, which runStreamFatal turns into a
// boot failure. The rest already throw their status away with `_ =`, so
// there's nothing for the reaper to take from them.
var owned = struct {
	sync.Mutex
	cmds map[int]*exec.Cmd
}{cmds: map[int]*exec.Cmd{}}

// startOwned starts cmd and records it as ours to wait on. The lock is held
// across Start so a concurrent reap can't observe the child in the window
// between fork and registration.
func startOwned(cmd *exec.Cmd) error {
	owned.Lock()
	defer owned.Unlock()
	if err := cmd.Start(); err != nil {
		return err
	}
	owned.cmds[cmd.Process.Pid] = cmd
	return nil
}

// waitOwned waits on a command started by startOwned and releases its pid.
func waitOwned(cmd *exec.Cmd) error {
	err := cmd.Wait()
	releaseOwned(cmd)
	return err
}

// releaseOwned drops cmd's pid, but only while that pid still belongs to cmd.
func releaseOwned(cmd *exec.Cmd) {
	pid := cmd.Process.Pid
	owned.Lock()
	if owned.cmds[pid] == cmd {
		delete(owned.cmds, pid)
	}
	owned.Unlock()
}

// runOwned is startOwned followed by waitOwned: the replacement for Cmd.Run.
func runOwned(cmd *exec.Cmd) error {
	if err := startOwned(cmd); err != nil {
		return err
	}
	return waitOwned(cmd)
}

// startReaper drains adopted zombies for the life of the process. It does
// nothing when we aren't pid 1, since then orphans reparent elsewhere.
func startReaper() {
	if os.Getpid() != 1 {
		return
	}
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGCHLD)
	go func() {
		// SIGCHLD coalesces, so a burst arriving mid-drain can leave
		// stragglers. The ticker bounds how long one sits around.
		tick := time.NewTicker(30 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ch:
			case <-tick.C:
			}
			reapOrphans()
		}
	}()
}

func reapOrphans() {
	for _, pid := range zombieChildren() {
		owned.Lock()
		if owned.cmds[pid] == nil {
			var ws syscall.WaitStatus
			// A pid that slipped away between the scan and here just comes
			// back around on the next wakeup.
			_, _ = syscall.Wait4(pid, &ws, syscall.WNOHANG, nil)
		}
		owned.Unlock()
	}
}

// zombieChildren returns the pids of our children currently in Z state.
func zombieChildren() []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	self := os.Getpid()
	var zombies []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if state, ppid, ok := procStat(pid); ok && state == 'Z' && ppid == self {
			zombies = append(zombies, pid)
		}
	}
	return zombies
}

// procStat reads a process's state and parent pid from /proc/<pid>/stat.
// comm is the second field, parenthesized, and may itself contain spaces and
// parens, so the fixed-width fields are read from after its closing paren.
func procStat(pid int) (state byte, ppid int, ok bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, 0, false
	}
	commEnd := bytes.LastIndexByte(b, ')')
	if commEnd < 0 || commEnd+2 >= len(b) {
		return 0, 0, false
	}
	fields := strings.Fields(string(b[commEnd+2:]))
	if len(fields) < 2 {
		return 0, 0, false
	}
	parent, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, false
	}
	return fields[0][0], parent, true
}
