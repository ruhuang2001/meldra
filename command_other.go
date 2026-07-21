//go:build !darwin && !linux

package main

import (
	"context"
	"os/exec"
)

func runCommandProcess(_ context.Context, command *exec.Cmd) error {
	return command.Run()
}
