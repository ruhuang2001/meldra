//go:build darwin || linux

package app

import (
	"context"
	"os/exec"
	"syscall"
	"time"
)

func runCommandProcess(ctx context.Context, command *exec.Cmd) error {
	// Bound pipe cleanup even when a child detaches and retains output descriptors.
	command.WaitDelay = 2 * time.Second
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		return err
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
