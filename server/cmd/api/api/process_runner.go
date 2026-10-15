package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/google/uuid"
	"github.com/kernel/kernel-images/server/lib/logger"
)

const processOutputDrainTimeout = time.Second
const processRetention = 10 * time.Second

func (s *ApiService) startProcess(ctx context.Context, cmd *exec.Cmd, tty, stdin bool, timeoutSec *int) (*processHandle, error) {
	s.procMu.Lock()
	if s.processOutput == nil {
		s.processOutput, _ = newProcessOutputBudget(ProcessOutputConfig{})
	}
	budget := s.processOutput
	s.procMu.Unlock()
	output := &processOutput{changed: make(chan struct{}), closed: make(chan struct{})}
	var err error
	if !tty {
		output, err = budget.open()
		if err != nil {
			return nil, fmt.Errorf("create process output: %v", err)
		}
	}
	h := &processHandle{id: uuid.New(), cmd: cmd, started: time.Now(), isTTY: tty, output: output, doneCh: make(chan struct{})}
	if tty {
		h.ptyFile, err = pty.Start(cmd)
		h.stdin = h.ptyFile
	} else {
		if cmd.SysProcAttr == nil {
			cmd.SysProcAttr = &syscall.SysProcAttr{}
		}
		cmd.SysProcAttr.Setpgid = true
		if stdin {
			h.stdin, err = cmd.StdinPipe()
		}
		if err == nil {
			h.stdout, h.stderr, err = startWithOutputPipes(cmd)
		}
	}
	if err != nil {
		if h.stdin != nil {
			h.stdin.Close()
		}
		output.close()
		return nil, err
	}
	h.pid = cmd.Process.Pid
	s.procMu.Lock()
	if s.procs == nil {
		s.procs = make(map[string]*processHandle)
	}
	s.procs[h.id.String()] = h
	s.procMu.Unlock()
	stzDisabled := false
	if s.stz != nil {
		if err := s.stz.Disable(ctx); err != nil {
			logger.FromContext(ctx).Error("failed to disable scale-to-zero", "err", err)
		} else {
			stzDisabled = true
		}
	}
	lifecycle := s.lifecycleCtx
	if lifecycle == nil {
		lifecycle = context.Background()
	}
	go h.run(ctx, lifecycle, timeoutSec, func() {
		if stzDisabled {
			releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if err := s.stz.Enable(releaseCtx); err != nil {
				logger.FromContext(ctx).Error("failed to enable scale-to-zero", "err", err)
			}
		}
	})
	return h, nil
}

func startWithOutputPipes(cmd *exec.Cmd) (io.ReadCloser, io.ReadCloser, error) {
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		stdoutR.Close()
		stdoutW.Close()
		return nil, nil, err
	}
	cmd.Stdout, cmd.Stderr = stdoutW, stderrW
	err = cmd.Start()
	stdoutW.Close()
	stderrW.Close()
	if err != nil {
		stdoutR.Close()
		stderrR.Close()
		return nil, nil, err
	}
	return stdoutR, stderrR, nil
}

func (h *processHandle) terminate(reason string, truncated bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.reason == "" {
		h.reason = reason
	}
	h.truncated = h.truncated || truncated
	if h.exitCode == nil {
		_ = signalChildProcessGroup(h.cmd, killSignal)
	}
}

func (h *processHandle) outputFailed(err error) {
	reason := "output_error"
	if errors.Is(err, errProcessOutputLimit) {
		reason = "output_limit"
	}
	h.terminate(reason, true)
}

func (h *processHandle) run(ctx, lifecycle context.Context, timeoutSec *int, release func()) {
	defer release()
	failures := make(chan error, 2)
	var readers sync.WaitGroup
	if !h.isTTY {
		for stream, pipe := range []io.ReadCloser{h.stdout, h.stderr} {
			readers.Add(1)
			go func(stream byte, pipe io.Reader) {
				defer readers.Done()
				if err := h.captureOutput(stream, pipe); err != nil {
					failures <- err
				}
			}(byte(stream), pipe)
		}
	}
	readersDone := make(chan struct{})
	go func() { readers.Wait(); close(readersDone) }()
	waited := make(chan error, 1)
	go func() { waited <- waitForProcessExit(h.cmd) }()
	var deadline, drain <-chan time.Time
	var deadlineAt time.Time
	if timeoutSec != nil && *timeoutSec > 0 {
		deadlineAt = h.started.Add(time.Duration(*timeoutSec) * time.Second)
		timer := time.NewTimer(time.Until(deadlineAt))
		defer timer.Stop()
		deadline = timer.C
	}
	canceled, shutdown := ctx.Done(), lifecycle.Done()
	leaderExited, outputComplete := false, false
	for !leaderExited || !outputComplete {
		select {
		case err := <-waited:
			if err != nil {
				h.terminate("process_canceled", false)
				logger.FromContext(ctx).Error("failed to wait for process exit", "err", err)
			}
			leaderExited = true
			waited = nil
			timer := time.NewTimer(processOutputDrainTimeout)
			defer timer.Stop()
			drain = timer.C
		case <-readersDone:
			outputComplete = true
			readersDone = nil
		case <-deadline:
			h.terminate("process_timeout", false)
			deadline = nil
		case <-canceled:
			h.terminate("process_canceled", false)
			canceled = nil
		case <-shutdown:
			h.terminate("process_canceled", false)
			shutdown = nil
		case err := <-failures:
			h.outputFailed(err)
		case <-drain:
			if !deadlineAt.IsZero() && !time.Now().Before(deadlineAt) {
				h.terminate("process_timeout", false)
			}
			h.mu.Lock()
			h.truncated = true
			_ = signalChildProcessGroup(h.cmd, killSignal)
			h.mu.Unlock()
			h.closeOutputPipes()
			drain = nil
		}
	}
	close(failures)
	for err := range failures {
		h.outputFailed(err)
	}
	h.closeOutputPipes()
	if h.stdin != nil {
		h.stdin.Close()
	}
	// Reap while holding the same lock used by signal requests, so no signal can
	// target a reused PID between Wait and publishing the final status.
	h.mu.Lock()
	reapProcess(h.cmd)
	code := h.cmd.ProcessState.ExitCode()
	h.durationMs = int(time.Since(h.started) / time.Millisecond)
	h.exitCode = &code
	h.mu.Unlock()
	h.output.finish()
	close(h.doneCh)
}

func (h *processHandle) captureOutput(stream byte, pipe io.Reader) error {
	buf := make([]byte, processOutputChunkSize)
	for {
		n, err := pipe.Read(buf)
		if n > 0 {
			if writeErr := h.output.append(stream, buf[:n]); writeErr != nil {
				return writeErr
			}
		}
		if errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (h *processHandle) closeOutputPipes() {
	if h.isTTY {
		h.ptyFile.Close()
	} else {
		h.stdout.Close()
		h.stderr.Close()
	}
}

func (s *ApiService) releaseProcess(h *processHandle) {
	s.procMu.Lock()
	delete(s.procs, h.id.String())
	s.procMu.Unlock()
	h.output.close()
}

func (s *ApiService) retainProcess(h *processHandle) {
	<-h.doneCh
	timer := time.NewTimer(processRetention)
	defer timer.Stop()
	var shutdown <-chan struct{}
	if s.lifecycleCtx != nil {
		shutdown = s.lifecycleCtx.Done()
	}
	select {
	case <-timer.C:
	case <-shutdown:
	}
	s.releaseProcess(h)
}
