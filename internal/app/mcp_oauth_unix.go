//go:build darwin || linux

package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

type mcpOAuthLock struct{ file *os.File }

func acquireMCPOAuthLock(ctx context.Context, path string) (*mcpOAuthLock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	held, err := file.Stat()
	if err != nil || !held.Mode().IsRegular() {
		file.Close()
		return nil, fmt.Errorf("OAuth lock must be a regular file")
	}
	if err = file.Chmod(0o600); err != nil {
		file.Close()
		return nil, err
	}
	for {
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			current, statErr := os.Lstat(path)
			if statErr != nil || !os.SameFile(held, current) || ctx.Err() != nil {
				syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
				file.Close()
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				return nil, fmt.Errorf("OAuth lock was replaced")
			}
			return &mcpOAuthLock{file: file}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			file.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			file.Close()
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}
func (l *mcpOAuthLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	return errors.Join(syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN), l.file.Close())
}
func mcpOAuthLockPath(paths ConfigPaths, name string) string {
	return paths.Home + string(os.PathSeparator) + "mcp-oauth-" + name + ".lock"
}
