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
