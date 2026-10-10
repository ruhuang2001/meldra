//go:build !darwin && !linux

package app

import (
	"io"
	"os"
	"time"
)

func deadlineOutput(output io.Writer) (*os.File, func(), error) {
	file, ok := output.(*os.File)
	if !ok {
		return nil, func() {}, nil
	}
	if err := file.SetWriteDeadline(time.Time{}); err != nil {
		return nil, func() {}, nil
	}
	return file, func() { _ = file.SetWriteDeadline(time.Time{}) }, nil
}
