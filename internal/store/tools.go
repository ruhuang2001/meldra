package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"meldra/internal/task"
)

func activeRun(ctx context.Context, tx *sql.Tx, id, taskID string) (task.Run, error) {
	r, err := getRecord[task.Run](ctx, tx, "SELECT record FROM runs WHERE id=?", id)
	if err != nil {
		return r, err
	}
	if r.TaskID != taskID {
		return r, task.ErrLease
	}
	if r.Status != task.RunRunning && r.Status != task.RunWaitingApproval {
		return r, task.ErrTransition
	}
	return r, nil
}

func (s *Store) PlanTool(ctx context.Context, l *Lease, c task.ToolCall) (task.ToolCall, error) {
	if c.ID == "" {
		c.ID = NewID()
	}
	if !validID(c.ID) || !validID(c.TaskID) || !validID(c.RunID) || c.Name == "" {
		return c, errors.New("tool call requires valid IDs and name")
	}
	if len(c.Name) > 256 || len(c.ProviderCallID) > 1024 || len(c.ReplayScope) > 128 {
		return c, task.ErrLimit
	}
	if c.Effect != task.Read && c.Effect != task.Write && c.Effect != task.Command {
		return c, errors.New("tool call requires a known effect")
	}
	if len(c.Arguments) == 0 {
		c.Arguments = json.RawMessage("{}")
	}
	if err := validateJSON(c.Arguments, MaxArgumentsBytes); err != nil {
		return c, err
	}
	c.ParameterHash = Hash(c.Arguments)
	c.Status = task.ToolPlanned
	c.PlannedAt = now()
	c.StartedAt = task.ToolCall{}.StartedAt
	c.EndedAt = task.ToolCall{}.EndedAt
	c.Result = task.Result{Status: task.ToolPlanned}
	err := s.owned(ctx, l, c.TaskID, func(tx *sql.Tx) error {
		r, err := activeRun(ctx, tx, c.RunID, c.TaskID)
		if err != nil {
			return err
		}
		if r.Status != task.RunRunning {
			return task.ErrTransition
		}
		raw, err := encode(c)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO tool_calls(id,task_id,run_id,status,planned,record) VALUES(?,?,?,?,?,?)", c.ID, c.TaskID, c.RunID, c.Status, timestamp(c.PlannedAt), raw); err != nil {
			return err
		}
		return s.appendEvent(ctx, tx, task.Event{TaskID: c.TaskID, RunID: c.RunID, ToolCallID: c.ID, Kind: "tool.planned", Status: string(c.Status)})
	})
	return c, err
}

func saveTool(ctx context.Context, tx *sql.Tx, c task.ToolCall) error {
	raw, err := encode(c)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE tool_calls SET status=?,record=? WHERE id=?", c.Status, raw, c.ID)
	return err
}

func (s *Store) StartTool(ctx context.Context, l *Lease, id string) error {
	if l == nil {
		return task.ErrLease
	}
	return s.owned(ctx, l, l.taskID, func(tx *sql.Tx) error {
		c, err := getRecord[task.ToolCall](ctx, tx, "SELECT record FROM tool_calls WHERE id=?", id)
		if err != nil {
			return err
		}
		r, err := activeRun(ctx, tx, c.RunID, l.taskID)
		if err != nil {
			return err
		}
		if r.Status != task.RunRunning {
			return task.ErrTransition
		}
		if err := task.ToolTransition(c.Status, task.ToolRunning); err != nil {
			return err
		}
		c.Status = task.ToolRunning
		c.Result.Status = c.Status
		c.StartedAt = now()
		if err := saveTool(ctx, tx, c); err != nil {
			return err
		}
		return s.appendEvent(ctx, tx, task.Event{TaskID: c.TaskID, RunID: c.RunID, ToolCallID: c.ID, Kind: "tool.started", Status: string(c.Status)})
	})
}

func validateResult(result task.Result) error {
	if len(result.Output) > MaxOutputBytes || len(result.Error) > maxTextBytes || len(result.Artifacts) > 128 || result.DurationMS < 0 {
		return task.ErrLimit
	}
	for _, ref := range result.Artifacts {
		if !validDigest(ref.ID) || ref.ID != ref.SHA256 || len(ref.Name) > 256 || ref.Size < 0 || ref.Size > MaxArtifactBytes {
			return task.ErrLimit
		}
	}
	return nil
}

func (s *Store) FinishTool(ctx context.Context, l *Lease, id string, result task.Result) error {
	if err := validateResult(result); err != nil {
		return err
	}
	if l == nil {
		return task.ErrLease
	}
	return s.owned(ctx, l, l.taskID, func(tx *sql.Tx) error {
		c, err := getRecord[task.ToolCall](ctx, tx, "SELECT record FROM tool_calls WHERE id=?", id)
		if err != nil {
			return err
		}
		if _, err := activeRun(ctx, tx, c.RunID, l.taskID); err != nil {
			return err
		}
		if err := task.ToolTransition(c.Status, result.Status); err != nil {
			return err
		}
		return s.finishTool(ctx, tx, c, result, "tool.finished", "")
	})
}

func (s *Store) finishTool(ctx context.Context, tx *sql.Tx, c task.ToolCall, result task.Result, kind, reason string) error {
	c.Status = result.Status
	c.Result = result
	c.EndedAt = now()
	if err := saveTool(ctx, tx, c); err != nil {
		return err
	}
	return s.appendEvent(ctx, tx, task.Event{TaskID: c.TaskID, RunID: c.RunID, ToolCallID: c.ID, Kind: kind, Status: string(c.Status), Reason: reason})
}

// ResolveTool records an explicit reconciliation decision without running the
// tool again. An old Run remains interrupted/failed/cancelled permanently.
func (s *Store) ResolveTool(ctx context.Context, l *Lease, id string, result task.Result, reason string) error {
	if result.Status != task.ToolSucceeded && result.Status != task.ToolFailed && result.Status != task.ToolDeclined && result.Status != task.ToolCancelled || reason == "" {
		return errors.New("reconciliation requires a terminal outcome and an evidence reason")
	}
	if err := validateResult(result); err != nil {
		return err
	}
	if err := validateText(reason, maxTextBytes); err != nil {
		return err
	}
	if l == nil {
		return task.ErrLease
	}
	return s.owned(ctx, l, l.taskID, func(tx *sql.Tx) error {
		c, err := getRecord[task.ToolCall](ctx, tx, "SELECT record FROM tool_calls WHERE id=?", id)
		if err != nil {
			return err
		}
		if c.TaskID != l.taskID {
			return task.ErrLease
		}
		if c.Status != task.ToolUnknown {
			return task.ErrTransition
		}
		r, err := getRecord[task.Run](ctx, tx, "SELECT record FROM runs WHERE id=?", c.RunID)
		if err != nil {
			return err
		}
		if !r.Status.Terminal() {
			return fmt.Errorf("%w: reconcile after ending the previous run", task.ErrTransition)
		}
		return s.finishTool(ctx, tx, c, result, "tool.reconciled", reason)
	})
}

func (s *Store) interruptCalls(ctx context.Context, tx *sql.Tx, r task.Run) error {
	calls, err := queryRecords[task.ToolCall](ctx, tx, "SELECT record FROM tool_calls WHERE run_id=? AND status IN ('planned','running') ORDER BY planned", r.ID)
	if err != nil {
		return err
	}
	for _, c := range calls {
		status := task.ToolUnknown
		if c.Status == task.ToolPlanned {
			status = task.ToolCancelled
		}
		result := task.Result{Status: status, Error: "execution interrupted before a durable result was recorded"}
		if err := s.finishTool(ctx, tx, c, result, "tool.interrupted", "execution_owner_lost"); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) RecordApproval(ctx context.Context, l *Lease, a task.Approval) (task.Approval, error) {
	if a.ID == "" {
		a.ID = NewID()
	}
	if !validID(a.ID) || !validID(a.TaskID) || !validID(a.RunID) || !validID(a.ToolCallID) || a.Operation == "" {
		return a, errors.New("approval requires IDs and operation")
	}
	if a.Decision == "" {
		a.Decision = task.Pending
	}
	if a.Decision != task.Pending && a.Decision != task.Approved && a.Decision != task.Declined {
		return a, errors.New("invalid approval decision")
	}
	if err := validateJSON(a.WorkspaceState, MaxArgumentsBytes); err != nil {
		return a, err
	}
	if err := validateJSON(a.Detail, MaxArgumentsBytes); err != nil {
		return a, err
	}
	if len(a.Operation) > maxTextBytes {
		return a, task.ErrLimit
	}
	a.Scope = "tool_call"
	a.CreatedAt = now()
	a.DecidedAt = task.Approval{}.DecidedAt
	if a.Decision != task.Pending {
		a.DecidedAt = a.CreatedAt
	}
	err := s.owned(ctx, l, a.TaskID, func(tx *sql.Tx) error {
		if _, err := activeRun(ctx, tx, a.RunID, a.TaskID); err != nil {
			return err
		}
		c, err := getRecord[task.ToolCall](ctx, tx, "SELECT record FROM tool_calls WHERE id=?", a.ToolCallID)
		if err != nil {
			return err
		}
		if c.TaskID != a.TaskID || c.RunID != a.RunID || c.Status != task.ToolPlanned && c.Status != task.ToolRunning {
			return task.ErrTransition
		}
		if a.ParameterHash == "" {
			a.ParameterHash = c.ParameterHash
		}
		if a.ParameterHash != c.ParameterHash {
			return errors.New("approval parameter hash does not match tool intent")
		}
		raw, err := encode(a)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO approvals(id,task_id,run_id,call_id,decision,record) VALUES(?,?,?,?,?,?)", a.ID, a.TaskID, a.RunID, a.ToolCallID, a.Decision, raw); err != nil {
			return err
		}
		if a.Decision == task.Pending {
			if err := s.setWaiting(ctx, tx, a.RunID, true); err != nil {
				return err
			}
		}
		return s.appendEvent(ctx, tx, task.Event{TaskID: a.TaskID, RunID: a.RunID, ToolCallID: a.ToolCallID, Kind: "approval.recorded", Status: string(a.Decision)})
	})
	return a, err
}

func (s *Store) DecideApproval(ctx context.Context, l *Lease, id string, decision task.Decision) error {
	if decision != task.Approved && decision != task.Declined {
		return errors.New("approval decision must be approved or declined")
	}
	if l == nil {
		return task.ErrLease
	}
	return s.owned(ctx, l, l.taskID, func(tx *sql.Tx) error {
		a, err := getRecord[task.Approval](ctx, tx, "SELECT record FROM approvals WHERE id=?", id)
		if err != nil {
			return err
		}
		if _, err := activeRun(ctx, tx, a.RunID, l.taskID); err != nil {
			return err
		}
		if a.Decision != task.Pending {
			return task.ErrTransition
		}
		a.Decision = decision
		a.DecidedAt = now()
		if err := saveApproval(ctx, tx, a); err != nil {
			return err
		}
		var pending int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM approvals WHERE run_id=? AND decision='pending'", a.RunID).Scan(&pending); err != nil {
			return err
		}
		if pending == 0 {
			if err := s.setWaiting(ctx, tx, a.RunID, false); err != nil {
				return err
			}
		}
		return s.appendEvent(ctx, tx, task.Event{TaskID: a.TaskID, RunID: a.RunID, ToolCallID: a.ToolCallID, Kind: "approval.decided", Status: string(decision)})
	})
}

func saveApproval(ctx context.Context, tx *sql.Tx, a task.Approval) error {
	raw, err := encode(a)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE approvals SET decision=?,record=? WHERE id=?", a.Decision, raw, a.ID)
	return err
}

func (s *Store) setWaiting(ctx context.Context, tx *sql.Tx, id string, waiting bool) error {
	r, err := getRecord[task.Run](ctx, tx, "SELECT record FROM runs WHERE id=?", id)
	if err != nil {
		return err
	}
	status := task.RunRunning
	if waiting {
		status = task.RunWaitingApproval
	}
	if r.Status == status {
		return nil
	}
	if err := task.RunTransition(r.Status, status); err != nil {
		return err
	}
	r.Status = status
	if err := saveRun(ctx, tx, r); err != nil {
		return err
	}
	t, err := getRecord[task.Task](ctx, tx, "SELECT record FROM tasks WHERE id=?", r.TaskID)
	if err != nil {
		return err
	}
	t.Status = task.StatusForRun(status)
	return saveTask(ctx, tx, t)
}

func (s *Store) expireApprovals(ctx context.Context, tx *sql.Tx, r task.Run) error {
	approvals, err := queryRecords[task.Approval](ctx, tx, "SELECT record FROM approvals WHERE run_id=? AND decision='pending'", r.ID)
	if err != nil {
		return err
	}
	for _, a := range approvals {
		a.Decision = task.Expired
		a.DecidedAt = now()
		if err := saveApproval(ctx, tx, a); err != nil {
			return err
		}
		if err := s.appendEvent(ctx, tx, task.Event{TaskID: a.TaskID, RunID: a.RunID, ToolCallID: a.ToolCallID, Kind: "approval.expired", Status: string(a.Decision)}); err != nil {
			return err
		}
	}
	return nil
}
