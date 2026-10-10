package app

import (
	"errors"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

func independentOutput(file *os.File, info os.FileInfo) (*os.File, error) {
	if info.Mode()&os.ModeCharDevice == 0 {
		return nil, errors.New("inherited pipe needs an output relay")
	}
	connection, err := file.SyscallConn()
	if err != nil {
		return nil, err
	}
	var path [1024]byte
	var lookupErr error
	if err := connection.Control(func(fd uintptr) {
		_, _, errno := syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_GETPATH, uintptr(unsafe.Pointer(&path[0])))
		if errno != 0 {
			lookupErr = errno
		}
	}); err != nil {
		return nil, err
	}
	if lookupErr != nil {
		return nil, lookupErr
	}
	name, _, _ := strings.Cut(string(path[:]), "\x00")
	if !strings.HasPrefix(name, "/dev/") || strings.HasPrefix(name, "/dev/fd/") || name == "/dev/stdout" || name == "/dev/stderr" {
		return nil, errors.New("terminal has no independent device path")
	}
	bounded, err := os.OpenFile(name, os.O_WRONLY|syscall.O_NONBLOCK|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, err
	}
	opened, err := bounded.Stat()
	if err != nil || !os.SameFile(info, opened) {
		_ = bounded.Close()
		return nil, errors.New("terminal device changed while opening output")
	}
	return bounded, nil
}
