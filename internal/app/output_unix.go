//go:build darwin || linux

package app

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// deadlineOutput never changes the flags of the caller's open file description.
// Dup would share those flags, leaving the caller nonblocking after a crash.
func deadlineOutput(output io.Writer) (*os.File, func() error, error) {
	file, ok := output.(*os.File)
	if !ok {
		return nil, func() error { return nil }, nil
	}
	info, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	if info.Mode().IsRegular() {
		return nil, func() error { return nil }, nil
	}
	if nullInfo, nullErr := os.Stat(os.DevNull); nullErr == nil && os.SameFile(info, nullInfo) {
		return nil, func() error { return nil }, nil
	}
	bounded, err := independentOutput(file, info)
	if err != nil {
		// A descriptor already in nonblocking mode can safely be duplicated
		// without changing its status flags. Deadlines are Go-file-local.
		bounded, err = duplicateNonblockingOutput(file)
	}
	if err == nil {
		if err = bounded.SetWriteDeadline(time.Time{}); err == nil {
			return bounded, bounded.Close, nil
		}
		_ = bounded.Close()
	}
	if info.Mode()&os.ModeCharDevice != 0 {
		return nil, nil, fmt.Errorf("open independent terminal output: %w", err)
	}
	return relayedOutput(file)
}

func duplicateNonblockingOutput(file *os.File) (*os.File, error) {
	connection, err := file.SyscallConn()
	if err != nil {
		return nil, err
	}
	fd := -1
	var operationErr error
	if err := connection.Control(func(original uintptr) {
		flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, original, syscall.F_GETFL, 0)
		if errno != 0 {
			operationErr = errno
			return
		}
		if flags&syscall.O_NONBLOCK == 0 {
			operationErr = errors.New("output descriptor is blocking")
			return
		}
		fd, operationErr = syscall.Dup(int(original))
		if operationErr == nil {
			syscall.CloseOnExec(fd)
		}
	}); err != nil {
		return nil, err
	}
	if operationErr != nil {
		return nil, operationErr
	}
	return os.NewFile(uintptr(fd), file.Name()+" (bounded output)"), nil
}

// Some systems cannot reopen an inherited pipe as an independent file
// description. A relay keeps its blocking writes outside this Go process. The
// separate liveness pipe terminates the entire private group even if the owner
// exits abruptly while cat is blocked. No workspace executable or shell input
// participates in this fixed transport protocol.
func relayedOutput(output *os.File) (*os.File, func() error, error) {
	input, bounded, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	life, owner, err := os.Pipe()
	if err != nil {
		_ = input.Close()
		_ = bounded.Close()
		return nil, nil, err
	}
	command := exec.Command("/bin/sh", "-c", `
trap 'trap "" TERM; kill -TERM "-$$" 2>/dev/null; wait; exit 143' TERM
/bin/cat <&3 3<&- 4<&- &
relay=$!
owner=$$
(
  exec 3<&-
  read -r unused <&4
  kill -TERM "$owner"
) >/dev/null 2>&1 &
watcher=$!
exec 3<&- 4<&-
wait "$relay"
status=$?
kill -TERM "$watcher" 2>/dev/null
wait "$watcher" 2>/dev/null
exit "$status"
`)
	command.Env = []string{}
	command.ExtraFiles = []*os.File{input, life}
	connection, err := output.SyscallConn()
	if err != nil {
		_ = input.Close()
		_ = bounded.Close()
		_ = life.Close()
		_ = owner.Close()
		return nil, nil, err
	}
	fd := -1
	var duplicateErr error
	if err := connection.Control(func(original uintptr) {
		fd, duplicateErr = syscall.Dup(int(original))
		if duplicateErr == nil {
			syscall.CloseOnExec(fd)
		}
	}); err != nil {
		duplicateErr = errors.Join(duplicateErr, err)
	}
	if duplicateErr != nil {
		_ = input.Close()
		_ = bounded.Close()
		_ = life.Close()
		_ = owner.Close()
		if fd >= 0 {
			_ = syscall.Close(fd)
		}
		return nil, nil, duplicateErr
	}
	// NewFile notices O_NONBLOCK without claiming ownership of that flag;
	// unlike the caller's os.Pipe file, its Fd method will not clear it.
	transport := os.NewFile(uintptr(fd), "output relay destination")
	defer transport.Close()
	command.Stdout = transport
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		_ = input.Close()
		_ = bounded.Close()
		_ = life.Close()
		_ = owner.Close()
		return nil, nil, err
	}
	_ = input.Close()
	_ = life.Close()
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	cleanup := func() error {
		_ = bounded.Close()
		defer owner.Close()
		select {
		case err := <-done:
			return err
		case <-time.After(time.Second):
			// Closing liveness also covers a delayed helper startup. Killing
			// the group directly bounds shutdown if its watchdog was stopped.
			_ = owner.Close()
			_ = command.Process.Signal(syscall.SIGTERM)
			select {
			case err := <-done:
				return errors.Join(os.ErrDeadlineExceeded, err)
			case <-time.After(500 * time.Millisecond):
				_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
				return errors.Join(os.ErrDeadlineExceeded, <-done)
			}
		}
	}
	return bounded, cleanup, nil
}
