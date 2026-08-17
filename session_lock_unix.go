//go:build darwin || linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func acquireSessionStoreLock(directory string) (func(), error) {
	file, err := os.OpenFile(filepath.Join(directory, ".save.lock"), os.O_CREATE|os.O_RDWR, privateFilePerm)
	if err != nil {
		return nil, fmt.Errorf("open session save lock: %w", err)
	}
	if err := file.Chmod(privateFilePerm); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("secure session save lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("lock session store: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}
