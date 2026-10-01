package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"meldra/internal/provider"
	taskstore "meldra/internal/store"
	"meldra/internal/task"
	"meldra/internal/tool"
)

func recordedAgent(t *testing.T, backend provider.Inference, definitions []ToolDefinition) (*Agent, ConfigPaths) {
	t.Helper()
	paths, err := ConfigPathsForHome(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := NewWorkspace(t.TempDir(), bufio.NewReader(strings.NewReader("")), io.Discard, true)
	if err != nil {
		t.Fatal(err)
	}
	session, err := NewSessionStore(paths).New(workspace.root)
	if err != nil {
		t.Fatal(err)
	}
	if definitions == nil {
		definitions = workspace.ToolDefinitions()
	}
	agent := NewAgent(backend, nil, definitions)
	agent.output = io.Discard
	agent.session = session
	agent.store = newTaskSessionStore(paths)
	agent.execution = &taskExecution{paths: paths, workspace: workspace, session: session, config: task.Config{Model: "fixture", Provider: "test", Workspace: workspace.root}}
	workspace.approvalRecord = agent.execution.approval
	workspace.approvalPending = agent.execution.pendingApproval
	return agent, paths
}
func callResponse(id, name, args string) *provider.Response {
	return &provider.Response{ID: "r-" + id, Status: "completed", Output: []provider.OutputItem{{Type: "function_call", CallID: id, Name: name, Arguments: args}}}
}
func finishResponse() *provider.Response {
	return &provider.Response{ID: "done", Status: "completed", Text: "verified"}
}
func openTaskDB(t *testing.T, paths ConfigPaths) *taskstore.Store {
	t.Helper()
	db, err := taskstore.Open(taskDirectory(paths))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestTaskExecutionRecordsFilesApprovalsAndArtifacts(t *testing.T) {
	requests := 0
	backend := inferenceFunc(func(context.Context, provider.Request, provider.Options, provider.Observer) (provider.Result, error) {
		requests++
		if requests == 1 {
			return provider.Result{Response: callResponse("new-file", "edit_file", `{"path":"new.txt","old_str":"","new_str":"content"}`)}, nil
		}
		return provider.Result{Response: finishResponse()}, nil
	})
	agent, paths := recordedAgent(t, backend, nil)
	if err := agent.RunTurn(t.Context(), "create file"); err != nil {
		t.Fatal(err)
	}
	db := openTaskDB(t, paths)
	record, err := db.GetTask(t.Context(), agent.session.ID)
	if err != nil || record.Status != task.Completed {
		t.Fatalf("record=%+v err=%v", record, err)
	}
	calls, err := db.ToolCalls(t.Context(), record.ID)
	if err != nil || len(calls) != 1 || calls[0].Status != task.ToolSucceeded {
		t.Fatalf("calls=%+v err=%v", calls, err)
	}
	approvals, err := db.Approvals(t.Context(), record.ID)
	if err != nil || len(approvals) != 1 || approvals[0].Decision != task.Approved {
		t.Fatalf("approvals=%+v err=%v", approvals, err)
	}
	if approvals[0].ParameterHash != calls[0].ParameterHash || len(approvals[0].WorkspaceState) == 0 {
		t.Fatal("approval lacks operation evidence")
	}
	if len(calls[0].Result.Artifacts) != 1 {
		t.Fatal("missing artifact")
	}
	data, err := db.ReadArtifact(t.Context(), calls[0].Result.Artifacts[0])
	if err != nil || !strings.Contains(string(data), "Applied successfully") {
		t.Fatalf("artifact=%q %v", data, err)
	}
	events, err := db.Events(t.Context(), record.ID, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for i, event := range events {
		if event.Sequence != int64(i+1) || event.SchemaVersion != 1 {
			t.Fatalf("event=%+v", event)
		}
	}
}
func TestTaskExecutionRedeliveredCallUsesRecordedResult(t *testing.T) {
	executed, requests := 0, 0
	backend := inferenceFunc(func(context.Context, provider.Request, provider.Options, provider.Observer) (provider.Result, error) {
		requests++
		if requests%2 == 1 {
			return provider.Result{Response: callResponse("stable-call", "count", "{}")}, nil
		}
		return provider.Result{Response: finishResponse()}, nil
	})
	agent, paths := recordedAgent(t, backend, []ToolDefinition{{Name: "count", Function: func(context.Context, json.RawMessage) (string, error) { executed++; return "done", nil }}})
	if err := agent.RunTurn(t.Context(), "one"); err != nil {
		t.Fatal(err)
	}
	if err := agent.RunTurn(t.Context(), "continue"); err != nil {
		t.Fatal(err)
	}
	if executed != 1 {
		t.Fatalf("side effect executed %d times", executed)
	}
	db := openTaskDB(t, paths)
	runs, err := db.Runs(t.Context(), agent.session.ID)
	if err != nil || len(runs) != 2 {
		t.Fatalf("runs=%v %v", runs, err)
	}
}
func TestTaskExecutionStopsAfterRecordingFailure(t *testing.T) {
	var agent *Agent
	requests, executed := 0, 0
	backend := inferenceFunc(func(context.Context, provider.Request, provider.Options, provider.Observer) (provider.Result, error) {
		requests++
		if requests == 1 {
			agent.execution.db.Close()
			return provider.Result{Response: callResponse("call", "effect", "{}")}, nil
		}
		t.Fatal("continued after ledger failure")
		return provider.Result{}, nil
	})
	agent, _ = recordedAgent(t, backend, []ToolDefinition{{Name: "effect", Function: func(context.Context, json.RawMessage) (string, error) { executed++; return "done", nil }}})
	if err := agent.RunTurn(t.Context(), "record"); err == nil {
		t.Fatal("expected storage error")
	}
	if executed != 0 {
		t.Fatal("side effect ran after persistence failure")
	}
}
func TestTaskExecutionUnknownEffectRequiresExplicitResolution(t *testing.T) {
	requests := 0
	backend := inferenceFunc(func(context.Context, provider.Request, provider.Options, provider.Observer) (provider.Result, error) {
		requests++
		return provider.Result{Response: callResponse("call", "external", "{}")}, nil
	})
	agent, paths := recordedAgent(t, backend, []ToolDefinition{{Name: "external", Function: func(ctx context.Context, _ json.RawMessage) (string, error) {
		tool.Observe(ctx, func(o *tool.Observation) { o.Started = true })
		return "", errors.New("connection lost after execution")
	}}})
	if err := agent.RunTurn(t.Context(), "external action"); err == nil {
		t.Fatal("unknown side effect should stop run")
	}
	db := openTaskDB(t, paths)
	calls, err := db.ToolCalls(t.Context(), agent.session.ID)
	if err != nil || len(calls) != 1 || calls[0].Status != task.ToolUnknown {
		t.Fatalf("calls=%v err=%v", calls, err)
	}
	agent.execution.resume = true
	if err := agent.RunTurn(t.Context(), "continue"); !errors.Is(err, task.ErrUnresolved) {
		t.Fatalf("resume = %v", err)
	}
	if requests != 1 {
		t.Fatal("model invoked before unknown outcome resolved")
	}
	lease, err := db.Acquire(t.Context(), agent.session.ID, agent.execution.workspace.root)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ResolveTool(t.Context(), lease, calls[0].ID, task.Result{Status: task.ToolSucceeded, Output: "user checked remote result"}, "verified externally"); err != nil {
		t.Fatal(err)
	}
	lease.Close()
}

func TestTaskRecoveryRecognizesFilePostimages(t *testing.T) {
	agent, paths := recordedAgent(t, inferenceFunc(func(context.Context, provider.Request, provider.Options, provider.Observer) (provider.Result, error) {
		return provider.Result{Response: finishResponse()}, nil
	}), nil)
	e := agent.execution
	if err := e.begin(t.Context(), "create file"); err != nil {
		t.Fatal(err)
	}
	input := json.RawMessage(`{"path":"restored.txt","old_str":"","new_str":"done"}`)
	call, err := e.db.PlanTool(t.Context(), e.lease, task.ToolCall{TaskID: e.session.ID, RunID: e.run.ID, Name: "edit_file", Arguments: input, Effect: task.Write})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.db.StartTool(t.Context(), e.lease, call.ID); err != nil {
		t.Fatal(err)
	}
	changes, err := e.workspace.prepare([]changeInput{{Path: "restored.txt", NewStr: "done"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.db.RecordApproval(t.Context(), e.lease, task.Approval{TaskID: e.session.ID, RunID: e.run.ID, ToolCallID: call.ID, Operation: "changes", ParameterHash: digest(input), Decision: task.Approved, Scope: "single_call", WorkspaceState: changeFingerprints(changes, false)})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.workspace.writeChanges(changes, false); err != nil {
		t.Fatal(err)
	}
	// Simulate process death after side effect and before result commit.
	if err := e.close(); err != nil {
		t.Fatal(err)
	}
	e.resume = true
	// Persist session needed for explicit recovery's legacy lookup, then mark as known task.
	if err := agent.store.Save(agent.session); err != nil {
		t.Fatal(err)
	}
	if err := agent.RunTurn(t.Context(), "continue"); err != nil {
		t.Fatal(err)
	}
	db := openTaskDB(t, paths)
	stored, err := db.GetToolCall(t.Context(), call.ID)
	if err != nil || stored.Status != task.ToolSucceeded {
		t.Fatalf("call=%+v err=%v", stored, err)
	}
	runs, err := db.Runs(t.Context(), agent.session.ID)
	if err != nil || len(runs) != 2 || runs[0].Status != task.RunInterrupted || runs[1].Status != task.RunSucceeded {
		t.Fatalf("runs=%+v %v", runs, err)
	}
	contents, err := os.ReadFile(filepath.Join(e.workspace.root, "restored.txt"))
	if err != nil || string(contents) != "done" {
		t.Fatalf("contents=%q %v", contents, err)
	}
}

func TestTaskPendingApprovalIsVisibleBeforeDecision(t *testing.T) {
	requests := 0
	agent, paths := recordedAgent(t, inferenceFunc(func(context.Context, provider.Request, provider.Options, provider.Observer) (provider.Result, error) {
		requests++
		if requests == 1 {
			return provider.Result{Response: callResponse("edit", "edit_file", `{"path":"new.txt","old_str":"","new_str":"content"}`)}, nil
		}
		return provider.Result{Response: finishResponse()}, nil
	}), nil)
	agent.execution.workspace.autoApprove = false
	agent.execution.workspace.SetApprovalFunc(func(ctx context.Context, request ApprovalRequest) bool {
		record, err := agent.execution.db.GetTask(ctx, agent.session.ID)
		if err != nil || record.Status != task.WaitingApproval {
			t.Fatalf("approval status=%+v err=%v", record, err)
		}
		approvals, err := agent.execution.db.Approvals(ctx, agent.session.ID)
		if err != nil || len(approvals) != 1 || approvals[0].Decision != task.Pending {
			t.Fatalf("pending=%+v err=%v", approvals, err)
		}
		return false
	})
	if err := agent.RunTurn(t.Context(), "decline file"); err != nil {
		t.Fatal(err)
	}
	db := openTaskDB(t, paths)
	calls, err := db.ToolCalls(t.Context(), agent.session.ID)
	if err != nil || len(calls) != 1 || calls[0].Status != task.ToolDeclined {
		t.Fatalf("calls=%+v err=%v", calls, err)
	}
	if _, err := os.Stat(filepath.Join(agent.execution.workspace.root, "new.txt")); !os.IsNotExist(err) {
		t.Fatal("declined file written")
	}
}

func TestTaskCommandArtifactRetainsOutputBeyondModelExcerpt(t *testing.T) {
	requests := 0
	agent, paths := recordedAgent(t, inferenceFunc(func(context.Context, provider.Request, provider.Options, provider.Observer) (provider.Result, error) {
		requests++
		if requests == 1 {
			return provider.Result{Response: callResponse("print", "run_command", `{"command":"python3","args":["emit.py"]}`)}, nil
		}
		return provider.Result{Response: finishResponse()}, nil
	}), nil)
	script := "print('x' * (300 * 1024))\nprint('TAIL_MARKER')\n"
	if err := os.WriteFile(filepath.Join(agent.execution.workspace.root, "emit.py"), []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	if err := agent.RunTurn(t.Context(), "run program"); err != nil {
		t.Fatal(err)
	}
	db := openTaskDB(t, paths)
	calls, err := db.ToolCalls(t.Context(), agent.session.ID)
	if err != nil || len(calls) != 1 {
		t.Fatalf("calls=%v err=%v", calls, err)
	}
	result := calls[0].Result
	if !result.Truncated || strings.Contains(result.Output, "TAIL_MARKER") || len(result.Artifacts) != 2 {
		t.Fatalf("truncated=%t artifacts=%v", result.Truncated, result.Artifacts)
	}
	full, err := db.ReadArtifact(t.Context(), result.Artifacts[0])
	if err != nil || !strings.Contains(string(full), "TAIL_MARKER") {
		t.Fatalf("log length=%d err=%v", len(full), err)
	}
}
