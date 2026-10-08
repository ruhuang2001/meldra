package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"meldra/internal/task"
)

func validateTask(t task.Task) error {
	if !validID(t.ID) || !validID(t.SessionID) {
		return errors.New("task and session IDs are required")
	}
	if t.Goal == "" {
		return errors.New("task goal is required")
	}
	return validateText(t.Goal, maxTextBytes)
}

func (s *Store) ensureTask(ctx context.Context, tx *sql.Tx, t task.Task) (task.Task, error) {
	existing, err := getRecord[task.Task](ctx, tx, "SELECT record FROM tasks WHERE id=?", t.ID)
	if err == nil {
		if existing.Workspace != t.Workspace || existing.SessionID != t.SessionID {
			return t, errors.New("task identity or workspace differs from existing record")
		}
		return existing, nil
	}
	if !errors.Is(err, task.ErrNotFound) {
		return t, err
	}
	t.Status = task.Queued
	t.CreatedAt = now()
	t.UpdatedAt = t.CreatedAt
	raw, err := encode(t)
	if err != nil {
		return t, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO tasks(id,workspace,status,updated,record) VALUES(?,?,?,?,?)", t.ID, t.Workspace, t.Status, timestamp(t.UpdatedAt), raw); err != nil {
		return t, err
	}
	err = s.appendEvent(ctx, tx, task.Event{TaskID: t.ID, Kind: "task.created", Status: string(t.Status)})
	return t, err
}

func (s *Store) EnsureTask(ctx context.Context, t task.Task) (task.Task, error) {
	if t.SessionID == "" {
		t.SessionID = t.ID
	}
	if err := validateTask(t); err != nil {
		return t, err
	}
	workspace, err := CanonicalWorkspace(t.Workspace)
	if err != nil {
		return t, err
	}
	t.Workspace = workspace
	err = s.transact(ctx, func(tx *sql.Tx) error { var err error; t, err = s.ensureTask(ctx, tx, t); return err })
	return t, err
}

func (s *Store) GetTask(ctx context.Context, id string) (task.Task, error) {
	return getRecord[task.Task](ctx, s.db, "SELECT record FROM tasks WHERE id=?", id)
}
func (s *Store) GetRun(ctx context.Context, id string) (task.Run, error) {
	return getRecord[task.Run](ctx, s.db, "SELECT record FROM runs WHERE id=?", id)
}
func (s *Store) GetToolCall(ctx context.Context, id string) (task.ToolCall, error) {
	return getRecord[task.ToolCall](ctx, s.db, "SELECT record FROM tool_calls WHERE id=?", id)
}

const providerLookupSQL = `SELECT record FROM tool_calls WHERE task_id=? AND json_extract(CAST(record AS TEXT),'$.provider_call_id')=? ORDER BY planned,id LIMIT 1`

// GetToolCallByProviderID looks up a replay identity within one task. It loads
// at most one bounded record even when a long task has thousands of calls.
// Empty provider IDs are not identities and never match another empty ID.
func (s *Store) GetToolCallByProviderID(ctx context.Context, taskID, providerID string) (task.ToolCall, error) {
	if !validID(taskID) || providerID == "" {
		return task.ToolCall{}, task.ErrNotFound
	}
	if len(providerID) > 1024 {
		return task.ToolCall{}, task.ErrLimit
	}
	return getRecord[task.ToolCall](ctx, s.db, providerLookupSQL, taskID, providerID)
}

func (s *Store) ListTasks(ctx context.Context, workspace string, limit int) ([]task.Task, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	if workspace != "" {
		canonical, err := CanonicalWorkspace(workspace)
		if err != nil {
			return nil, err
		}
		return queryRecords[task.Task](ctx, s.db, "SELECT record FROM tasks WHERE workspace=? ORDER BY updated DESC,id LIMIT ?", canonical, limit)
	}
	return queryRecords[task.Task](ctx, s.db, "SELECT record FROM tasks ORDER BY updated DESC,id LIMIT ?", limit)
}

func (s *Store) Runs(ctx context.Context, id string) ([]task.Run, error) {
	return queryRecords[task.Run](ctx, s.db, "SELECT record FROM runs WHERE task_id=? ORDER BY started,id", id)
}
func (s *Store) ToolCalls(ctx context.Context, id string) ([]task.ToolCall, error) {
	return queryRecords[task.ToolCall](ctx, s.db, "SELECT record FROM tool_calls WHERE task_id=? ORDER BY planned,id", id)
}
func (s *Store) Approvals(ctx context.Context, id string) ([]task.Approval, error) {
	return queryRecords[task.Approval](ctx, s.db, "SELECT record FROM approvals WHERE task_id=? ORDER BY rowid", id)
}
func (s *Store) Events(ctx context.Context, id string, after int64, limit int) ([]task.Event, error) {
	if after < 0 {
		return nil, errors.New("event cursor must be nonnegative")
	}
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	return queryRecords[task.Event](ctx, s.db, "SELECT record FROM events WHERE task_id=? AND seq>? ORDER BY seq LIMIT ?", id, after, limit)
}

// RequestEvents pages only durable user requests, avoiding full tool history
// payloads while rebuilding a snapshot that lagged behind the execution log.
func (s *Store) RequestEvents(ctx context.Context, id string, after int64, limit int) ([]task.Event, error) {
	if after < 0 {
		return nil, errors.New("request cursor must be nonnegative")
	}
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	return queryRecords[task.Event](ctx, s.db, `SELECT record FROM events WHERE task_id=? AND seq>? AND json_extract(CAST(record AS TEXT),'$.kind')='turn.started' ORDER BY seq LIMIT ?`, id, after, limit)
}

func (s *Store) LatestRequestEvent(ctx context.Context, id string) (task.Event, error) {
	return getRecord[task.Event](ctx, s.db, `SELECT record FROM events WHERE task_id=? AND json_extract(CAST(record AS TEXT),'$.kind')='turn.started' ORDER BY seq DESC LIMIT 1`, id)
}

type rowsQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func queryRecords[T any](ctx context.Context, q rowsQueryer, query string, args ...any) ([]T, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []T{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var item T
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, fmt.Errorf("corrupt task record: %w", err)
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func saveTask(ctx context.Context, tx *sql.Tx, t task.Task) error {
	t.UpdatedAt = now()
	raw, err := encode(t)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE tasks SET status=?,updated=?,record=? WHERE id=?", t.Status, timestamp(t.UpdatedAt), raw, t.ID)
	return err
}

func saveRun(ctx context.Context, tx *sql.Tx, r task.Run) error {
	raw, err := encode(r)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE runs SET status=?,record=? WHERE id=?", r.Status, raw, r.ID)
	return err
}

func (s *Store) StartRun(ctx context.Context, l *Lease, r task.Run) (task.Run, error) {
	if r.ID == "" {
		r.ID = NewID()
	}
	if !validID(r.ID) || !validID(r.TaskID) {
		return r, errors.New("invalid run or task ID")
	}
	if len(r.Config.Model) > 4096 || !cleanProvider(r.Config.Provider) || len(r.Executor) > 4096 {
		return r, task.ErrLimit
	}
	err := s.owned(ctx, l, r.TaskID, func(tx *sql.Tx) error {
		var unresolved, active int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM tool_calls WHERE task_id=? AND status='unknown'", r.TaskID).Scan(&unresolved); err != nil {
			return err
		}
		if unresolved > 0 {
			return task.ErrUnresolved
		}
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM runs WHERE task_id=? AND status IN ('running','waiting_approval')", r.TaskID).Scan(&active); err != nil {
			return err
		}
		if active > 0 {
			return fmt.Errorf("%w: interrupted run requires explicit recovery", task.ErrBusy)
		}
		r.Config.Workspace = l.workspace
		r.Status = task.RunRunning
		r.StartedAt = now()
		r.EndedAt = task.Run{}.EndedAt
		r.Reason = ""
		raw, err := encode(r)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO runs(id,task_id,status,started,record) VALUES(?,?,?,?,?)", r.ID, r.TaskID, r.Status, timestamp(r.StartedAt), raw); err != nil {
			return err
		}
		t, err := getRecord[task.Task](ctx, tx, "SELECT record FROM tasks WHERE id=?", r.TaskID)
		if err != nil {
			return err
		}
		t.Status = task.Running
		if err := saveTask(ctx, tx, t); err != nil {
			return err
		}
		return s.appendEvent(ctx, tx, task.Event{TaskID: r.TaskID, RunID: r.ID, Kind: "run.started", Status: string(r.Status)})
	})
	return r, err
}

func (s *Store) FinishRun(ctx context.Context, l *Lease, id string, status task.RunStatus, reason string) error {
	if !status.Terminal() {
		return task.ErrTransition
	}
	if err := validateText(reason, maxTextBytes); err != nil {
		return err
	}
	if l == nil {
		return task.ErrLease
	}
	return s.owned(ctx, l, l.taskID, func(tx *sql.Tx) error {
		r, err := getRecord[task.Run](ctx, tx, "SELECT record FROM runs WHERE id=?", id)
		if err != nil {
			return err
		}
		if r.TaskID != l.taskID {
			return task.ErrLease
		}
		return s.finishRun(ctx, tx, r, status, reason)
	})
}

func (s *Store) finishRun(ctx context.Context, tx *sql.Tx, r task.Run, status task.RunStatus, reason string) error {
	if err := task.RunTransition(r.Status, status); err != nil {
		return err
	}
	if status == task.RunSucceeded {
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM tool_calls WHERE run_id=? AND status IN ('planned','running','unknown')", r.ID).Scan(&count); err != nil {
			return err
		}
		if count > 0 {
			return task.ErrUnresolved
		}
	} else if err := s.interruptCalls(ctx, tx, r); err != nil {
		return err
	}
	if err := s.expireApprovals(ctx, tx, r); err != nil {
		return err
	}
	r.Status = status
	r.Reason = reason
	r.EndedAt = now()
	if err := saveRun(ctx, tx, r); err != nil {
		return err
	}
	t, err := getRecord[task.Task](ctx, tx, "SELECT record FROM tasks WHERE id=?", r.TaskID)
	if err != nil {
		return err
	}
	t.Status = task.StatusForRun(status)
	if err := saveTask(ctx, tx, t); err != nil {
		return err
	}
	return s.appendEvent(ctx, tx, task.Event{TaskID: r.TaskID, RunID: r.ID, Kind: "run.finished", Status: string(status), Reason: reason})
}

// RecoverInterrupted requires explicit user recovery and newly acquired
// ownership. In-flight side effects become unknown, never inferred successful.
func (s *Store) RecoverInterrupted(ctx context.Context, l *Lease, id string) error {
	return s.owned(ctx, l, id, func(tx *sql.Tx) error {
		runs, err := queryRecords[task.Run](ctx, tx, "SELECT record FROM runs WHERE task_id=? AND status IN ('running','waiting_approval') ORDER BY started", id)
		if err != nil {
			return err
		}
		for _, r := range runs {
			if err := s.finishRun(ctx, tx, r, task.RunInterrupted, "execution_owner_lost"); err != nil {
				return err
			}
		}
		return nil
	})
}

// ImportLegacy deduplicates each source ID and content hash atomically. The
// caller retains and validates the original JSON; no historical tool success
// is inferred from a conversation transcript.
func (s *Store) ImportLegacy(ctx context.Context, t task.Task, sourceID, contentHash string) (task.Task, error) {
	if t.SessionID == "" {
		t.SessionID = t.ID
	}
	if err := validateTask(t); err != nil {
		return t, err
	}
	if len(sourceID) == 0 || len(sourceID) > 4096 || len(contentHash) != 64 {
		return t, errors.New("legacy import requires source ID and SHA-256")
	}
	workspace, err := CanonicalWorkspace(t.Workspace)
	if err != nil {
		return t, err
	}
	t.Workspace = workspace
	t.LegacyHistoryMissing = true
	err = s.transact(ctx, func(tx *sql.Tx) error {
		var existing string
		err := tx.QueryRowContext(ctx, "SELECT task_id FROM legacy_imports WHERE source_id=? AND content_hash=?", sourceID, contentHash).Scan(&existing)
		if err == nil {
			t, err = getRecord[task.Task](ctx, tx, "SELECT record FROM tasks WHERE id=?", existing)
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		t, err = s.ensureTask(ctx, tx, t)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO legacy_imports(source_id,content_hash,task_id) VALUES(?,?,?)", sourceID, contentHash, t.ID); err != nil {
			return err
		}
		data, _ := json.Marshal(map[string]string{"source_id": sourceID, "content_hash": contentHash, "history": "tool records unavailable"})
		return s.appendEvent(ctx, tx, task.Event{TaskID: t.ID, Kind: "task.imported", Data: data})
	})
	return t, err
}
