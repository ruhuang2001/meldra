//go:build darwin || linux

package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

func preflightUpdateTarget(target string) error {
	for _, marker := range []string{"/Cellar/", "/opt/homebrew/", "/linuxbrew/", "/.linuxbrew/"} {
		if strings.Contains(target, marker) {
			return errors.New("Homebrew-managed installation; use brew upgrade meldra")
		}
	}
	if strings.Contains(target, "/node_modules/") {
		return errors.New("npm-managed installation; update Meldra using npm")
	}
	if strings.Contains(target, "/nix/store/") {
		return errors.New("Nix-managed installation; update Meldra using Nix")
	}
	info, err := os.Lstat(target)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("update target must be a regular file")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return errors.New("installed binary is owned by another user; update it as its owner")
	}
	if err := syscall.Access(filepath.Dir(target), 2); err != nil {
		return fmt.Errorf("installation directory is not writable; update it as its owner: %w", err)
	}
	return nil
}
