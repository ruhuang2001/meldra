package app

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
)

func syncDirectory(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open directory for sync: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP) {
			return nil
		}
		return fmt.Errorf("sync directory: %w", err)
	}
	return nil
}

func makeDirectoryTreeDurable(path string, mode fs.FileMode, syncParent func(string) error) error {
	_, err := makeDirectoryTreeDurableTracked(path, mode, syncParent)
	return err
}

func makeDirectoryTreeDurableTracked(path string, mode fs.FileMode, syncParent func(string) error) ([]string, error) {
	if syncParent == nil {
		syncParent = syncDirectory
	}
	path = filepath.Clean(path)
	missing := []string{}
	current := path
	for {
		info, err := os.Stat(current)
		if err == nil {
			if !info.IsDir() {
				return nil, fmt.Errorf("directory component %s is not a directory", current)
			}
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("inspect directory component %s: %w", current, err)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil, fmt.Errorf("find existing parent for %s", path)
		}
		missing = append(missing, current)
		current = parent
	}

	created := make([]string, 0, len(missing))
	for index := len(missing) - 1; index >= 0; index-- {
		directory := missing[index]
		wasCreated := false
		if err := os.Mkdir(directory, mode); err != nil {
			if !errors.Is(err, fs.ErrExist) {
				return created, fmt.Errorf("create directory %s: %w", directory, err)
			}
			info, statErr := os.Stat(directory)
			if statErr != nil {
				return created, fmt.Errorf("inspect concurrently created directory %s: %w", directory, statErr)
			}
			if !info.IsDir() {
				return created, fmt.Errorf("directory component %s is not a directory", directory)
			}
		} else {
			wasCreated = true
		}
		if wasCreated {
			created = append(created, directory)
		}
		if err := syncParent(filepath.Dir(directory)); err != nil {
			return created, fmt.Errorf("sync parent directory for %s: %w", directory, err)
		}
	}
	return created, nil
}
