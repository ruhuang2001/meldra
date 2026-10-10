//go:build !darwin && !linux

package app

import (
	"fmt"
	"os"
)

// Platforms without a verified no-follow/nonblocking implementation reject
// skill reads rather than risking a symlink or FIFO race in OpenFile.
const skillReadFlags = os.O_RDONLY

func checkSkillLinkCount(info os.FileInfo) error {
	return fmt.Errorf("secure skill file reads are unsupported on this platform")
}
