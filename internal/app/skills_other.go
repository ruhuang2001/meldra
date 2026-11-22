//go:build !darwin && !linux

package app

import (
	"fmt"
	"os"
)

const skillReadFlags = os.O_RDONLY

func checkSkillLinkCount(info os.FileInfo) error {
	return fmt.Errorf("secure skill file reads are unsupported on this platform")
}
