//go:build !darwin && !linux

package app

import (
	"context"
	"os/exec"
)

func runCommandProcess(_ context.Context, command *exec.Cmd, onStart ...func()) error {
	if err := command.Start(); err != nil {
		return err
	}
	for _, started := range onStart {
		started()
	}
	return command.Wait()
}

func prepareMCPProcess(_ *exec.Cmd) {}

func stopMCPProcess(command *exec.Cmd) {
	if command.Process != nil {
		_ = command.Process.Kill()
	}
}
