//go:build darwin || linux

package app

import (
	"context"
	"os/exec"
	"syscall"
	"time"
)

func runCommandProcess(ctx context.Context, command *exec.Cmd, onStart ...func()) error {
	// Bound pipe cleanup even when a child detaches and retains output descriptors.
	prepareCommandProcess(command)
	if err := command.Start(); err != nil {
		return err
	}
	for _, started := range onStart {
		started()
	}
	if execution, ok := ctx.Value(executionContextKey{}).(*taskExecution); ok {
		if err := execution.event(ctx, "command.started", "running", map[string]any{"pid": command.Process.Pid, "process_group": command.Process.Pid, "started_at": time.Now().UTC()}); err != nil {
			_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
			_ = command.Wait()
			return err
		}
	}
	return waitCommandProcess(ctx, command)
}

func prepareCommandProcess(command *exec.Cmd) {
	command.WaitDelay = 2 * time.Second
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// CommandContext's default cancellation kills only the direct child. Keep
	// cancellation and process-group escalation in our bounded owner below.
	command.Cancel = nil
}

func waitCommandProcess(ctx context.Context, command *exec.Cmd) error {
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		return err
	case <-ctx.Done():
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGTERM)
		timer := time.NewTimer(500 * time.Millisecond)
		defer timer.Stop()
		select {
		case err := <-done:
			_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
			return err
		case <-timer.C:
			_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
			return <-done
		}
	}
}

func processSignal(command *exec.Cmd) string {
	if command.ProcessState == nil {
		return ""
	}
	status, ok := command.ProcessState.Sys().(syscall.WaitStatus)
	if ok && status.Signaled() {
		return status.Signal().String()
	}
	return ""
}

func prepareMCPProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func stopMCPProcess(command *exec.Cmd) {
	if command.Process != nil {
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
}
