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

type taskCompactionInspection struct {
	EventID      string `json:"event_id"`
	Sequence     int64  `json:"sequence"`
	FromBytes    int    `json:"from_bytes"`
	ToBytes      int    `json:"to_bytes"`
	KeptMessages int    `json:"kept_messages"`
	SourceTurns  struct {
		FirstSequence int64 `json:"first_sequence"`
		LastSequence  int64 `json:"last_sequence"`
	} `json:"source_turns"`
	Artifact           task.ArtifactRef `json:"artifact"`
	CompactedCalls     []string         `json:"compacted_calls"`
	CompactedCallCount int              `json:"compacted_call_count"`
	Trigger            string           `json:"trigger"`
	Focus              string           `json:"focus,omitempty"`
	Calls              []task.ToolCall  `json:"tool_calls"`
}

func taskCompactionsForInspection(ctx context.Context, db *taskstore.Store, id string, calls []task.ToolCall) ([]taskCompactionInspection, error) {
	byID := make(map[string]task.ToolCall, len(calls))
	for _, call := range calls {
		byID[call.ID] = call
	}
	compactions := []taskCompactionInspection{}
	var after int64
	for {
		events, err := db.Events(ctx, id, after, 1000)
		if err != nil {
			return nil, err
		}
		if len(events) == 0 {
			return compactions, nil
		}
		for _, event := range events {
			after = event.Sequence
			if event.Kind != "context.compacted" {
				continue
			}
			var compact taskCompactionInspection
			if err := json.Unmarshal(event.Data, &compact); err != nil {
				return nil, fmt.Errorf("read compaction event %d: %w", event.Sequence, err)
			}
			data, err := db.ReadArtifact(ctx, compact.Artifact)
			if err != nil {
				return nil, fmt.Errorf("read compaction event %d artifact: %w", event.Sequence, err)
			}
			var artifact struct {
				SchemaVersion  int      `json:"schema_version"`
				CompactedCalls []string `json:"compacted_calls"`
			}
			if err := json.Unmarshal(data, &artifact); err != nil {
				return nil, fmt.Errorf("decode compaction event %d artifact: %w", event.Sequence, err)
			}
			if artifact.SchemaVersion != 1 || len(artifact.CompactedCalls) != compact.CompactedCallCount {
				return nil, fmt.Errorf("compaction event %d artifact has an invalid schema or call count", event.Sequence)
			}
			compact.EventID, compact.Sequence = event.ID, event.Sequence
			compact.CompactedCalls = artifact.CompactedCalls
			compact.Calls = make([]task.ToolCall, 0, len(artifact.CompactedCalls))
			seen := make(map[string]bool, len(artifact.CompactedCalls))
			for _, callID := range artifact.CompactedCalls {
				call, ok := byID[callID]
				if !ok || seen[callID] {
					return nil, fmt.Errorf("compaction event %d references a missing or duplicate tool call %q", event.Sequence, callID)
				}
				seen[callID] = true
				compact.Calls = append(compact.Calls, call)
			}
			compactions = append(compactions, compact)
		}
	}
}

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
		return &usageError{fmt.Errorf("usage: meldra task show|events|resume|resolve|resolve-process TASK_ID [options]")}
	}
	action, id := args[0], args[1]
	if !validSessionID(id) {
		return &usageError{fmt.Errorf("invalid task ID")}
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
	case "resolve-process":
		if len(args) != 5 || args[3] != "--reason" || strings.TrimSpace(args[4]) == "" {
			return fmt.Errorf("usage: meldra task resolve-process TASK_ID PROCESS_ID --reason TEXT")
		}
		lease, err := db.Acquire(ctx, id, record.Workspace)
		if err != nil {
			return err
		}
		defer lease.Close()
		process, err := db.GetProcess(ctx, args[2])
		if err != nil {
			return err
		}
		if process.TaskID != id {
			return fmt.Errorf("process belongs to another task")
		}
		if process.State.Terminal() && process.Effects != "unknown" {
			return task.ErrTransition
		}
		if err := db.RecoverInterrupted(ctx, lease, id); err != nil {
			return err
		}
		if err := db.ResolveProcess(ctx, lease, args[2], args[4]); err != nil {
			return err
		}
		fmt.Fprintln(out, "Process effects resolved; resume the task explicitly.")
		return nil
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
		processes, err := db.Processes(ctx, id)
		if err != nil {
			return err
		}
		compactions, err := taskCompactionsForInspection(ctx, db, id, calls)
		if err != nil {
			return err
		}
		if len(args) == 3 {
			return json.NewEncoder(out).Encode(struct {
				Compactions   []taskCompactionInspection `json:"compactions"`
				Processes     []task.Process             `json:"processes"`
				SchemaVersion int                        `json:"schema_version"`
				Task          task.Task                  `json:"task"`
				Runs          []task.Run                 `json:"runs"`
				Calls         []task.ToolCall            `json:"tool_calls"`
				Approvals     []task.Approval            `json:"approvals"`
			}{compactions, processes, task.SchemaVersion, record, runs, calls, approvals})
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
		for _, process := range processes {
			fmt.Fprintf(out, "Process %s: %s (effects: %s)\n", process.ID, process.State, process.Effects)
		}
		for _, approval := range approvals {
			fmt.Fprintf(out, "Approval %s for %s: %s\n", approval.ID, approval.ToolCallID, approval.Decision)
		}
		if len(compactions) > 0 {
			fmt.Fprintln(out, "Context compactions:")
		}
		for _, compact := range compactions {
			fmt.Fprintf(out, "  Event %d: %s, turns %d–%d, %d → %d bytes, kept %d messages\n",
				compact.Sequence, sanitizeTerminalText(compact.Trigger), compact.SourceTurns.FirstSequence,
				compact.SourceTurns.LastSequence, compact.FromBytes, compact.ToBytes, compact.KeptMessages)
			fmt.Fprintf(out, "    Artifact: %s\n", compact.Artifact.ID)
			if compact.Focus != "" {
				fmt.Fprintf(out, "    Focus: %s\n", sanitizeTerminalText(compact.Focus))
			}
			for _, call := range compact.Calls {
				fmt.Fprintf(out, "    Folded tool %s %s: %s\n", sanitizeTerminalText(call.ID), sanitizeTerminalText(call.Name), call.Status)
			}
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
		optionArgs := make([]string, 0, len(args)-2)
		approvalHash := ""
		for i := 2; i < len(args); i++ {
			if args[i] == "--approve-detail-hash" {
				i++
				if i >= len(args) || approvalHash != "" {
					return &usageError{fmt.Errorf("--approve-detail-hash requires one SHA-256 hash")}
				}
				approvalHash = args[i]
			} else if value, ok := strings.CutPrefix(args[i], "--approve-detail-hash="); ok {
				if approvalHash != "" || value == "" {
					return &usageError{fmt.Errorf("--approve-detail-hash requires one SHA-256 hash")}
				}
				approvalHash = value
			} else {
				optionArgs = append(optionArgs, args[i])
			}
		}
		options, err := parseChatOptions(optionArgs)
		if err != nil {
			return err
		}
		if approvalHash != "" {
			if len(approvalHash) != 64 || strings.Trim(approvalHash, "0123456789abcdef") != "" {
				return &usageError{fmt.Errorf("--approve-detail-hash requires a lowercase SHA-256 hash")}
			}
			if options.AutoApprove {
				return &usageError{fmt.Errorf("--approve-detail-hash cannot be combined with --auto-approve")}
			}
			options.ApproveDetailHash = approvalHash
			if options.OutputFormat == "" {
				options.OutputFormat = "stream-json"
			}
		}
		if options.Resume != "" {
			return &usageError{fmt.Errorf("task resume does not accept --resume")}
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
