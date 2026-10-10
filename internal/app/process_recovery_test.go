package app

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	taskstore "meldra/internal/store"
	"meldra/internal/task"
)

func savedRecoveryProcess(t *testing.T, e *taskExecution, known bool) task.Process {
	t.Helper()
	call, err := e.db.PlanTool(t.Context(), e.lease, task.ToolCall{TaskID: e.session.ID, RunID: e.run.ID, Name: "start_process", Effect: task.Command})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.db.StartTool(t.Context(), e.lease, call.ID); err != nil {
		t.Fatal(err)
	}
	p, err := e.db.CreateProcess(t.Context(), e.lease, task.Process{TaskID: e.session.ID, RunID: e.run.ID, ToolCallID: call.ID, OwnerEpoch: taskstore.NewID(), LaunchHash: taskstore.Hash([]byte("launch")), Executable: "/never/executable", Directory: e.workspace.root, TimeoutSeconds: 600})
	if err != nil {
		t.Fatal(err)
	}
	p.State, p.Effects, p.PID, p.EndedAt = task.ProcessExited, "known", os.Getpid(), time.Now().UTC()
	p.ExitCode = new(7)
	if !known {
		p.State, p.Effects, p.ExitCode = task.ProcessUnknown, "unknown", nil
	}
	artifact, err := e.db.PutArtifact(t.Context(), e.lease, e.session.ID, "process.log", []byte("actual retained output\n\x1b[31mlog with color\x1b[0m\n"+strings.Repeat("bounded ", 500)))
	if err != nil {
		t.Fatal(err)
	}
	p.Artifacts = []task.ArtifactRef{artifact}
	p.OutputEnd = artifact.Size
	if err := e.db.SaveProcess(t.Context(), e.lease, p); err != nil {
		t.Fatal(err)
	}
	if err := e.db.FinishTool(t.Context(), e.lease, call.ID, task.Result{Status: task.ToolSucceeded, Output: `{"state":"running","effects":"pending"}`}); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestProcessRecoveryIncludesIndependentOutcomesAndEvidence(t *testing.T) {
	agent, _ := recordedAgent(t, nil, nil)
	e := agent.execution
	if err := e.begin(t.Context(), "recover commands"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.close() })
	exited := savedRecoveryProcess(t, e, true)
	resolved := savedRecoveryProcess(t, e, false)
	if err := e.db.ResolveProcess(t.Context(), e.lease, resolved.ID, "checked files and external service state"); err != nil {
		t.Fatal(err)
	}
	record, err := e.db.GetTask(t.Context(), e.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := e.db.Events(t.Context(), e.session.ID, 0, 100)
	context, err := e.recoveryContext(t.Context(), record)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{exited.ID, "state=exited effects=known exit_code=7", resolved.ID, "state=unknown effects=resolved exit_code=unobserved", "checked files and external service state", "actual retained output", "log with color", exited.Artifacts[0].SHA256, "supersede its pending state", "never signal a persisted PID"} {
		if !strings.Contains(context, want) {
			t.Fatalf("missing process evidence %q in %s", want, context)
		}
	}
	if strings.ContainsRune(context, '\x1b') {
		t.Fatal("unsafe terminal bytes in recovery output")
	}
	after, _ := e.db.Events(t.Context(), e.session.ID, 0, 100)
	if len(after) != len(before) || e.workspace.processes != nil {
		t.Fatal("recovery evidence started execution or mutated history")
	}
	stored, err := e.db.GetProcess(t.Context(), resolved.ID)
	if err != nil || stored.PID != os.Getpid() || stored.State != task.ProcessUnknown {
		t.Fatalf("historical process identity changed: %+v %v", stored, err)
	}
}

func TestProcessRecoveryBoundAndCorruptLogDiagnostic(t *testing.T) {
	agent, paths := recordedAgent(t, nil, nil)
	e := agent.execution
	if err := e.begin(t.Context(), "bounded recovery"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.close() })
	for range 12 {
		_ = savedRecoveryProcess(t, e, true)
	}
	evidence, err := e.processRecoveryContext(t.Context(), e.session.ID)
	if err != nil || len(evidence) > maxProcessRecoveryBytes || !strings.Contains(evidence, "Additional process evidence omitted") {
		t.Fatalf("unbounded evidence len=%d err=%v", len(evidence), err)
	}
	processes, err := e.db.LatestProcesses(t.Context(), e.session.ID)
	if err != nil || len(processes) != 10 {
		t.Fatalf("unbounded process lookup: %d %v", len(processes), err)
	}
	path := taskDirectory(paths) + "/artifacts/" + processes[0].Artifacts[0].ID
	if err := os.WriteFile(path, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	evidence, err = e.processRecoveryContext(t.Context(), e.session.ID)
	if err != nil || !strings.Contains(evidence, "Retained log unavailable") || strings.Contains(evidence, "actual retained output") {
		t.Fatalf("corrupt log hidden or trusted: %q %v", evidence, err)
	}
}

func TestInterruptedProcessObservationsRecoverAsReads(t *testing.T) {
	agent, _ := recordedAgent(t, nil, nil)
	e := agent.execution
	if err := e.begin(t.Context(), "observe process"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.close() })
	for _, name := range []string{"process_status", "wait_process"} {
		if toolEffect(name) != task.Read {
			t.Fatalf("%s classified as side effect", name)
		}
		call, err := e.db.PlanTool(t.Context(), e.lease, task.ToolCall{TaskID: e.session.ID, RunID: e.run.ID, Name: name, Arguments: json.RawMessage(`{"process_id":"historical"}`), Effect: toolEffect(name)})
		if err != nil {
			t.Fatal(err)
		}
		if err := e.db.StartTool(t.Context(), e.lease, call.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.db.RecoverInterrupted(t.Context(), e.lease, e.session.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	unknown, err := e.db.UnknownToolCalls(t.Context(), e.session.ID, "")
	if err != nil || len(unknown) != 0 {
		t.Fatalf("observation needs side-effect resolution: %+v %v", unknown, err)
	}
}
