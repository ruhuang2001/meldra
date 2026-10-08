package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"meldra/internal/task"
)

type fixture struct {
	s         *Store
	l         *Lease
	task      task.Task
	run       task.Run
	workspace string
}

func setup(t testing.TB) fixture {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	workspace := t.TempDir()
	record, err := s.EnsureTask(t.Context(), task.Task{ID: NewID(), Goal: "repair test project", Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	l, err := s.Acquire(t.Context(), record.ID, workspace)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	run, err := s.StartRun(t.Context(), l, task.Run{TaskID: record.ID, Config: task.Config{Model: "offline", Provider: "fake"}, Executor: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return fixture{s, l, record, run, workspace}
}

func plan(t testing.TB, f fixture, name string, effect task.Effect) task.ToolCall {
	t.Helper()
	c, err := f.s.PlanTool(t.Context(), f.l, task.ToolCall{TaskID: f.task.ID, RunID: f.run.ID, Name: name, Arguments: json.RawMessage(`{"path":"file.go"}`), Effect: effect})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestDurableTaskRunToolsAndEvents(t *testing.T) {
	f := setup(t)
	c := plan(t, f, "read_file", task.Read)
	if err := f.s.StartTool(t.Context(), f.l, c.ID); err != nil {
		t.Fatal(err)
	}
	result := task.Result{Status: task.ToolSucceeded, Output: "contents", ExitCode: new(0), DurationMS: 10}
	if err := f.s.FinishTool(t.Context(), f.l, c.ID, result); err != nil {
		t.Fatal(err)
	}
	if err := f.s.FinishRun(t.Context(), f.l, f.run.ID, task.RunSucceeded, "model_completed"); err != nil {
		t.Fatal(err)
	}
	if err := f.s.FinishRun(t.Context(), f.l, f.run.ID, task.RunFailed, "overwrite"); !errors.Is(err, task.ErrTransition) {
		t.Fatalf("terminal run changed: %v", err)
	}
	run, err := f.s.GetRun(t.Context(), f.run.ID)
	if err != nil || run.Status != task.RunSucceeded || run.EndedAt.IsZero() {
		t.Fatalf("run = %+v, %v", run, err)
	}
	taskRecord, err := f.s.GetTask(t.Context(), f.task.ID)
	if err != nil || taskRecord.Status != task.Completed {
		t.Fatalf("task = %+v, %v", taskRecord, err)
	}
	calls, err := f.s.ToolCalls(t.Context(), f.task.ID)
	if err != nil || len(calls) != 1 || calls[0].Result.Output != "contents" || *calls[0].Result.ExitCode != 0 {
		t.Fatalf("calls = %+v, %v", calls, err)
	}
	events, err := f.s.Events(t.Context(), f.task.ID, 0, 100)
	if err != nil || len(events) != 6 {
		t.Fatalf("events = %+v, %v", events, err)
	}
	for i, e := range events {
		if e.Sequence != int64(i+1) || e.ID == "" || e.SchemaVersion != 1 || e.Time.IsZero() {
			t.Fatalf("bad event %+v", e)
		}
	}
	next, err := f.s.Events(t.Context(), f.task.ID, events[2].Sequence, 2)
	if err != nil || len(next) != 2 || next[0].Sequence != 4 {
		t.Fatalf("cursor: %+v %v", next, err)
	}
	listed, err := f.s.ListTasks(t.Context(), f.workspace, 20)
	if err != nil || len(listed) != 1 {
		t.Fatalf("list: %+v %v", listed, err)
	}
	if _, err := f.s.GetToolCall(t.Context(), "missing"); !errors.Is(err, task.ErrNotFound) {
		t.Fatal(err)
	}
	if err := f.l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(f.s.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err := reopened.GetToolCall(t.Context(), c.ID)
	if err != nil || got.Result.Output != "contents" {
		t.Fatalf("not durable: %+v %v", got, err)
	}
}

func TestRecoverInterruptedAndExplicitReconciliation(t *testing.T) {
	f := setup(t)
	unstarted := plan(t, f, "write_file", task.Write)
	started := plan(t, f, "run_command", task.Command)
	if err := f.s.StartTool(t.Context(), f.l, started.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.s.RecoverInterrupted(t.Context(), f.l, f.task.ID); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]task.ToolStatus{unstarted.ID: task.ToolCancelled, started.ID: task.ToolUnknown} {
		got, err := f.s.GetToolCall(t.Context(), id)
		if err != nil || got.Status != want {
			t.Fatalf("call: %+v %v", got, err)
		}
	}
	if _, err := f.s.StartRun(t.Context(), f.l, task.Run{TaskID: f.task.ID}); !errors.Is(err, task.ErrUnresolved) {
		t.Fatalf("unknown replay gate: %v", err)
	}
	if err := f.s.ResolveTool(t.Context(), f.l, started.ID, task.Result{Status: task.ToolSucceeded, Output: "user verified command output"}, "user_confirmed"); err != nil {
		t.Fatal(err)
	}
	if err := f.s.ResolveTool(t.Context(), f.l, started.ID, task.Result{Status: task.ToolFailed}, "overwrite"); !errors.Is(err, task.ErrTransition) {
		t.Fatalf("resolved outcome mutated: %v", err)
	}
	newRun, err := f.s.StartRun(t.Context(), f.l, task.Run{TaskID: f.task.ID})
	if err != nil || newRun.ID == f.run.ID {
		t.Fatalf("new run = %+v %v", newRun, err)
	}
	runs, err := f.s.Runs(t.Context(), f.task.ID)
	if err != nil || len(runs) != 2 || runs[0].Status != task.RunInterrupted || runs[0].Reason != "execution_owner_lost" {
		t.Fatalf("old history overwritten: %+v %v", runs, err)
	}
	if err := f.s.RecoverInterrupted(t.Context(), f.l, f.task.ID); err != nil {
		t.Fatal(err)
	}
	before, _ := f.s.Events(t.Context(), f.task.ID, 0, 1000)
	if err := f.s.RecoverInterrupted(t.Context(), f.l, f.task.ID); err != nil {
		t.Fatal(err)
	}
	after, _ := f.s.Events(t.Context(), f.task.ID, 0, 1000)
	if len(before) != len(after) {
		t.Fatal("recovery not idempotent")
	}
}

func TestApprovalBindingDecisionsAndExpiry(t *testing.T) {
	f := setup(t)
	c := plan(t, f, "write_file", task.Write)
	a := task.Approval{TaskID: f.task.ID, RunID: f.run.ID, ToolCallID: c.ID, Operation: "write file", ParameterHash: "wrong", WorkspaceState: json.RawMessage(`{"before":"abc"}`)}
	if _, err := f.s.RecordApproval(t.Context(), f.l, a); err == nil {
		t.Fatal("accepted stale parameters")
	}
	a.ParameterHash = c.ParameterHash
	pending, err := f.s.RecordApproval(t.Context(), f.l, a)
	if err != nil {
		t.Fatal(err)
	}
	gotTask, _ := f.s.GetTask(t.Context(), f.task.ID)
	if gotTask.Status != task.WaitingApproval {
		t.Fatal(gotTask.Status)
	}
	if err := f.s.StartTool(t.Context(), f.l, c.ID); !errors.Is(err, task.ErrTransition) {
		t.Fatalf("executed while awaiting approval: %v", err)
	}
	if err := f.s.DecideApproval(t.Context(), f.l, pending.ID, task.Approved); err != nil {
		t.Fatal(err)
	}
	if err := f.s.DecideApproval(t.Context(), f.l, pending.ID, task.Approved); !errors.Is(err, task.ErrTransition) {
		t.Fatalf("approval consumed twice: %v", err)
	}
	if err := f.s.StartTool(t.Context(), f.l, c.ID); err != nil {
		t.Fatal(err)
	}
	a.ID = ""
	a.Decision = task.Pending
	second, err := f.s.RecordApproval(t.Context(), f.l, a)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.FinishRun(t.Context(), f.l, f.run.ID, task.RunCancelled, "signal"); err != nil {
		t.Fatal(err)
	}
	if err := f.s.DecideApproval(t.Context(), f.l, second.ID, task.Approved); !errors.Is(err, task.ErrTransition) {
		t.Fatalf("old approval survived: %v", err)
	}
	approvals, err := f.s.Approvals(t.Context(), f.task.ID)
	if err != nil || len(approvals) != 2 || approvals[0].Decision != task.Approved || approvals[1].Decision != task.Expired || approvals[0].Scope != "tool_call" {
		t.Fatalf("approvals: %+v %v", approvals, err)
	}
}

func TestFaultsRollbackStateAndEvents(t *testing.T) {
	for _, boundary := range []string{"intent", "started", "result", "finished_run"} {
		t.Run(boundary, func(t *testing.T) {
			f := setup(t)
			var c task.ToolCall
			if boundary != "intent" {
				c = plan(t, f, "write_file", task.Write)
			}
			if boundary == "result" || boundary == "finished_run" {
				if err := f.s.StartTool(t.Context(), f.l, c.ID); err != nil {
					t.Fatal(err)
				}
			}
			if boundary == "finished_run" {
				if err := f.s.FinishTool(t.Context(), f.l, c.ID, task.Result{Status: task.ToolSucceeded}); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := f.s.Events(t.Context(), f.task.ID, 0, 1000)
			fault := errors.New("injected disk commit failure")
			f.s.beforeCommit = func() error { return fault }
			var err error
			switch boundary {
			case "intent":
				_, err = f.s.PlanTool(t.Context(), f.l, task.ToolCall{TaskID: f.task.ID, RunID: f.run.ID, Name: "write_file", Effect: task.Write})
			case "started":
				err = f.s.StartTool(t.Context(), f.l, c.ID)
			case "result":
				err = f.s.FinishTool(t.Context(), f.l, c.ID, task.Result{Status: task.ToolSucceeded, Output: "changed"})
			case "finished_run":
				err = f.s.FinishRun(t.Context(), f.l, f.run.ID, task.RunSucceeded, "done")
			}
			if !errors.Is(err, fault) {
				t.Fatalf("fault not propagated: %v", err)
			}
			f.s.beforeCommit = nil
			after, _ := f.s.Events(t.Context(), f.task.ID, 0, 1000)
			if len(before) != len(after) {
				t.Fatal("event committed without operation")
			}
			if err := f.s.RecoverInterrupted(t.Context(), f.l, f.task.ID); err != nil {
				t.Fatal(err)
			}
			calls, _ := f.s.ToolCalls(t.Context(), f.task.ID)
			switch boundary {
			case "intent":
				if len(calls) != 0 {
					t.Fatal("uncommitted intent persisted")
				}
			case "started":
				if calls[0].Status != task.ToolCancelled {
					t.Fatal(calls[0].Status)
				}
			case "result":
				if calls[0].Status != task.ToolUnknown {
					t.Fatal(calls[0].Status)
				}
			case "finished_run":
				if calls[0].Status != task.ToolSucceeded {
					t.Fatal(calls[0].Status)
				}
			}
		})
	}
}

func TestOpenMigratesRejectsFutureAndPreservesRecords(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 1 {
		t.Fatalf("version %d %v", version, err)
	}
	var journal string
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil || journal != "wal" {
		t.Fatalf("journal %s %v", journal, err)
	}
	for _, path := range []string{dir, filepath.Join(dir, "tasks.db"), filepath.Join(dir, "tasks.db-wal"), filepath.Join(dir, "tasks.db-shm")} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0077 != 0 {
			t.Fatalf("insecure permissions %s %o", path, info.Mode().Perm())
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("PRAGMA user_version=99"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("opened future schema: %v", err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "tasks.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 99 {
		t.Fatal("future database modified")
	}
}

func TestImportIdempotentAtomicAndImmutable(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	workspace := t.TempDir()
	legacy := []byte(`{"id":"old-session","messages":["hello"]}`)
	path := filepath.Join(t.TempDir(), "session.json")
	if err := os.WriteFile(path, legacy, 0600); err != nil {
		t.Fatal(err)
	}
	record := task.Task{ID: "old-session", Goal: "hello", Workspace: workspace}
	fault := errors.New("import interrupted")
	s.beforeCommit = func() error { return fault }
	if _, err := s.ImportLegacy(t.Context(), record, path, Hash(legacy)); !errors.Is(err, fault) {
		t.Fatal(err)
	}
	if _, err := s.GetTask(t.Context(), record.ID); !errors.Is(err, task.ErrNotFound) {
		t.Fatal("partial import exists")
	}
	s.beforeCommit = nil
	first, err := s.ImportLegacy(t.Context(), record, path, Hash(legacy))
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.ImportLegacy(t.Context(), record, path, Hash(legacy))
	if err != nil || first.ID != second.ID || !second.LegacyHistoryMissing {
		t.Fatalf("import: %+v %v", second, err)
	}
	events, _ := s.Events(t.Context(), first.ID, 0, 1000)
	if len(events) != 2 {
		t.Fatal("duplicate import event")
	}
	if _, err := s.ImportLegacy(t.Context(), record, path, Hash([]byte("changed"))); err != nil {
		t.Fatal(err)
	}
	listed, _ := s.ListTasks(t.Context(), "", 100)
	if len(listed) != 1 {
		t.Fatal("new snapshot created duplicate task")
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(legacy) {
		t.Fatal("source altered")
	}
	calls, _ := s.ToolCalls(t.Context(), first.ID)
	if len(calls) != 0 {
		t.Fatal("fabricated legacy tools")
	}
}

func TestArtifactsLimitsDigestsAndSymlinks(t *testing.T) {
	f := setup(t)
	data := []byte("full command log")
	ref, err := f.s.PutArtifact(t.Context(), f.l, f.task.ID, "command.log", data)
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.s.ReadArtifact(t.Context(), ref)
	if err != nil || string(got) != string(data) {
		t.Fatalf("artifact %q %v", got, err)
	}
	if _, err := f.s.PutArtifact(t.Context(), f.l, f.task.ID, "another.log", data); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.PutArtifact(t.Context(), f.l, f.task.ID, "huge", make([]byte, MaxArtifactBytes+1)); !errors.Is(err, task.ErrLimit) {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.s.dir, "artifacts", ref.ID), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.ReadArtifact(t.Context(), ref); err == nil {
		t.Fatal("digest mismatch accepted")
	}
	if _, err := f.s.PutArtifact(t.Context(), f.l, f.task.ID, "command.log", data); err == nil {
		t.Fatal("corrupt content overwritten silently")
	}
	ref.ID = "../tasks.db"
	if _, err := f.s.ReadArtifact(t.Context(), ref); err == nil {
		t.Fatal("artifact traversal accepted")
	}
	dir := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(filepath.Join(link, "nested")); err == nil {
		t.Fatal("store ancestor symlink accepted")
	}
	if err := os.Symlink(filepath.Join(f.s.dir, "tasks.db"), filepath.Join(dir, "tasks.db")); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err == nil {
		t.Fatal("database symlink accepted")
	}
}

func TestOwnershipAcrossTasksStoresAndSymlinkAliases(t *testing.T) {
	f := setup(t)
	other, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	alias := filepath.Join(t.TempDir(), "workspace")
	if err := os.Symlink(f.workspace, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := other.Acquire(t.Context(), "other-task", alias); !errors.Is(err, task.ErrBusy) {
		t.Fatalf("config dir bypass: %v", err)
	}
	if _, err := f.s.Acquire(t.Context(), f.task.ID, t.TempDir()); !errors.Is(err, task.ErrBusy) {
		t.Fatalf("task lock bypass: %v", err)
	}
	if err := f.s.AppendEvent(t.Context(), nil, task.Event{TaskID: f.task.ID, Kind: "bad"}); !errors.Is(err, task.ErrLease) {
		t.Fatal(err)
	}
	if _, err := other.StartRun(t.Context(), f.l, task.Run{TaskID: f.task.ID}); !errors.Is(err, task.ErrLease) {
		t.Fatal("cross store lease accepted")
	}
	if err := f.l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.s.AppendEvent(t.Context(), f.l, task.Event{TaskID: f.task.ID, Kind: "bad"}); !errors.Is(err, task.ErrLease) {
		t.Fatal("closed lease accepted")
	}
	l, err := other.Acquire(t.Context(), "other-task", alias)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := f.s.Acquire(ctx, "cancelled", f.workspace); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestSubprocessOwnershipHelper(t *testing.T) {
	if os.Getenv("MELDRA_LOCK_HELPER") != "1" {
		return
	}
	s, err := Open(os.Getenv("MELDRA_LOCK_STORE"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(3)
	}
	l, err := s.Acquire(context.Background(), "child-task", os.Getenv("MELDRA_LOCK_WORKSPACE"))
	if errors.Is(err, task.ErrBusy) {
		os.Exit(4)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(5)
	}
	if os.Getenv("MELDRA_LOCK_READY") != "" {
		if err := os.WriteFile(os.Getenv("MELDRA_LOCK_READY"), []byte("ready"), 0600); err != nil {
			os.Exit(6)
		}
		for {
			time.Sleep(time.Hour)
		}
	}
	_ = l.Close()
	_ = s.Close()
	os.Exit(0)
}

func TestProcessLockContentionAndCrashRelease(t *testing.T) {
	f := setup(t)
	childDir := t.TempDir()
	command := func(ready string) *exec.Cmd {
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestSubprocessOwnershipHelper$")
		cmd.Env = append(os.Environ(), "MELDRA_LOCK_HELPER=1", "MELDRA_LOCK_STORE="+childDir, "MELDRA_LOCK_WORKSPACE="+f.workspace, "MELDRA_LOCK_READY="+ready)
		return cmd
	}
	if err := command("").Run(); err == nil {
		t.Fatal("second process acquired locked workspace")
	} else if e, ok := errors.AsType[*exec.ExitError](err); !ok || e.ExitCode() != 4 {
		t.Fatal(err)
	}
	if err := f.l.Close(); err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(t.TempDir(), "ready")
	cmd := command(ready)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child did not acquire lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := f.s.Acquire(t.Context(), f.task.ID, f.workspace); !errors.Is(err, task.ErrBusy) {
		t.Fatalf("child lock absent: %v", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	l, err := f.s.Acquire(t.Context(), f.task.ID, f.workspace)
	if err != nil {
		t.Fatalf("SIGKILL leaked lock: %v", err)
	}
	defer l.Close()
	// Inspecting the stale run never mutates it; explicit recovery is required.
	r, err := f.s.GetRun(t.Context(), f.run.ID)
	if err != nil || r.Status != task.RunRunning {
		t.Fatalf("read recovered automatically: %+v %v", r, err)
	}
}
