//go:build !darwin && !linux

package app

import "os/exec"

func newHookCommand(command string, args []string) *exec.Cmd {
	if len(args) > 0 {
		return exec.Command(command, args...)
	}
	return exec.Command("cmd", "/C", command)
}
