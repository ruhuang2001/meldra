//go:build !darwin && !linux

package app

import (
	"context"
	"os/exec"
	"time"
)

func runCommandProcess(ctx context.Context, command *exec.Cmd, onStart ...func()) error {
	prepareCommandProcess(command)
	if err := command.Start(); err != nil {
		return err
	}
	for _, started := range onStart {
		started()
	}
	return waitCommandProcess(ctx, command)
}

func prepareCommandProcess(command *exec.Cmd) {
	command.WaitDelay = 2 * time.Second
	command.Cancel = nil
}

func waitCommandProcess(ctx context.Context, command *exec.Cmd) error {
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = command.Process.Kill()
		return <-done
	}
}

func processSignal(_ *exec.Cmd) string { return "" }

func prepareMCPProcess(_ *exec.Cmd) {}

func stopMCPProcess(command *exec.Cmd) {
	if command.Process != nil {
		_ = command.Process.Kill()
	}
}
