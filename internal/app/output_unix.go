//go:build darwin || linux

package app

import (
	"errors"
	"io"
	"os"
	"syscall"
	"time"
)

// deadlineOutput puts a private duplicate into Go's poller. Standard stdout
// descriptors otherwise use blocking writes and reject SetWriteDeadline. The
// original descriptor stays open and its status flags are restored on cleanup.
func deadlineOutput(output io.Writer) (*os.File, func(), error) {
	file, ok := output.(*os.File)
	if !ok {
		return nil, func() {}, nil
	}
	info, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	if info.Mode().IsRegular() {
		return nil, func() {}, nil
	}
	connection, err := file.SyscallConn()
	if err != nil {
		return nil, nil, err
	}
	fd := -1
	var flags uintptr
	var operationErr error
	if err := connection.Control(func(original uintptr) {
		var errno syscall.Errno
		flags, _, errno = syscall.Syscall(syscall.SYS_FCNTL, original, syscall.F_GETFL, 0)
		if errno != 0 {
			operationErr = errno
			return
		}
		fd, operationErr = syscall.Dup(int(original))
		if operationErr == nil {
			syscall.CloseOnExec(fd)
			operationErr = syscall.SetNonblock(fd, true)
		}
	}); err != nil {
		operationErr = errors.Join(operationErr, err)
	}
	if operationErr != nil {
		if fd >= 0 {
			_ = syscall.Close(fd)
		}
		return nil, nil, operationErr
	}
	bounded := os.NewFile(uintptr(fd), file.Name()+" (bounded output)")
	cleanup := func() {
		_, _, _ = syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_SETFL, flags)
		_ = bounded.Close()
	}
	if err := bounded.SetWriteDeadline(time.Time{}); err != nil {
		cleanup()
		return nil, nil, err
	}
	return bounded, cleanup, nil
}
