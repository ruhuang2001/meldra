package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"meldra/internal/task"
)

func (s *Store) CreateProcess(ctx context.Context, l *Lease, p task.Process) (task.Process, error) {
	if p.ID == "" {
		p.ID = NewID()
	}
	if !validID(p.ID) || !validID(p.TaskID) || !validID(p.RunID) || !validID(p.ToolCallID) || !validID(p.OwnerEpoch) || !validDigest(p.LaunchHash) {
		return p, errors.New("process requires valid identities and launch hash")
	}
	if len(p.Executable) > 4096 || len(p.Directory) > 4096 || len(p.Arguments) > 256 || p.TimeoutSeconds < 1 || p.TimeoutSeconds > 86400 {
		return p, task.ErrLimit
	}
	for _, arg := range p.Arguments {
		if len(arg) > MaxArgumentsBytes {
			return p, task.ErrLimit
		}
	}
	p.State, p.Effects, p.StartedAt = task.ProcessStarting, "pending", now()
	p.PID, p.ExitCode, p.EndedAt = 0, nil, task.Process{}.EndedAt
	err := s.owned(ctx, l, p.TaskID, func(tx *sql.Tx) error {
		if _, err := activeRun(ctx, tx, p.RunID, p.TaskID); err != nil {
			return err
		}
		call, err := getRecord[task.ToolCall](ctx, tx, "SELECT record FROM tool_calls WHERE id=?", p.ToolCallID)
		if err != nil {
			return err
		}
		if call.RunID != p.RunID || call.TaskID != p.TaskID || call.Status != task.ToolRunning {
			return task.ErrTransition
		}
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM processes WHERE run_id=?", p.RunID).Scan(&count); err != nil {
			return err
		}
		if count >= 64 {
			return task.ErrLimit
		}
		raw, err := encode(p)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO processes(id,task_id,run_id,call_id,state,effects,record) VALUES(?,?,?,?,?,?,?)", p.ID, p.TaskID, p.RunID, p.ToolCallID, p.State, p.Effects, raw); err != nil {
			return err
		}
		return s.appendEvent(ctx, tx, task.Event{TaskID: p.TaskID, RunID: p.RunID, ToolCallID: p.ToolCallID, Kind: "process.planned", Status: string(p.State)})
	})
	return p, err
}

// SaveProcess records only lifecycle evidence from the current owner. It cannot
// change the immutable launch specification or revive a completed process.
func (s *Store) SaveProcess(ctx context.Context, l *Lease, p task.Process) error {
	if l == nil {
		return task.ErrLease
	}
	if len(p.Error) > maxTextBytes || len(p.Signal) > 128 || len(p.Artifacts) > 4 || p.OutputStart < 0 || p.OutputEnd < p.OutputStart {
		return task.ErrLimit
	}
	if p.State != task.ProcessRunning && !p.State.Terminal() {
		return task.ErrTransition
	}
	if p.Effects != "pending" && p.Effects != "known" && p.Effects != "unknown" {
		return task.ErrTransition
	}
	if p.State == task.ProcessRunning && (p.Effects != "pending" || p.PID <= 0 || !p.EndedAt.IsZero()) || p.State.Terminal() && (p.Effects == "pending" || p.EndedAt.IsZero()) || p.State == task.ProcessUnknown && p.Effects != "unknown" || p.State == task.ProcessExited && p.ExitCode == nil {
		return task.ErrTransition
	}
	if err := validateResult(task.Result{Artifacts: p.Artifacts}); err != nil {
		return err
	}
	return s.owned(ctx, l, l.taskID, func(tx *sql.Tx) error {
		old, err := getRecord[task.Process](ctx, tx, "SELECT record FROM processes WHERE id=?", p.ID)
		if err != nil {
			return err
		}
		if old.TaskID != l.taskID || old.RunID != p.RunID || old.ToolCallID != p.ToolCallID || old.OwnerEpoch != p.OwnerEpoch || old.LaunchHash != p.LaunchHash {
			return task.ErrLease
		}
		if _, err := activeRun(ctx, tx, p.RunID, p.TaskID); err != nil {
			return err
		}
		if old.State.Terminal() {
			return task.ErrTransition
		}
		old.State, old.Effects, old.PID = p.State, p.Effects, p.PID
		old.EndedAt, old.ExitCode, old.Signal, old.Error = p.EndedAt, p.ExitCode, p.Signal, p.Error
		old.OutputStart, old.OutputEnd, old.Truncated, old.Artifacts = p.OutputStart, p.OutputEnd, p.Truncated, p.Artifacts
		if err := saveProcess(ctx, tx, old); err != nil {
			return err
		}
		return s.appendEvent(ctx, tx, task.Event{TaskID: old.TaskID, RunID: old.RunID, ToolCallID: old.ToolCallID, Kind: "process.updated", Status: string(old.State)})
	})
}

func saveProcess(ctx context.Context, tx *sql.Tx, p task.Process) error {
	raw, err := encode(p)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE processes SET state=?,effects=?,record=? WHERE id=?", p.State, p.Effects, raw, p.ID)
	return err
}

func (s *Store) Processes(ctx context.Context, taskID string) ([]task.Process, error) {
	return queryRecords[task.Process](ctx, s.db, "SELECT record FROM processes WHERE task_id=? ORDER BY rowid DESC LIMIT 1000", taskID)
}

// LatestProcesses limits record decoding for model recovery independently of
// the task's full process history. Historical handles are evidence, not owners.
func (s *Store) LatestProcesses(ctx context.Context, taskID string) ([]task.Process, error) {
	return queryRecords[task.Process](ctx, s.db, "SELECT record FROM processes WHERE task_id=? ORDER BY rowid DESC LIMIT 10", taskID)
}
func (s *Store) UnknownProcesses(ctx context.Context, taskID string) ([]task.Process, error) {
	return queryRecords[task.Process](ctx, s.db, "SELECT record FROM processes WHERE task_id=? AND effects='unknown' ORDER BY id LIMIT 1000", taskID)
}
func (s *Store) GetProcess(ctx context.Context, id string) (task.Process, error) {
	return getRecord[task.Process](ctx, s.db, "SELECT record FROM processes WHERE id=?", id)
}

func (s *Store) ResolveProcess(ctx context.Context, l *Lease, id, reason string) error {
	if l == nil {
		return task.ErrLease
	}
	if strings.TrimSpace(reason) == "" {
		return errors.New("process reconciliation requires a reason")
	}
	if err := validateText(reason, maxTextBytes); err != nil {
		return err
	}
	return s.owned(ctx, l, l.taskID, func(tx *sql.Tx) error {
		p, err := getRecord[task.Process](ctx, tx, "SELECT record FROM processes WHERE id=?", id)
		if err != nil {
			return err
		}
		if p.TaskID != l.taskID {
			return task.ErrLease
		}
		if p.Effects != "unknown" || !p.State.Terminal() {
			return task.ErrTransition
		}
		p.Effects = "resolved"
		p.ResolutionReason, p.ResolvedAt = reason, now()
		if err := saveProcess(ctx, tx, p); err != nil {
			return err
		}
		return s.appendEvent(ctx, tx, task.Event{TaskID: p.TaskID, RunID: p.RunID, ToolCallID: p.ToolCallID, Kind: "process.resolved", Status: string(p.State), Reason: reason})
	})
}

func (s *Store) interruptProcesses(ctx context.Context, tx *sql.Tx, runID string) error {
	processes, err := queryRecords[task.Process](ctx, tx, "SELECT record FROM processes WHERE run_id=? AND state IN ('starting','running')", runID)
	if err != nil {
		return err
	}
	for _, p := range processes {
		p.State, p.Effects, p.EndedAt = task.ProcessUnknown, "unknown", now()
		p.Error = "Execution owner lost; process and external effects were not observed. Historical PID is not safe to signal."
		if err := saveProcess(ctx, tx, p); err != nil {
			return err
		}
		if err := s.appendEvent(ctx, tx, task.Event{TaskID: p.TaskID, RunID: p.RunID, ToolCallID: p.ToolCallID, Kind: "process.interrupted", Status: string(p.State)}); err != nil {
			return err
		}
	}
	return nil
}

func requireSettledProcesses(ctx context.Context, tx *sql.Tx, runID string) error {
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM processes WHERE run_id=? AND (state IN ('starting','running') OR effects='unknown')", runID).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return fmt.Errorf("%w: managed processes require cleanup or reconciliation", task.ErrUnresolved)
	}
	return nil
}
