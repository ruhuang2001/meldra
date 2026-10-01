//go:build !darwin && !linux

package store

import (
	"errors"
	"os"
)

func lockFile(string) (*os.File, error) {
	return nil, errors.New("task execution ownership is supported on Darwin and Linux only")
}
func unlockFile(f *os.File) error { return f.Close() }
