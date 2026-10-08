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
	command.WaitDelay = 2 * time.Second
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
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
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		return err
	case <-ctx.Done():
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		return <-done
	}
}
