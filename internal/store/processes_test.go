package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"meldra/internal/task"
)

func startingProcess(t *testing.T, f fixture) task.Process {
	t.Helper()
	call := plan(t, f, "start_process", task.Command)
	if err := f.s.StartTool(t.Context(), f.l, call.ID); err != nil {
		t.Fatal(err)
	}
	p, err := f.s.CreateProcess(t.Context(), f.l, task.Process{TaskID: f.task.ID, RunID: f.run.ID, ToolCallID: call.ID, OwnerEpoch: NewID(), LaunchHash: Hash([]byte("spec")), Executable: "/usr/bin/python3", Arguments: []string{"server.py"}, Directory: f.workspace, TimeoutSeconds: 600})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestProcessLifecycleSeparateFromStartAndRecovery(t *testing.T) {
	f := setup(t)
	p := startingProcess(t, f)
	p.State, p.PID = task.ProcessRunning, 123456
	if err := f.s.SaveProcess(t.Context(), f.l, p); err != nil {
		t.Fatal(err)
	}
	if err := f.s.FinishTool(t.Context(), f.l, p.ToolCallID, task.Result{Status: task.ToolSucceeded, Output: p.ID}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.FinishRun(t.Context(), f.l, f.run.ID, task.RunSucceeded, "done"); !errors.Is(err, task.ErrUnresolved) {
		t.Fatalf("running process accepted as completion: %v", err)
	}
	if err := f.s.RecoverInterrupted(t.Context(), f.l, f.task.ID); err != nil {
		t.Fatal(err)
	}
	recovered, err := f.s.GetProcess(t.Context(), p.ID)
	if err != nil || recovered.State != task.ProcessUnknown || recovered.Effects != "unknown" || recovered.PID != p.PID || recovered.ExitCode != nil {
		t.Fatalf("invented process completion: %+v %v", recovered, err)
	}
	call, err := f.s.GetToolCall(t.Context(), p.ToolCallID)
	if err != nil || call.Status != task.ToolSucceeded {
		t.Fatalf("start result overwritten: %+v %v", call, err)
	}
	if _, err := f.s.StartRun(t.Context(), f.l, task.Run{TaskID: f.task.ID}); !errors.Is(err, task.ErrUnresolved) {
		t.Fatalf("resumed unresolved process: %v", err)
	}
	if err := f.s.ResolveProcess(t.Context(), f.l, p.ID, ""); err == nil {
		t.Fatal("resolution without evidence accepted")
	}
	if err := f.s.ResolveProcess(t.Context(), f.l, p.ID, "inspected files and verified old server is no longer running"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.StartRun(t.Context(), f.l, task.Run{TaskID: f.task.ID}); err != nil {
		t.Fatal(err)
	}
	recovered, _ = f.s.GetProcess(t.Context(), p.ID)
	if recovered.State != task.ProcessUnknown || recovered.Effects != "resolved" {
		t.Fatalf("resolution invented known exit: %+v", recovered)
	}
}

func TestProcessRecordsRejectWrongOwnerAndDuplicateStart(t *testing.T) {
	f := setup(t)
	p := startingProcess(t, f)
	if _, err := f.s.CreateProcess(t.Context(), f.l, p); err == nil {
		t.Fatal("duplicate start accepted")
	}
	wrong := p
	wrong.State = task.ProcessRunning
	wrong.PID = 1
	wrong.OwnerEpoch = NewID()
	if err := f.s.SaveProcess(t.Context(), f.l, wrong); !errors.Is(err, task.ErrLease) {
		t.Fatalf("wrong owner accepted: %v", err)
	}
	p.State, p.Effects, p.EndedAt, p.ExitCode = task.ProcessExited, "known", time.Now(), new(7)
	if err := f.s.SaveProcess(t.Context(), f.l, p); err != nil {
		t.Fatal(err)
	}
	if err := f.s.SaveProcess(t.Context(), f.l, p); !errors.Is(err, task.ErrTransition) {
		t.Fatalf("terminal process rewritten: %v", err)
	}
	if err := f.s.FinishTool(t.Context(), f.l, p.ToolCallID, task.Result{Status: task.ToolSucceeded}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.FinishRun(t.Context(), f.l, f.run.ID, task.RunSucceeded, "observed exit"); err != nil {
		t.Fatal(err)
	}
}

func TestProcessMigrationFromSchemaOneRetainsHistory(t *testing.T) {
	f := setup(t)
	if _, err := f.s.db.Exec("DROP TABLE processes; PRAGMA user_version=1;"); err != nil {
		t.Fatal(err)
	}
	if err := f.l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(f.s.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.GetTask(t.Context(), f.task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Processes(t.Context(), f.task.ID); err != nil {
		t.Fatal(err)
	}
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != task.DatabaseSchemaVersion {
		t.Fatalf("migration version=%d err=%v", version, err)
	}
	events, err := s.Events(t.Context(), f.task.ID, 0, 100)
	if err != nil || len(events) == 0 || !strings.Contains(events[0].Kind, "task") || events[0].SchemaVersion != 1 {
		t.Fatalf("old events altered: %+v %v", events, err)
	}
}

func TestProcessCreatePersistenceFailureDoesNotLeaveIntent(t *testing.T) {
	f := setup(t)
	call := plan(t, f, "start_process", task.Command)
	if err := f.s.StartTool(t.Context(), f.l, call.ID); err != nil {
		t.Fatal(err)
	}
	f.s.beforeCommit = func() error { return errors.New("injected disk failure") }
	_, err := f.s.CreateProcess(t.Context(), f.l, task.Process{TaskID: f.task.ID, RunID: f.run.ID, ToolCallID: call.ID, OwnerEpoch: NewID(), LaunchHash: Hash([]byte("spec")), Executable: "/test", Directory: f.workspace, TimeoutSeconds: 5})
	if err == nil {
		t.Fatal("failed intent accepted")
	}
	f.s.beforeCommit = nil
	processes, err := f.s.Processes(t.Context(), f.task.ID)
	if err != nil || len(processes) != 0 {
		t.Fatalf("failed intent survived: %+v %v", processes, err)
	}
}
