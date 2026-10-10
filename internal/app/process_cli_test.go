package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	taskstore "meldra/internal/store"
	"meldra/internal/task"
)

func TestTaskProcessInspectionAndExplicitResolution(t *testing.T) {
	agent, paths := recordedAgent(t, nil, nil)
	e := agent.execution
	if err := e.begin(t.Context(), "process recovery"); err != nil {
		t.Fatal(err)
	}
	call, err := e.db.PlanTool(t.Context(), e.lease, task.ToolCall{TaskID: agent.session.ID, RunID: e.run.ID, Name: "start_process", Effect: task.Command})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.db.StartTool(t.Context(), e.lease, call.ID); err != nil {
		t.Fatal(err)
	}
	p, err := e.db.CreateProcess(t.Context(), e.lease, task.Process{TaskID: agent.session.ID, RunID: e.run.ID, ToolCallID: call.ID, OwnerEpoch: taskstore.NewID(), LaunchHash: taskstore.Hash([]byte("spec")), Executable: "/fixture/python3", Directory: e.workspace.root, TimeoutSeconds: 600})
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately retain our own PID in historical evidence. Inspection and
	// reconciliation must never signal it or reinterpret it as live ownership.
	p.State, p.PID = task.ProcessRunning, os.Getpid()
	if err := e.db.SaveProcess(t.Context(), e.lease, p); err != nil {
		t.Fatal(err)
	}
	if err := e.db.FinishTool(t.Context(), e.lease, call.ID, task.Result{Status: task.ToolSucceeded, Output: p.ID}); err != nil {
		t.Fatal(err)
	}
	if err := e.close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MELDRA_HOME", paths.Home)
	var output bytes.Buffer
	if err := runTaskCommand(t.Context(), []string{"show", agent.session.ID, "--json"}, nil, &output); err != nil {
		t.Fatal(err)
	}
	var shown struct {
		Processes []task.Process `json:"processes"`
	}
	if err := json.Unmarshal(output.Bytes(), &shown); err != nil || len(shown.Processes) != 1 || shown.Processes[0].State != task.ProcessRunning {
		t.Fatalf("process omitted or inspection recovered it: %s %v", output.String(), err)
	}
	db := openTaskDB(t, paths)
	if err := runTaskCommand(t.Context(), []string{"resolve-process", agent.session.ID, "nonexistent", "--reason", "check"}, nil, io.Discard); !errors.Is(err, task.ErrNotFound) {
		t.Fatalf("invalid target accepted: %v", err)
	}
	run, err := db.GetRun(t.Context(), p.RunID)
	if err != nil || run.Status != task.RunRunning {
		t.Fatalf("wrong target changed run: %+v %v", run, err)
	}
	if err := runTaskCommand(t.Context(), []string{"resolve-process", agent.session.ID, p.ID, "--reason", " "}, nil, io.Discard); err == nil {
		t.Fatal("empty reason accepted")
	}
	output.Reset()
	if err := runTaskCommand(t.Context(), []string{"resolve-process", agent.session.ID, p.ID, "--reason", "inspected outputs and current process state"}, nil, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "resume the task explicitly") {
		t.Fatalf("resolution output=%q", output.String())
	}
	recovered, err := db.GetProcess(t.Context(), p.ID)
	if err != nil || recovered.State != task.ProcessUnknown || recovered.Effects != "resolved" || recovered.PID != os.Getpid() {
		t.Fatalf("bad resolution %+v %v", recovered, err)
	}
	run, err = db.GetRun(t.Context(), p.RunID)
	if err != nil || run.Status != task.RunInterrupted {
		t.Fatalf("historical run changed incorrectly: %+v %v", run, err)
	}
	events, err := db.Events(t.Context(), agent.session.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, event := range events {
		if event.Kind == "process.resolved" && event.Reason == "inspected outputs and current process state" {
			found = true
		}
	}
	if !found {
		t.Fatal("missing explicit reconciliation evidence")
	}
}

func TestManagedProcessExcludesUnknownRemoteEvenWithReadOnlyDescription(t *testing.T) {
	w, registry, executable := managedWorkspace(t)
	view := invokeProcess(t, registry, "start_process", helperInput(executable, "wait", 10))
	executed := false
	definitions := w.guardedDefinitions([]ToolDefinition{{Name: "mcp__server__read", Description: "read-only external server annotation", Function: w.bindTool(func(json.RawMessage) (string, error) { executed = true; return "done", nil })}})
	if _, err := definitions[0].Function(t.Context(), json.RawMessage(`{}`)); !errors.Is(err, ErrWorkspaceBusy) || executed {
		t.Fatalf("remote side effects admitted: executed=%v err=%v", executed, err)
	}
	_ = invokeProcess(t, registry, "stop_process", map[string]any{"process_id": view.Process.ID})
	if _, err := definitions[0].Function(t.Context(), json.RawMessage(`{}`)); !errors.Is(err, task.ErrUnresolved) || executed {
		t.Fatalf("unknown process effects bypassed: executed=%v err=%v", executed, err)
	}
}
