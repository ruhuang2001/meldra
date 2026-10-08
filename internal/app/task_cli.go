package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	taskstore "meldra/internal/store"
	"meldra/internal/task"
)

func taskStoreForRead(paths ConfigPaths) (*taskstore.Store, error) {
	if _, err := os.Lstat(filepath.Join(taskDirectory(paths), "tasks.db")); err != nil {
		return nil, err
	}
	return taskstore.Open(taskDirectory(paths))
}

func runTasksCommand(ctx context.Context, args []string, out io.Writer) error {
	if len(args) > 1 || len(args) == 1 && args[0] != "--json" {
		return fmt.Errorf("usage: meldra tasks [--json]")
	}
	paths, err := ResolveConfigPaths()
	if err != nil {
		return err
	}
	db, err := taskStoreForRead(paths)
	if errors.Is(err, os.ErrNotExist) {
		if len(args) > 0 {
			_, err = fmt.Fprintln(out, "[]")
		} else {
			_, err = fmt.Fprintln(out, "No recorded tasks.")
		}
		return err
	}
	if err != nil {
		return err
	}
	defer db.Close()
	tasks, err := db.ListTasks(ctx, "", 1000)
	if err != nil {
		return err
	}
	if len(args) > 0 {
		if tasks == nil {
			tasks = []task.Task{}
		}
		return json.NewEncoder(out).Encode(tasks)
	}
	for _, record := range tasks {
		if _, err := fmt.Fprintf(out, "%s  %-18s %s\n", record.ID, record.Status, sanitizeTerminalText(record.Goal)); err != nil {
			return err
		}
	}
	return nil
}

func runTaskCommand(ctx context.Context, args []string, in io.Reader, out io.Writer) (err error) {
	chatOutput := out
	checked := &taskOutput{Writer: out}
	out = checked
	defer func() { err = errors.Join(err, checked.err) }()
	if len(args) < 2 {
		return fmt.Errorf("usage: meldra task show|events|resume|resolve TASK_ID [options]")
	}
	action, id := args[0], args[1]
	if !validSessionID(id) {
		return fmt.Errorf("invalid task ID")
	}
	paths, err := ResolveConfigPaths()
	if err != nil {
		return err
	}
	db, err := taskStoreForRead(paths)
	if err != nil {
		return fmt.Errorf("open tasks: %w", err)
	}
	defer db.Close()
	record, err := db.GetTask(ctx, id)
	if err != nil {
		return err
	}
	switch action {
	case "show":
		if len(args) > 3 || len(args) == 3 && args[2] != "--json" {
			return fmt.Errorf("usage: meldra task show TASK_ID [--json]")
		}
		runs, err := db.Runs(ctx, id)
		if err != nil {
			return err
		}
		calls, err := db.ToolCalls(ctx, id)
		if err != nil {
			return err
		}
		approvals, err := db.Approvals(ctx, id)
		if err != nil {
			return err
		}
		if len(args) == 3 {
			return json.NewEncoder(out).Encode(struct {
				SchemaVersion int             `json:"schema_version"`
				Task          task.Task       `json:"task"`
				Runs          []task.Run      `json:"runs"`
				Calls         []task.ToolCall `json:"tool_calls"`
				Approvals     []task.Approval `json:"approvals"`
			}{task.SchemaVersion, record, runs, calls, approvals})
		}
		fmt.Fprintf(out, "Task: %s\nStatus: %s\nWorkspace: %s\nGoal: %s\n", record.ID, record.Status, sanitizeTerminalText(record.Workspace), sanitizeTerminalText(record.Goal))
		if record.Status == task.Running || record.Status == task.WaitingApproval {
			fmt.Fprintln(out, "Recorded state may be stale after a crash; inspection does not restart or recover it.")
		}
		for _, run := range runs {
			fmt.Fprintf(out, "Run %s: %s (%s)\n", run.ID, run.Status, sanitizeTerminalText(run.Reason))
		}
		for _, call := range calls {
			fmt.Fprintf(out, "Tool %s %s: %s\n", call.ID, sanitizeTerminalText(call.Name), call.Status)
			if call.Result.Error != "" {
				fmt.Fprintln(out, sanitizeTerminalText(call.Result.Error))
			}
		}
		for _, approval := range approvals {
			fmt.Fprintf(out, "Approval %s for %s: %s\n", approval.ID, approval.ToolCallID, approval.Decision)
		}
		return nil
	case "events":
		var after int64
		jsonMode := false
		for i := 2; i < len(args); i++ {
			switch args[i] {
			case "--json":
				jsonMode = true
			case "--after":
				i++
				if i >= len(args) {
					return fmt.Errorf("--after requires sequence")
				}
				after, err = strconv.ParseInt(args[i], 10, 64)
				if err != nil || after < 0 {
					return fmt.Errorf("invalid sequence")
				}
			default:
				return fmt.Errorf("usage: meldra task events TASK_ID --json [--after N]")
			}
		}
		if !jsonMode {
			return fmt.Errorf("events requires --json")
		}
		events, err := db.Events(ctx, id, after, 1000)
		if err != nil {
			return err
		}
		encoder := json.NewEncoder(out)
		for _, event := range events {
			if err := encoder.Encode(event); err != nil {
				return err
			}
		}
		return nil
	case "resume":
		options, err := parseChatOptions(args[2:])
		if err != nil {
			return err
		}
		if options.Resume != "" {
			return fmt.Errorf("task resume does not accept --resume")
		}
		if options.workspaceExplicit {
			canonical, err := canonicalWorkspacePath(options.Workspace)
			if err != nil {
				return err
			}
			if canonical != record.Workspace {
				return fmt.Errorf("task belongs to workspace %s", sanitizeTerminalText(record.Workspace))
			}
		}
		options.Resume = record.SessionID
		options.Workspace = record.Workspace
		options.workspaceExplicit = true
		if options.Prompt == "" {
			options.Prompt = "Continue the original task from its recorded execution state. Inspect any unresolved work before acting."
		}
		return runChat(ctx, in, chatOutput, options)
	case "resolve":
		if len(args) != 7 || args[3] != "--outcome" || args[5] != "--reason" || strings.TrimSpace(args[6]) == "" {
			return fmt.Errorf("usage: meldra task resolve TASK_ID CALL_ID --outcome succeeded|failed --reason TEXT")
		}
		status := task.ToolStatus(args[4])
		if status != task.ToolSucceeded && status != task.ToolFailed {
			return fmt.Errorf("outcome must be succeeded or failed")
		}
		lease, err := db.Acquire(ctx, id, record.Workspace)
		if err != nil {
			return err
		}
		defer lease.Close()
		call, err := db.GetToolCall(ctx, args[2])
		if err != nil {
			return err
		}
		if call.TaskID != id {
			return fmt.Errorf("call belongs to another task")
		}
		if call.Status != task.ToolUnknown && call.Status != task.ToolRunning {
			return task.ErrTransition
		}
		if err := db.RecoverInterrupted(ctx, lease, id); err != nil {
			return err
		}
		call, err = db.GetToolCall(ctx, args[2])
		if err != nil {
			return err
		}
		if call.TaskID != id {
			return fmt.Errorf("call belongs to another task")
		}
		result := task.Result{Status: status, Artifacts: call.Result.Artifacts}
		result.Output = "User reconciliation: " + args[6]
		result.Retryable = false
		if err := db.ResolveTool(ctx, lease, call.ID, result, args[6]); err != nil {
			return err
		}
		fmt.Fprintln(out, "Outcome recorded. No tools ran. Resume explicitly to continue.")
		return nil
	default:
		return fmt.Errorf("unknown task command %q", action)
	}
}

// Preserve output failures even on the human-readable inspection paths.
type taskOutput struct {
	io.Writer
	err error
}

func (w *taskOutput) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	n, err := w.Writer.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	w.err = err
	return n, err
}

// A crash after task creation but before the first JSON snapshot must remain
// explicitly resumable. Missing snapshots can be reconstructed from the ledger;
// corrupt or inaccessible snapshots are never silently replaced.
func loadSessionForResume(paths ConfigPaths, id string) (*Session, error) {
	legacy := NewSessionStore(paths)
	session, err := legacy.Load(id)
	if err == nil {
		return session, nil
	}
	if !validSessionID(id) {
		return nil, err
	}
	for _, path := range []string{legacy.path(id), newTaskSessionStore(paths).path(id)} {
		if _, checkErr := os.Lstat(path); !errors.Is(checkErr, os.ErrNotExist) {
			return nil, err
		}
	}
	db, openErr := taskStoreForRead(paths)
	if openErr != nil {
		if errors.Is(openErr, os.ErrNotExist) {
			return nil, err
		}
		return nil, fmt.Errorf("reconstruct session: open task store: %w", openErr)
	}
	defer db.Close()
	record, lookupErr := db.GetTask(context.Background(), id)
	if lookupErr != nil {
		if errors.Is(lookupErr, task.ErrNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("reconstruct session: read task: %w", lookupErr)
	}
	return &Session{ID: record.SessionID, Workspace: record.Workspace, CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt, resumed: true, taskSnapshot: true}, nil
}
