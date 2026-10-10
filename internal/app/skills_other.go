//go:build !darwin && !linux

package app

import (
	"fmt"
	"os"
)

// Release platforms use the nonblocking, no-follow flags in skills_unix.go.
const skillReadFlags = os.O_RDONLY

func checkSkillLinkCount(info os.FileInfo) error {
	return fmt.Errorf("secure skill file reads are unsupported on this platform")
}
