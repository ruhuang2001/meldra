package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"meldra/internal/tool"
)

type commandSpec struct {
	Executable        string            `json:"executable"`
	Arguments         []string          `json:"arguments"`
	Directory         string            `json:"directory"`
	TimeoutSeconds    int               `json:"timeout_seconds"`
	EnvironmentPolicy string            `json:"environment_policy"`
	Mode              ExecutionMode     `json:"mode"`
	Generation        uint64            `json:"generation"`
	PermissionPolicy  PermissionProfile `json:"permission_policy"`
	ApprovalRequired  bool              `json:"-"`
	DisplayCommand    string            `json:"-"`
}

// CommandOutput is a coalesced, sanitized presentation update. Receivers must
// enqueue without blocking; output retention never relies on UI delivery.
type CommandOutput struct {
	ProcessID string
	Text      string
	Cursor    int64
	Truncated bool
}

func (w *Workspace) SetCommandTimeout(seconds int) error {
	if seconds < 1 || seconds > maxCommandTimeout {
		return fmt.Errorf("command timeout must be 1..86400 seconds")
	}
	w.commandTimeout = seconds
	return nil
}

func (w *Workspace) SetCommandOutput(output func(CommandOutput)) { w.commandOutput = output }

// SetCommandExecutables permits explicit host executable identities. These
// extensions still require approval and do not weaken built-in Git restrictions.
func (w *Workspace) SetCommandExecutables(paths []string) error {
	configured := make(map[string]string, len(paths))
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			return fmt.Errorf("allowed executable must be an absolute path: %q", path)
		}
		canonical, err := w.trustedExecutable(path)
		if err != nil {
			return err
		}
		configured[path] = canonical
		configured[canonical] = canonical
	}
	w.commandExecutables = configured
	return nil
}

func (w *Workspace) approveCommandSpec(spec commandSpec) bool {
	detail, _ := json.Marshal(spec)
	text := renderCommand(spec.Executable, spec.Arguments)
	return w.requestApproval(ApprovalRequest{Kind: ApprovalCommand, Title: "Run command with OS user privileges", WorkspaceState: detail,
		Detail: text + fmt.Sprintf("\nWorkspace: %s\nDeadline: %d seconds\nEnvironment: %s\n\nThis command runs repository code with your OS user privileges and may access the filesystem and network.", spec.Directory, spec.TimeoutSeconds, spec.EnvironmentPolicy),
		Prompt: fmt.Sprintf("Run command with OS-user privileges? %s [y/N] ", text)})
}

func (w *Workspace) executeWithApproval(command string, args []string, seconds int, requestApproval bool) (string, error) {
	if err := w.processWriteConflict("run_command", nil); err != nil {
		return "", err
	}
	spec, err := w.prepareCommand(command, args, seconds)
	if err != nil {
		return "", err
	}
	return w.executePreparedCommand(spec, requestApproval)
}

func (w *Workspace) executePreparedCommand(spec commandSpec, requestApproval bool) (string, error) {
	if err := w.processWriteConflict("run_command", nil); err != nil {
		return "", err
	}
	ctx := w.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		tool.Observe(ctx, func(o *tool.Observation) { o.Result.Status = tool.Cancelled })
		return "Command cancelled before start.\n[cancelled]", nil
	}
	if requestApproval && spec.ApprovalRequired && !w.approveCommandSpec(spec) {
		return "Declined; command not run.", nil
	}
	if err := w.admitOperation(ctx); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(spec.TimeoutSeconds)*time.Second)
	defer cancel()
	environment, cleanup, err := newCommandEnvironment()
	if err != nil {
		return "", err
	}
	defer cleanup()
	log, err := newCommandLog(w.commandOutput, "")
	if err != nil {
		return "", err
	}
	defer log.close()
	cmd := exec.CommandContext(ctx, spec.Executable, spec.Arguments...)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = spec.Directory, environment, log, log
	started := false
	if err := w.admitOperation(ctx); err != nil {
		return "", err
	}
	err = runCommandProcess(ctx, cmd, func() {
		started = true
		tool.Observe(ctx, func(o *tool.Observation) { o.Started = true })
	})
	log.finish()
	status := 0
	if err != nil {
		if exit, ok := errors.AsType[*exec.ExitError](err); ok {
			status = exit.ExitCode()
		} else if ctx.Err() != nil {
			status = -1
		} else {
			tool.Observe(ctx, func(o *tool.Observation) {
				if !started {
					o.Result.Status = tool.Failed
				}
			})
			return "", err
		}
	}
	output, truncated, logErr := log.artifact()
	if logErr != nil {
		truncated = true
	}
	preview := output
	if len(preview) > maxToolOutput {
		preview = preview[:maxToolOutput]
		truncated = true
	}
	suffix := ""
	if truncated {
		suffix += "\n[output truncated]"
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		suffix += "\n[timed out]"
	} else if ctx.Err() != nil {
		suffix += "\n[cancelled]"
	}
	tool.Observe(ctx, func(o *tool.Observation) {
		if o.Result.ExitCode == nil || *o.Result.ExitCode == 0 {
			o.Result.ExitCode = new(status)
		}
		o.Result.Truncated = o.Result.Truncated || truncated
		o.Result.Attachments = append(o.Result.Attachments, tool.OutputArtifact{Name: fmt.Sprintf("command-%d.log", len(o.Result.Attachments)+1), Content: output, Truncated: truncated})
		if ctx.Err() != nil {
			if started {
				o.Result.Status = tool.Unknown
			} else {
				o.Result.Status = tool.Cancelled
			}
		} else if status != 0 {
			o.Result.Status = tool.Failed
		}
	})
	return fmt.Sprintf("command: %s %s\nstatus: %d\n%s", spec.DisplayCommand, strings.Join(spec.Arguments, " "), status, string(preview)) + suffix, nil
}

const commandLogLimit = 16 << 20

// commandLog drains all bytes, retains a bounded tail for cursor reads and a
// bounded private spool for the final artifact. Neither allocation nor disk use
// grows with command duration. Log write failures do not stop pipe drainage.
type commandLog struct {
	mu         sync.Mutex
	file       *os.File
	tail       []byte
	total      int64
	retained   int64
	truncated  bool
	err        error
	callback   func(CommandOutput)
	processID  string
	lastEmit   time.Time
	emitCursor int64
	tailState  terminalStream
	emitTimer  *time.Timer
	finished   bool
	closed     bool
}

func newCommandLog(callback func(CommandOutput), processID string) (*commandLog, error) {
	f, err := os.CreateTemp("", "meldra-command-log-*")
	if err != nil {
		return nil, err
	}
	return &commandLog{file: f, callback: callback, processID: processID}, nil
}

func (l *commandLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed || l.finished {
		return len(p), nil
	}
	l.total += int64(len(p))
	if len(p) >= maxToolOutput {
		l.tailState.append(l.tail, false, nil)
		l.tailState.append(p[:len(p)-maxToolOutput], false, nil)
		l.tail = append(l.tail[:0], p[len(p)-maxToolOutput:]...)
	} else {
		if overflow := len(l.tail) + len(p) - maxToolOutput; overflow > 0 {
			l.tailState.append(l.tail[:overflow], false, nil)
			copy(l.tail, l.tail[overflow:])
			l.tail = l.tail[:len(l.tail)-overflow]
		}
		l.tail = append(l.tail, p...)
	}
	keep := min(int64(len(p)), int64(commandLogLimit)-l.retained)
	if keep > 0 && l.err == nil {
		n, err := l.file.Write(p[:keep])
		l.retained += int64(n)
		l.err = err
	}
	if keep < int64(len(p)) || l.err != nil {
		l.truncated = true
	}
	if l.callback != nil {
		if time.Since(l.lastEmit) >= 100*time.Millisecond {
			l.flushLocked()
		} else if l.emitTimer == nil {
			l.emitTimer = time.AfterFunc(time.Until(l.lastEmit.Add(100*time.Millisecond)), func() {
				l.mu.Lock()
				defer l.mu.Unlock()
				l.emitTimer = nil
				if !l.closed && !l.finished {
					l.flushLocked()
				}
			})
		}
	}
	return len(p), nil
}

func (l *commandLog) read(cursor int64) (string, int64, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if cursor < 0 || cursor > l.total {
		return "", cursor, false, fmt.Errorf("output cursor must be between 0 and %d", l.total)
	}
	start := l.total - int64(len(l.tail))
	truncated := cursor < start
	cursor = max(cursor, start)
	text, next := l.readLocked(cursor, min(l.total, cursor+64<<10))
	return text, next, truncated, nil
}

// Replay the retained prefix from its bounded decoder checkpoint. This keeps
// independently polled raw-byte cursors meaningful even inside escape sequences,
// and avoids exposing their payload when the retained tail starts mid-sequence.
func (l *commandLog) readLocked(cursor, end int64) (string, int64) {
	if cursor == l.total {
		return "", cursor
	}
	start := l.total - int64(len(l.tail))
	decoder := l.tailState
	decoder.append(l.tail[:cursor-start], false, nil)
	var output strings.Builder
	final := l.finished && end == l.total
	decoder.append(l.tail[cursor-start:end-start], final, &output)
	if !final {
		// Return a cursor before an incomplete rune so the next observation can
		// emit it exactly once after its remaining bytes arrive.
		end = max(cursor, end-int64(decoder.pendingLen))
	}
	return output.String(), end
}

// Callbacks must enqueue promptly. Serialize them with close so no callback can
// outlive log ownership, including a timer that has already started firing.
func (l *commandLog) flushLocked() {
	if l.callback == nil || l.emitCursor == l.total {
		return
	}
	start := max(l.emitCursor, l.total-int64(len(l.tail)), l.total-8192)
	text, next := l.readLocked(start, l.total)
	if next == l.emitCursor && text == "" {
		return
	}
	update := CommandOutput{ProcessID: l.processID, Text: text, Cursor: next, Truncated: start > l.emitCursor}
	l.lastEmit, l.emitCursor = time.Now(), next
	l.callback(update)
}

func (l *commandLog) finish() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.finishLocked()
}

func (l *commandLog) finishLocked() {
	if l.finished || l.closed {
		return
	}
	l.finished = true
	if l.emitTimer != nil {
		l.emitTimer.Stop()
		l.emitTimer = nil
	}
	l.flushLocked()
}

func (l *commandLog) artifact() ([]byte, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.file.Seek(0, io.SeekStart); err != nil {
		return nil, true, err
	}
	data, err := io.ReadAll(io.LimitReader(l.file, commandLogLimit))
	return data, l.truncated, errors.Join(l.err, err)
}

func (l *commandLog) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.finishLocked()
	l.closed = true
	_ = l.file.Close()
	_ = os.Remove(l.file.Name())
}
