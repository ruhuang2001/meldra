package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type synchronizedWriter struct {
	mu        sync.Mutex
	writer    io.Writer
	ctx       context.Context
	deadline  interface{ SetWriteDeadline(time.Time) error }
	err       error
	live      bool
	closeOnce sync.Once
	cleanup   func()
}

func (w *synchronizedWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return 0, w.err
	}
	if w.ctx != nil && w.ctx.Err() != nil {
		w.err = w.ctx.Err()
		return 0, w.err
	}
	if w.deadline != nil {
		deadline := time.Now().Add(time.Second)
		if w.ctx != nil {
			if end, ok := w.ctx.Deadline(); ok && end.Before(deadline) {
				deadline = end
			}
		}
		if err := w.deadline.SetWriteDeadline(deadline); err != nil {
			w.err = err
			return 0, err
		}
		if w.ctx != nil {
			cancelled := make(chan struct{})
			stop := context.AfterFunc(w.ctx, func() { _ = w.deadline.SetWriteDeadline(time.Now()); close(cancelled) })
			defer func() {
				if !stop() {
					<-cancelled
				}
				_ = w.deadline.SetWriteDeadline(time.Time{})
			}()
		} else {
			defer w.deadline.SetWriteDeadline(time.Time{})
		}
	}
	n, err := w.writer.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err != nil {
		w.err = err
	}
	return n, err
}

func (w *synchronizedWriter) Err() error { w.mu.Lock(); defer w.mu.Unlock(); return w.err }

// Fd preserves terminal detection and window-size queries without calling
// os.File.Fd, which would switch a pollable descriptor back to blocking mode.
// Non-file writers deliberately return the invalid descriptor sentinel.
func (w *synchronizedWriter) Fd() uintptr {
	file, ok := w.writer.(*os.File)
	if !ok {
		return ^uintptr(0)
	}
	connection, err := file.SyscallConn()
	if err != nil {
		return ^uintptr(0)
	}
	fd := ^uintptr(0)
	if err := connection.Control(func(value uintptr) { fd = value }); err != nil {
		return ^uintptr(0)
	}
	return fd
}

// Terminal libraries use io.ReadWriteCloser plus Fd to identify output devices.
// This wrapper owns only its output duplicate; it neither reads the terminal's
// input stream nor closes the caller's original file.
func (w *synchronizedWriter) Read([]byte) (int, error) { return 0, os.ErrPermission }

func (w *synchronizedWriter) Close() error {
	w.closeOnce.Do(func() {
		if w.cleanup != nil {
			w.cleanup()
		}
	})
	return nil
}

// Ordinary embedders retain the usual io.Writer contract. Background live
// output is only enabled for bounded OS descriptors or the nonblocking discard
// sink; an arbitrary writer may block forever and cannot be safely detached.
func newSynchronizedWriter(ctx context.Context, output io.Writer) (*synchronizedWriter, func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	w := &synchronizedWriter{writer: output, ctx: ctx, live: output == io.Discard}
	bounded, cleanup, err := deadlineOutput(output)
	if err != nil {
		return nil, nil, err
	}
	if bounded != nil {
		w.writer = bounded
		w.deadline = bounded
		w.live = true
	}
	w.cleanup = cleanup
	return w, func() { _ = w.Close() }, nil
}

func (w *synchronizedWriter) writeLive(ctx context.Context, data []byte) (int, error) {
	if !w.live {
		return 0, errors.New("live output requires a bounded writer")
	}
	// Write has its own one-second deadline. The main context interrupts real
	// shutdown immediately; per-turn cancellation discards remaining updates.
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return w.Write(data)
}

type inputLine struct {
	text string
	err  error
}
type lineController struct {
	errMu   sync.Mutex
	readErr error
	ctx     context.Context
	cancel  context.CancelFunc
	input   *mcpCLIInput
	lines   chan inputLine
	done    chan struct{}
	human   atomic.Bool
	agent   *Agent
	output  io.Writer
}

// One reader owns stdin for chat, approvals and MCP forms. Only explicit slash
// controls are intercepted while busy; ordinary piped lines retain their order.
func newLineController(ctx context.Context, input *mcpCLIInput, agent *Agent, output io.Writer) *lineController {
	ctx, cancel := context.WithCancel(ctx)
	l := &lineController{ctx: ctx, cancel: cancel, input: input, agent: agent, output: output, lines: make(chan inputLine, 32), done: make(chan struct{})}
	go l.read()
	return l
}

func (l *lineController) read() {
	defer close(l.done)
	defer close(l.lines)
	for {
		line, err := l.input.ReadString('\n')
		if err != nil && err != io.EOF && l.ctx.Err() == nil {
			l.errMu.Lock()
			l.readErr = err
			l.errMu.Unlock()
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if line != "" && !l.human.Load() {
			if kind, text, ok := parseControlText(line); ok {
				c := l.agent.initControl()
				c.mu.Lock()
				active := c.active
				c.mu.Unlock()
				if active {
					controlErr := l.agent.SubmitControl(kind, text)
					if controlErr != nil {
						fmt.Fprintln(l.output, "Control failed:", controlErr)
					} else {
						fmt.Fprintln(l.output, map[string]string{"steer": "Steering received", "queue": "Queued", "stop": "Stop requested", "mode": "Mode change requested"}[kind])
					}
					if err != nil {
						return
					}
					continue
				}
			}
		}
		select {
		case l.lines <- inputLine{line, err}:
		case <-l.ctx.Done():
			return
		}
		if err != nil {
			return
		}
	}
}

func (l *lineController) next(ctx context.Context) (string, bool) {
	select {
	case line, ok := <-l.lines:
		return line.text, ok && (line.err == nil || line.err == io.EOF && line.text != "")
	case <-ctx.Done():
		return "", false
	}
}

func (l *lineController) humanInput(ctx context.Context, prompt string) (string, bool) {
	l.human.Store(true)
	defer l.human.Store(false)
	fmt.Fprintln(l.output, sanitizeTerminalText(prompt))
	return l.next(ctx)
}

func (l *lineController) close() {
	l.cancel()
	if l.input.interrupt != nil {
		l.input.interrupt.Cancel()
		<-l.done
	}
}

func (l *lineController) approve(ctx context.Context, request ApprovalRequest) bool {
	prompt := request.Prompt
	if prompt == "" {
		prompt = "Approve? [y/N]"
	}
	fmt.Fprintln(l.output, sanitizeTerminalText(request.Title+"\n"+request.Detail+"\n"+prompt))
	text, ok := l.next(ctx)
	return ok && (strings.EqualFold(strings.TrimSpace(text), "y") || strings.EqualFold(strings.TrimSpace(text), "yes"))
}

func (l *lineController) inputError() error { l.errMu.Lock(); defer l.errMu.Unlock(); return l.readErr }
