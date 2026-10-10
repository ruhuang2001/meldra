package app

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func independentOutput(file *os.File, info os.FileInfo) (*os.File, error) {
	connection, err := file.SyscallConn()
	if err != nil {
		return nil, err
	}
	var bounded *os.File
	var openErr error
	if err := connection.Control(func(fd uintptr) {
		// Opening procfs fd paths, unlike dup or Darwin's /dev/fd, creates a
		// new file description with independent status flags.
		bounded, openErr = os.OpenFile(fmt.Sprintf("/proc/self/fd/%d", fd), os.O_WRONLY|syscall.O_NONBLOCK|syscall.O_NOCTTY, 0)
	}); err != nil {
		return nil, err
	}
	if openErr != nil {
		return nil, openErr
	}
	opened, err := bounded.Stat()
	if err != nil || !os.SameFile(info, opened) {
		_ = bounded.Close()
		return nil, errors.New("output descriptor changed while reopening")
	}
	return bounded, nil
}
