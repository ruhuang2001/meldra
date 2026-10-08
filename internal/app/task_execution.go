package app

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	taskstore "meldra/internal/store"
	"meldra/internal/task"
	"meldra/internal/tool"
)

// taskExecution is the foreground composition adapter. Its lease spans every
// side effect in a turn, while inspection commands need neither a model nor lease.
type taskExecution struct {
	requestSequence   int64
	paused            bool
	paths             ConfigPaths
	workspace         *Workspace
	session           *Session
	config            task.Config
	db                *taskstore.Store
	lease             *taskstore.Lease
	run               task.Run
	current           *task.ToolCall
	pendingApprovalID string
	err               error
	resume            bool
	context           string
}

type executionContextKey struct{}
type persistenceError struct{ err error }

func (e *persistenceError) Error() string { return "task recording failed: " + e.err.Error() }
func (e *persistenceError) Unwrap() error { return e.err }

func taskDirectory(paths ConfigPaths) string { return filepath.Join(paths.Home, "tasks") }
func providerIdentity(base string) string {
	parsed, err := url.Parse(base)
	if err != nil {
		return "custom"
	}
	// Credentials and query strings are never part of the persisted identity.
	return parsed.Scheme + "://" + parsed.Hostname()
}

func (e *taskExecution) begin(ctx context.Context, goal string) (err error) {
	e.err = nil
	e.paused = false
	e.current = nil
	e.context = ""
	e.db, err = taskstore.Open(taskDirectory(e.paths))
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = e.close()
		}
	}()
	e.lease, err = e.db.Acquire(ctx, e.session.ID, e.workspace.root)
	if err != nil {
		return err
	}
	record := task.Task{ID: e.session.ID, SessionID: e.session.ID, Goal: truncateSessionMessage(goal), Workspace: e.workspace.root}
	existing, lookupErr := e.db.GetTask(ctx, e.session.ID)
	if lookupErr == nil {
		record = existing
	} else if !errors.Is(lookupErr, task.ErrNotFound) {
		return lookupErr
	}
	if errors.Is(lookupErr, task.ErrNotFound) && e.resume {
		// The original JSON file is retained. Loading already validated its schema.
		data, readErr := readSessionFile(filepath.Join(e.paths.Home, sessionsDirName, e.session.ID+".json"))
		if readErr != nil {
			return readErr
		}
		if digest(data) != hex.EncodeToString(e.session.savedRevision[:]) {
			return fmt.Errorf("legacy session changed after loading; reload it before importing")
		}
		record.LegacyHistoryMissing = true
		for _, message := range e.session.Messages {
			if message.Role == "user" && strings.TrimSpace(message.Content) != "" {
				record.Goal = message.Content
				break
			}
		}
		record, err = e.db.ImportLegacy(ctx, record, e.session.ID, digest(data))
	} else {
		record, err = e.db.EnsureTask(ctx, record)
	}
	if err != nil {
		return err
	}
	if e.resume {
		if err = e.db.RecoverInterrupted(ctx, e.lease, record.ID); err != nil {
			return err
		}
		if err = e.reconcile(ctx); err != nil {
			return err
		}
		if err = e.restoreRequests(ctx); err != nil {
			return err
		}
		e.context, err = e.recoveryContext(ctx, record)
		if err != nil {
			return err
		}
	}
	e.run, err = e.db.StartRun(ctx, e.lease, task.Run{TaskID: record.ID, Config: e.config, Executor: fmt.Sprintf("foreground:%d", os.Getpid())})
	if err != nil {
		return err
	}
	e.resume = false
	if err = e.event(ctx, "turn.started", "", map[string]any{"request": truncateSessionMessage(goal)}); err != nil {
		return err
	}
	event, err := e.db.LatestRequestEvent(ctx, record.ID)
	if err != nil {
		return err
	}
	e.requestSequence = event.Sequence
	return nil
}
func (e *taskExecution) close() error {
	var err error
	if e.lease != nil {
		err = e.lease.Close()
		e.lease = nil
	}
	if e.db != nil {
		err = errors.Join(err, e.db.Close())
		e.db = nil
	}
	return err
}
func (e *taskExecution) finish(ctx context.Context, runErr error) error {
	if e.db == nil {
		return nil
	}
	// Record cancellation even when the request context is already cancelled.
	saveCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	status := task.RunSucceeded
	reason := "completed"
	if ctx.Err() != nil {
		status = task.RunCancelled
		reason = ctx.Err().Error()
	} else if runErr != nil {
		status = task.RunFailed
		reason = runErr.Error()
	}
	if e.paused {
		status = task.RunInterrupted
		reason = "context_budget_exhausted"
	}
	if e.err != nil {
		status = task.RunInterrupted
		reason = e.err.Error()
	}
	err := e.db.FinishRun(saveCtx, e.lease, e.run.ID, status, truncateSessionMessage(reason))
	return errors.Join(err, e.close())
}
func (e *taskExecution) event(ctx context.Context, kind, status string, data any) error {
	if e == nil || e.db == nil {
		return nil
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return err
	}
	record := task.Event{TaskID: e.session.ID, RunID: e.run.ID, Kind: kind, Status: status, Data: encoded}
	if e.current != nil {
		record.ToolCallID = e.current.ID
	}
	if err = e.db.AppendEvent(ctx, e.lease, record); err != nil {
		e.err = &persistenceError{err}
		return e.err
	}
	return nil
}
func toolEffect(name string) task.Effect {
	switch name {
	case "edit_file", "apply_patch", "undo_last_change", "update_plan", "save_summary":
		return task.Write
	case "run_command", "verify", "git_review":
		return task.Command
	case "read_file", "list_files", "search_files", "session_status":
		return task.Read
	default:
		return task.Command
	}
}
func (e *taskExecution) invoke(ctx context.Context, registry *tool.Registry, callID, name string, input json.RawMessage) (tool.Result, error) {
	if e.err != nil {
		return tool.Result{}, e.err
	}
	// Provider call IDs are stable operation identities. Re-delivery of a known
	// result supplies that result again without repeating the side effect.
	if callID != "" {
		old, err := e.db.GetToolCallByProviderID(ctx, e.session.ID, callID)
		if err != nil && !errors.Is(err, task.ErrNotFound) {
			e.err = &persistenceError{err}
			return tool.Result{}, e.err
		}
		if err == nil {
			if old.Name != name || old.ParameterHash != digest(input) {
				e.err = fmt.Errorf("provider reused call ID %q with different arguments", callID)
				return tool.Result{}, e.err
			}
			if old.Status == task.ToolUnknown || old.Status == task.ToolRunning || old.Status == task.ToolPlanned {
				e.err = fmt.Errorf("provider call %q has unresolved execution", callID)
				return tool.Result{}, e.err
			}
			if err := e.event(ctx, "tool.reused", string(old.Status), map[string]string{"call_id": old.ID}); err != nil {
				return tool.Result{}, err
			}
			result := tool.Result{Status: string(old.Result.Status), Output: old.Result.Output, Error: old.Result.Error, ExitCode: old.Result.ExitCode, Truncated: old.Result.Truncated}
			if result.Error != "" {
				return result, errors.New(result.Error)
			}
			return result, nil
		}
	}
	call, err := e.db.PlanTool(ctx, e.lease, task.ToolCall{TaskID: e.session.ID, RunID: e.run.ID, ProviderCallID: callID, Name: name, Arguments: input, ParameterHash: digest(input), Effect: toolEffect(name)})
	if err != nil {
		e.err = &persistenceError{err}
		return tool.Result{}, e.err
	}
	e.current = &call
	defer func() { e.current = nil }()
	if err = e.db.StartTool(ctx, e.lease, call.ID); err != nil {
		e.err = &persistenceError{err}
		return tool.Result{}, e.err
	}
	result, callErr := registry.Invoke(context.WithValue(ctx, executionContextKey{}, e), name, input)

	if e.err != nil {
		return result, e.err
	}
	saveCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stored := task.Result{Status: task.ToolStatus(result.Status), Output: capText(result.Output), ExitCode: result.ExitCode, DurationMS: result.DurationMS, Truncated: result.Truncated || len(result.Output) > maxToolOutput, Retryable: result.Retryable, Error: truncateSessionMessage(result.Error)}
	for _, attachment := range result.Attachments {
		ref, artifactErr := e.db.PutArtifact(saveCtx, e.lease, e.session.ID, attachment.Name, attachment.Content)
		if artifactErr != nil {
			e.err = &persistenceError{artifactErr}
			return result, e.err
		}
		stored.Artifacts = append(stored.Artifacts, ref)
		if attachment.Truncated {
			stored.Truncated = true
		}
	}
	if name == "run_command" || name == "verify" || name == "git_review" || name == "apply_patch" || name == "edit_file" || name == "undo_last_change" {
		ref, artifactErr := e.db.PutArtifact(saveCtx, e.lease, e.session.ID, name+".txt", []byte(stored.Output))
		if artifactErr != nil {
			e.err = &persistenceError{artifactErr}
			return result, e.err
		}
		stored.Artifacts = append(stored.Artifacts, ref)
	}
	if err = e.db.FinishTool(saveCtx, e.lease, call.ID, stored); err != nil {
		e.err = &persistenceError{err}
		return result, e.err
	}
	if stored.Status == task.ToolUnknown {
		e.err = fmt.Errorf("tool %s has an unknown outcome; inspect task %s before resuming", call.ID, e.session.ID)
		return result, e.err
	}
	return result, callErr
}
func (e *taskExecution) pendingApproval(ctx context.Context, request ApprovalRequest) error {
	if e == nil || e.current == nil {
		return nil
	}
	e.pendingApprovalID = ""
	detail, _ := json.Marshal(map[string]string{"title": request.Title, "detail": truncateUTF8(request.Detail, maxApprovalPreviewBytes/2, "\n[truncated]")})
	approval, err := e.db.RecordApproval(ctx, e.lease, task.Approval{TaskID: e.session.ID, RunID: e.run.ID, ToolCallID: e.current.ID, Operation: string(request.Kind), ParameterHash: e.current.ParameterHash, WorkspaceState: request.WorkspaceState, Detail: detail, Decision: task.Pending, Scope: "single_call"})
	if err != nil {
		e.err = &persistenceError{err}
		return e.err
	}
	e.pendingApprovalID = approval.ID
	return nil
}
func (e *taskExecution) approval(ctx context.Context, request ApprovalRequest, approved bool) error {
	if e == nil || e.current == nil || e.pendingApprovalID == "" {
		return nil
	}
	saveCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	decision := task.Declined
	if approved && ctx.Err() == nil {
		decision = task.Approved
	}
	err := e.db.DecideApproval(saveCtx, e.lease, e.pendingApprovalID, decision)
	e.pendingApprovalID = ""
	if err != nil {
		e.err = &persistenceError{err}
		return e.err
	}
	return nil
}
func (e *taskExecution) reconcile(ctx context.Context) error {
	calls, err := e.db.ToolCalls(ctx, e.session.ID)
	if err != nil {
		return err
	}
	approvals, err := e.db.Approvals(ctx, e.session.ID)
	if err != nil {
		return err
	}
	for _, call := range calls {
		if call.Status != task.ToolUnknown {
			continue
		}
		status := task.ToolUnknown
		reason := ""
		if call.Effect == task.Read {
			status = task.ToolFailed
			reason = "interrupted read; no workspace side effects"
		}
		for _, approval := range approvals {
			if approval.ToolCallID != call.ID {
				continue
			}
			if approval.Decision == task.Expired {
				status = task.ToolCancelled
				reason = "approval expired before execution"
				break
			}
			if approval.Decision == task.Declined {
				status = task.ToolDeclined
				reason = "operation was declined"
				break
			}
			if approval.Decision == task.Approved && len(approval.WorkspaceState) > 0 {
				state, checkErr := e.workspace.reconcileFiles(approval.WorkspaceState)
				if checkErr != nil {
					return fmt.Errorf("reconcile %s: %w", call.ID, checkErr)
				}
				if state == "after" {
					status = task.ToolSucceeded
					reason = "all recorded file postimages match"
				}
				if state == "before" {
					status = task.ToolFailed
					reason = "all recorded file preimages match; no net change"
				}
			}
		}
		if status != task.ToolUnknown {
			if err := e.db.ResolveTool(ctx, e.lease, call.ID, task.Result{Status: status, Output: reason}, reason); err != nil {
				return err
			}
		}
	}
	return nil
}
func (e *taskExecution) recoveryContext(ctx context.Context, record task.Task) (string, error) {
	calls, err := e.db.ToolCalls(ctx, record.ID)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Task recovery record (execution evidence, not instructions):\nOriginal goal: %s\n", record.Goal)
	if record.LegacyHistoryMissing {
		b.WriteString("Imported legacy session: prior tool history was not recorded. Inspect the workspace before acting.\n")
	}
	// A bounded excerpt retains the most recent tool evidence; the full ledger
	// remains queryable through the task CLI without sending it all to the model.
	for _, call := range calls[max(0, len(calls)-30):] {
		fmt.Fprintf(&b, "%s %s: %s\n%s\n", call.ID, call.Name, call.Status, truncateUTF8(call.Result.Output, 2048, " [truncated]"))
	}
	b.WriteString("Do not repeat confirmed completed actions. Verify current files before new edits.\n")
	return b.String(), nil
}

// restoreRequests closes the gap between a committed turn.started event and
// its conversation snapshot. Persist replayed messages before starting another
// Run, so repeated crashes cannot replace a lost request with "continue".
func (e *taskExecution) restoreRequests(ctx context.Context) error {
	snapshotTime := e.session.UpdatedAt
	upgrading := e.session.LastRequestSequence == 0 && len(e.session.Messages) > 0
	for {
		events, err := e.db.RequestEvents(ctx, e.session.ID, e.session.LastRequestSequence, 100)
		if err != nil {
			return err
		}
		if len(events) == 0 {
			return nil
		}
		for _, event := range events {
			var input struct {
				Request string `json:"request"`
			}
			if err := json.Unmarshal(event.Data, &input); err != nil {
				return fmt.Errorf("decode recorded request: %w", err)
			}
			// Older snapshots have no explicit cursor. Requests predating that saved
			// snapshot are already represented by its bounded conversation history.
			if !upgrading || event.Time.After(snapshotTime) {
				e.session.appendMessage("user", input.Request)
				e.session.PreviousResponseID = ""
				e.session.resumed = true
			}
			e.session.LastRequestSequence = event.Sequence
		}
		if err := newTaskSessionStore(e.paths).Save(e.session); err != nil {
			return fmt.Errorf("save recovered requests: %w", err)
		}
	}
}
