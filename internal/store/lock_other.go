//go:build !darwin && !linux

package store

import (
	"errors"
	"os"
	"path/filepath"
)

// Inspection remains possible on other platforms, but lockFile below refuses
// execution rather than pretending that path spelling establishes ownership.
func directoryIdentity(path string) (string, error) { return filepath.EvalSymlinks(path) }

func lockFile(string) (*os.File, error) {
	return nil, errors.New("task execution ownership is supported on Darwin and Linux only")
}
func unlockFile(f *os.File) error { return f.Close() }
