//go:build darwin || linux

package app

import (
	"fmt"
	"os"
	"syscall"
)

// Reject a final-component symlink swap, and never block if a regular file is
// replaced with a FIFO between Lstat and OpenFile. Stat still checks identity.
const skillReadFlags = os.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK

func checkSkillLinkCount(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 {
		return fmt.Errorf("hard links are not allowed in skill files")
	}
	return nil
}
