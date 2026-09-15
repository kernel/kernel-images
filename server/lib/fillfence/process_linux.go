package fillfence

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// StopPrevious runs only inside the single-browser image's process namespace,
// before a new browser starts. Both browser and renderer/zygote executables are
// covered, including orphans left when the launcher was killed. A disconnected
// DevTools socket or a successful supervisorctl response is not proof of exit.
// Permission errors, unsupported pidfds and unkillable processes fail closed.
func StopPrevious(ctx context.Context, executable string) error {
	self, err := os.Readlink("/proc/self")
	if err != nil || self != strconv.Itoa(os.Getpid()) {
		return errors.New("proc process namespace mismatch")
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return err
	}
	return stopProcesses(ctx, executable, unix.PidfdSendSignal)
}

func stopProcesses(ctx context.Context, executable string, signal func(int, unix.Signal, *unix.Siginfo, int) error) error {
	for {
		entries, err := os.ReadDir("/proc")
		if err != nil {
			return err
		}
		found := false
		for _, entry := range entries {
			pid, err := strconv.Atoi(entry.Name())
			if err != nil {
				continue
			}
			path, err := os.Readlink(filepath.Join("/proc", entry.Name(), "exe"))
			if errors.Is(err, os.ErrNotExist) {
				// /proc/<pid>/exe can also disappear when only the main thread exits.
				// A readable pidfd, unlike a zombie-looking leader, proves group exit.
				exited, checkErr := processExited(pid)
				if checkErr != nil {
					return checkErr
				}
				if !exited {
					kernel, err := kernelThread(pid)
					if err != nil || !kernel {
						return errors.New("cannot inspect live process executable")
					}
				}
				continue
			}
			if err != nil {
				return fmt.Errorf("inspect process: %w", err)
			}
			path = strings.TrimSuffix(path, " (deleted)")
			if path != executable && path != filepath.Join(filepath.Dir(executable), "chrome_crashpad_handler") && path != filepath.Join(filepath.Dir(executable), "chrome_sandbox") {
				continue
			}
			found = true
			fd, err := unix.PidfdOpen(pid, 0)
			if errors.Is(err, unix.ESRCH) {
				continue
			}
			if err != nil {
				return fmt.Errorf("open process handle: %w", err)
			}
			// Check the executable again after acquiring a stable process handle. A PID
			// reused for an unrelated program must not be signalled.
			current, readErr := os.Readlink(filepath.Join("/proc", entry.Name(), "exe"))
			if readErr == nil && strings.TrimSuffix(current, " (deleted)") == path {
				err = signal(fd, unix.SIGKILL, nil, 0)
			}
			unix.Close(fd)
			if err != nil && !errors.Is(err, unix.ESRCH) {
				return fmt.Errorf("kill previous browser: %w", err)
			}
			if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
				return readErr
			}
		}
		if !found {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("previous browser exit not confirmed: %w", ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func kernelThread(pid int) (bool, error) {
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return false, err
	}
	fields := strings.Fields(string(stat)[strings.LastIndexByte(string(stat), ')')+1:])
	if len(fields) < 7 {
		return false, errors.New("invalid process stat")
	}
	flags, err := strconv.ParseUint(fields[6], 10, 64)
	return flags&0x00200000 != 0, err // Linux PF_KTHREAD; cannot execute userspace.
}

func processExited(pid int) (bool, error) {
	fd, err := unix.PidfdOpen(pid, 0)
	if errors.Is(err, unix.ESRCH) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	defer unix.Close(fd)
	events := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	_, err = unix.Poll(events, 0)
	return events[0].Revents&unix.POLLIN != 0, err
}
