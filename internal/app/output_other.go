//go:build !darwin && !linux

package app

import (
	"io"
	"os"
	"time"
)

func deadlineOutput(output io.Writer) (*os.File, func() error, error) {
	file, ok := output.(*os.File)
	if !ok {
		return nil, func() error { return nil }, nil
	}
	if err := file.SetWriteDeadline(time.Time{}); err != nil {
		return nil, func() error { return nil }, nil
	}
	return file, func() error { return file.SetWriteDeadline(time.Time{}) }, nil
}
