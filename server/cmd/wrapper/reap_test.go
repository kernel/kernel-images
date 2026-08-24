package main

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

func waitForZombie(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if state, ppid, ok := procStat(pid); ok && state == 'Z' && ppid == os.Getpid() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("pid %d never became a zombie child", pid)
}

func isZombieChild(pid int) bool {
	state, ppid, ok := procStat(pid)
	return ok && state == 'Z' && ppid == os.Getpid()
}

func TestReapOrphansCollectsUnownedZombie(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid
	waitForZombie(t, pid)

	reapOrphans()

	if isZombieChild(pid) {
		t.Fatalf("pid %d still a zombie after reapOrphans", pid)
	}
}

func TestReapOrphansLeavesOwnedZombie(t *testing.T) {
	cmd := exec.Command("true")
	if err := startOwned(cmd); err != nil {
		t.Fatalf("startOwned: %v", err)
	}
	pid := cmd.Process.Pid
	waitForZombie(t, pid)

	reapOrphans()

	if !isZombieChild(pid) {
		t.Fatalf("reapOrphans collected owned pid %d", pid)
	}
	if err := waitOwned(cmd); err != nil {
		t.Fatalf("waitOwned after reapOrphans: %v", err)
	}
	if owned.cmds[pid] != nil {
		t.Fatalf("pid %d still tracked after waitOwned", pid)
	}
}

func TestReleaseOwnedKeepsLaterHolderOfSamePid(t *testing.T) {
	first := exec.Command("true")
	if err := startOwned(first); err != nil {
		t.Fatalf("startOwned: %v", err)
	}
	pid := first.Process.Pid
	if err := waitOwned(first); err != nil {
		t.Fatalf("waitOwned: %v", err)
	}

	// Stand in for the kernel handing this pid to a later command.
	second := exec.Command("true")
	owned.Lock()
	owned.cmds[pid] = second
	owned.Unlock()

	releaseOwned(first)

	owned.Lock()
	defer owned.Unlock()
	if owned.cmds[pid] != second {
		t.Fatal("a stale release evicted the current holder of the pid")
	}
	delete(owned.cmds, pid)
}

func TestProcStatReadsSelf(t *testing.T) {
	state, ppid, ok := procStat(os.Getpid())
	if !ok {
		t.Fatal("procStat failed on self")
	}
	if state == 'Z' {
		t.Fatalf("self reported as zombie")
	}
	if ppid != os.Getppid() {
		t.Fatalf("ppid = %d, want %d", ppid, os.Getppid())
	}
}
