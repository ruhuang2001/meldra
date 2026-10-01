//go:build darwin || linux

package store

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"meldra/internal/task"
)

// Device and inode identify the directory independently of case, symlink, or
// mount-path spelling. Path hashes alone do not exclude aliases on APFS.
func directoryIdentity(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("ownership path is not a directory: %s", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", errors.New("filesystem identity unavailable")
	}
	return fmt.Sprintf("%d:%d", stat.Dev, stat.Ino), nil
}

func lockFile(path string) (*os.File, error) {
	// O_NOFOLLOW closes the final-component symlink race for lock inodes.
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	if err := f.Chmod(0600); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, task.ErrBusy
		}
		return nil, err
	}
	return f, nil
}

func unlockFile(f *os.File) error {
	return errors.Join(syscall.Flock(int(f.Fd()), syscall.LOCK_UN), f.Close())
}
