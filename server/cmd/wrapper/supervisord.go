package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	supervisorConf  = "/etc/supervisor/supervisord.conf"
	supervisorConfD = "/etc/supervisor/conf.d/services"
	supervisorSock  = "/var/run/supervisor.sock"
	supervisordLogD = "/var/log/supervisord"
)

// startAll asks supervisord to start the given programs. We invoke
// supervisorctl once (it accepts multiple args) so we don't pay python
// cold-start costs per service.
func startAll(progs ...string) {
	supervisorctl("start", progs...)
}

// restartAll is the start-or-stop+start variant. It's used for services
// that may already be running from a snapshot restore (post-fork, see the
// FORK HOOK in main) so they pick up refreshed envs cleanly. supervisorctl
// `restart` is a no-op stop on cold programs followed by a normal start.
func restartAll(progs ...string) {
	supervisorctl("restart", progs...)
}

func supervisorctl(verb string, progs ...string) {
	if len(progs) == 0 {
		return
	}
	args := append([]string{"-c", supervisorConf, verb}, progs...)
	cmd := exec.Command("supervisorctl", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	_ = cmd.Run() // a service that fails to come up will surface via readiness checks
}

func waitForSocket(path string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fi, err := os.Stat(path); err == nil && fi.Mode()&os.ModeSocket != 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	logf("WARNING: socket %s not ready after %s", path, timeout)
}

// startLogAggregator tails any file under /var/log/supervisord, prefixing
// each line with the relative path so the container log stream remains
// readable.
func startLogAggregator() {
	_ = os.MkdirAll(supervisordLogD, 0o755)
	go func() {
		seen := map[string]bool{}
		for {
			entries, _ := os.ReadDir(supervisordLogD)
			for _, e := range entries {
				path := filepath.Join(supervisordLogD, e.Name())
				if seen[path] {
					continue
				}
				if fi, err := os.Stat(path); err == nil && fi.Mode().IsRegular() {
					seen[path] = true
					go tailFile(path)
				}
			}
			time.Sleep(500 * time.Millisecond)
		}
	}()
}

func tailFile(path string) {
	cmd := exec.Command("tail", "-n", "+1", "-F", path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return
	}
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return
	}
	label := filepath.Base(path)
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		fmt.Printf("[%s] %s\n", label, scanner.Text())
	}
}

func runStream(label, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = prefixWriter{label: label, w: os.Stdout}
	cmd.Stderr = prefixWriter{label: label, w: os.Stderr}
	return cmd.Run()
}

// runStreamFatal is runStream + fatalf on non-zero exit. Use for scripts the
// boot path cannot proceed without (init-envoy). The old wrapper.sh ran under
// `set -o errexit`, so these were already fatal there.
func runStreamFatal(label, name string, args ...string) {
	if err := runStream(label, name, args...); err != nil {
		fatalf("%s failed: %v", label, err)
	}
}

type prefixWriter struct {
	label string
	w     *os.File
}

func (p prefixWriter) Write(b []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if line == "" {
			continue
		}
		fmt.Fprintf(p.w, "[%s] %s\n", p.label, line)
	}
	return len(b), nil
}

func isExecutable(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0
}
