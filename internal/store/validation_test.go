package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"meldra/internal/task"
)

func TestStorageLimitsRejectBeforeMutation(t *testing.T) {
	f := setup(t)
	c := plan(t, f, "run_command", task.Command)
	if err := f.s.StartTool(t.Context(), f.l, c.ID); err != nil {
		t.Fatal(err)
	}
	before, err := f.s.Events(t.Context(), f.task.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for name, op := range map[string]func() error{
		"goal": func() error {
			_, err := f.s.EnsureTask(t.Context(), task.Task{ID: "large", Goal: strings.Repeat("x", maxTextBytes+1), Workspace: f.workspace})
			return err
		},
		"arguments": func() error {
			_, err := f.s.PlanTool(t.Context(), f.l, task.ToolCall{TaskID: f.task.ID, RunID: f.run.ID, Name: "write_file", Effect: task.Write, Arguments: json.RawMessage(`"` + strings.Repeat("x", MaxArgumentsBytes) + `"`)})
			return err
		},
		"output": func() error {
			return f.s.FinishTool(t.Context(), f.l, c.ID, task.Result{Status: task.ToolSucceeded, Output: strings.Repeat("x", MaxOutputBytes+1)})
		},
		"error": func() error {
			return f.s.FinishTool(t.Context(), f.l, c.ID, task.Result{Status: task.ToolFailed, Error: strings.Repeat("x", maxTextBytes+1)})
		},
		"artifact": func() error {
			return f.s.FinishTool(t.Context(), f.l, c.ID, task.Result{Status: task.ToolSucceeded, Artifacts: []task.ArtifactRef{{ID: "../escape", Size: 3}}})
		},
		"event": func() error {
			return f.s.AppendEvent(t.Context(), f.l, task.Event{TaskID: f.task.ID, Kind: "huge", Data: json.RawMessage(`"` + strings.Repeat("x", MaxEventBytes) + `"`)})
		},
		"event_kind": func() error {
			return f.s.AppendEvent(t.Context(), f.l, task.Event{TaskID: f.task.ID, Kind: strings.Repeat("x", 129)})
		},
		"reason": func() error {
			return f.s.FinishRun(t.Context(), f.l, f.run.ID, task.RunFailed, strings.Repeat("x", maxTextBytes+1))
		},
		"config": func() error {
			_, err := f.s.StartRun(t.Context(), f.l, task.Run{TaskID: f.task.ID, Config: task.Config{Model: strings.Repeat("x", 4097)}})
			return err
		},
		"approval_snapshot": func() error {
			_, err := f.s.RecordApproval(t.Context(), f.l, task.Approval{TaskID: f.task.ID, RunID: f.run.ID, ToolCallID: c.ID, Operation: "write", WorkspaceState: json.RawMessage(`"` + strings.Repeat("x", MaxArgumentsBytes) + `"`)})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := op(); !errors.Is(err, task.ErrLimit) {
				t.Fatalf("limit not enforced: %v", err)
			}
		})
	}
	after, err := f.s.Events(t.Context(), f.task.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Fatal("rejected large record appended event")
	}
	got, err := f.s.GetToolCall(t.Context(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.ToolRunning {
		t.Fatal("rejected result changed state")
	}
}

func TestInvalidRecordInputsAndCrossTaskReferences(t *testing.T) {
	f := setup(t)
	c := plan(t, f, "read_file", task.Read)
	for name, op := range map[string]func() error{
		"task_id": func() error {
			_, err := f.s.EnsureTask(t.Context(), task.Task{ID: "../bad", Goal: "x", Workspace: f.workspace})
			return err
		},
		"goal": func() error {
			_, err := f.s.EnsureTask(t.Context(), task.Task{ID: "valid", Workspace: f.workspace})
			return err
		},
		"workspace": func() error {
			_, err := f.s.EnsureTask(t.Context(), task.Task{ID: "valid", Goal: "x", Workspace: filepath.Join(f.workspace, "missing")})
			return err
		},
		"run_id": func() error {
			_, err := f.s.StartRun(t.Context(), f.l, task.Run{ID: "../bad", TaskID: f.task.ID})
			return err
		},
		"active_run":         func() error { _, err := f.s.StartRun(t.Context(), f.l, task.Run{TaskID: f.task.ID}); return err },
		"nonterminal_finish": func() error { return f.s.FinishRun(t.Context(), f.l, f.run.ID, task.RunRunning, "") },
		"missing_tool":       func() error { return f.s.StartTool(t.Context(), f.l, "missing") },
		"bad_tool_json": func() error {
			_, err := f.s.PlanTool(t.Context(), f.l, task.ToolCall{TaskID: f.task.ID, RunID: f.run.ID, Name: "x", Effect: task.Read, Arguments: json.RawMessage(`{broken`)})
			return err
		},
		"bad_tool_effect": func() error {
			_, err := f.s.PlanTool(t.Context(), f.l, task.ToolCall{TaskID: f.task.ID, RunID: f.run.ID, Name: "x", Effect: "unknown"})
			return err
		},
		"missing_tool_name": func() error {
			_, err := f.s.PlanTool(t.Context(), f.l, task.ToolCall{TaskID: f.task.ID, RunID: f.run.ID, Effect: task.Read})
			return err
		},
		"unstarted_success":     func() error { return f.s.FinishTool(t.Context(), f.l, c.ID, task.Result{Status: task.ToolSucceeded}) },
		"unresolved_run_finish": func() error { return f.s.FinishRun(t.Context(), f.l, f.run.ID, task.RunSucceeded, "incorrect") },
		"resolve_without_evidence": func() error {
			return f.s.ResolveTool(t.Context(), f.l, c.ID, task.Result{Status: task.ToolSucceeded}, "")
		},
		"resolve_pending": func() error {
			return f.s.ResolveTool(t.Context(), f.l, c.ID, task.Result{Status: task.ToolRunning}, "wrong")
		},
		"approve_without_operation": func() error {
			_, err := f.s.RecordApproval(t.Context(), f.l, task.Approval{TaskID: f.task.ID, RunID: f.run.ID, ToolCallID: c.ID})
			return err
		},
		"bad_approval_decision": func() error {
			_, err := f.s.RecordApproval(t.Context(), f.l, task.Approval{TaskID: f.task.ID, RunID: f.run.ID, ToolCallID: c.ID, Operation: "read", Decision: task.Expired})
			return err
		},
		"bad_approval_detail": func() error {
			_, err := f.s.RecordApproval(t.Context(), f.l, task.Approval{TaskID: f.task.ID, RunID: f.run.ID, ToolCallID: c.ID, Operation: "read", Detail: json.RawMessage(`broken`)})
			return err
		},
		"bad_approval_transition": func() error { return f.s.DecideApproval(t.Context(), f.l, "missing", task.Pending) },
		"missing_approval":        func() error { return f.s.DecideApproval(t.Context(), f.l, "missing", task.Approved) },
		"negative_cursor":         func() error { _, err := f.s.Events(t.Context(), f.task.ID, -1, 100); return err },
		"event_missing_kind":      func() error { return f.s.AppendEvent(t.Context(), f.l, task.Event{TaskID: f.task.ID}) },
		"bad_event_json": func() error {
			return f.s.AppendEvent(t.Context(), f.l, task.Event{TaskID: f.task.ID, Kind: "x", Data: json.RawMessage(`broken`)})
		},
		"event_missing_run": func() error {
			return f.s.AppendEvent(t.Context(), f.l, task.Event{TaskID: f.task.ID, RunID: "missing", Kind: "x"})
		},
		"event_missing_tool": func() error {
			return f.s.AppendEvent(t.Context(), f.l, task.Event{TaskID: f.task.ID, ToolCallID: "missing", Kind: "x"})
		},
		"legacy_bad_source": func() error {
			_, err := f.s.ImportLegacy(t.Context(), task.Task{ID: "old", Goal: "old", Workspace: f.workspace}, "", "bad")
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := op(); err == nil {
				t.Fatal("invalid operation accepted")
			}
		})
	}
	other, err := f.s.EnsureTask(t.Context(), task.Task{ID: "other", Goal: "other", Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	l, err := f.s.Acquire(t.Context(), other.ID, other.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	r, err := f.s.StartRun(t.Context(), l, task.Run{TaskID: other.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.AppendEvent(t.Context(), f.l, task.Event{TaskID: f.task.ID, RunID: r.ID, Kind: "wrong"}); err == nil {
		t.Fatal("cross-task event accepted")
	}
	if err := f.s.AppendEvent(t.Context(), l, task.Event{TaskID: other.ID, ToolCallID: c.ID, Kind: "wrong"}); err == nil {
		t.Fatal("cross-task tool event accepted")
	}
	if err := f.s.FinishRun(t.Context(), f.l, r.ID, task.RunFailed, "wrong"); !errors.Is(err, task.ErrLease) {
		t.Fatal(err)
	}
	if _, err := f.s.PlanTool(t.Context(), f.l, task.ToolCall{TaskID: f.task.ID, RunID: r.ID, Name: "read", Effect: task.Read}); !errors.Is(err, task.ErrLease) {
		t.Fatal(err)
	}
	if err := f.s.AppendEvent(t.Context(), f.l, task.Event{TaskID: f.task.ID, RunID: f.run.ID, ToolCallID: c.ID, Kind: "model.completed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Events(t.Context(), f.task.ID, 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.ListTasks(t.Context(), "", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.ListTasks(t.Context(), "/definitely/missing/workspace", 1); err == nil {
		t.Fatal("missing workspace accepted")
	}
	if f.s.Directory() == "" {
		t.Fatal("missing directory")
	}
}

func TestReadFailsOnCorruptRecordsAndDoesNotRepair(t *testing.T) {
	f := setup(t)
	if _, err := f.s.db.Exec("UPDATE tasks SET record='bad JSON' WHERE id=?", f.task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.GetTask(t.Context(), f.task.ID); err == nil {
		t.Fatal("corrupt task decoded")
	}
	if _, err := f.s.ListTasks(t.Context(), "", 10); err == nil {
		t.Fatal("corrupt task list decoded")
	}
	var raw string
	if err := f.s.db.QueryRow("SELECT record FROM tasks WHERE id=?", f.task.ID).Scan(&raw); err != nil || raw != "bad JSON" {
		t.Fatal("read silently repaired corruption")
	}
}

func TestCancelledContextAndStoragePathGuards(t *testing.T) {
	f := setup(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := f.s.EnsureTask(ctx, task.Task{ID: "cancelled", Goal: "cancelled", Workspace: f.workspace}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := f.s.ReadArtifact(ctx, task.ArtifactRef{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := CanonicalWorkspace(file); err == nil {
		t.Fatal("file accepted as workspace")
	}
	if _, err := Open(file); err == nil {
		t.Fatal("file accepted as store")
	}
	if _, err := f.s.Acquire(t.Context(), "../escape", f.workspace); err == nil {
		t.Fatal("bad lease ID accepted")
	}
	if _, err := f.s.Acquire(t.Context(), "task", file); err == nil {
		t.Fatal("bad workspace lease accepted")
	}
	if err := os.Truncate(file, MaxDatabaseBytes+1); err != nil {
		t.Fatal(err)
	}
	if err := secureExistingFile(file); !errors.Is(err, task.ErrLimit) {
		t.Fatal(err)
	}
}
