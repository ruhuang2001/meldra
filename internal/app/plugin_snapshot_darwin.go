package app

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func checkPluginSnapshotSource(file *os.File) error {
	_, err := unix.Fgetxattr(int(file.Fd()), "com.apple.quarantine", nil)
	if errors.Is(err, unix.ENOATTR) || errors.Is(err, unix.ENOTSUP) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("cannot snapshot a quarantined plugin executable")
}
