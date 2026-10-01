package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"meldra/internal/task"
	"meldra/internal/tool"
)

type recoveryCheckpoint struct {
	TaskID string `json:"task_id"`
	RunID  string `json:"run_id"`
	CallID string `json:"call_id"`
}

const recoveryEditArguments = `{"path":"evidence.txt","old_str":"before","new_str":"after"}`

// This child deliberately never defers execution cleanup. Its parent kills it
// after a durable marker so every case exercises real OS/process loss, rather
// than treating an ordinary close as proof of crash recovery.
func TestTaskRecoveryCrashProcessHelper(t *testing.T) {
	stage := os.Getenv("MELDRA_RECOVERY_FAULT_STAGE")
	if stage == "" {
		return
	}
	paths, err := ConfigPathsForHome(os.Getenv("MELDRA_RECOVERY_FAULT_HOME"))
	if err != nil {
		t.Fatal(err)
	}
	w, err := NewWorkspace(os.Getenv("MELDRA_RECOVERY_FAULT_WORKSPACE"), bufio.NewReader(strings.NewReader("")), io.Discard, true)
	if err != nil {
		t.Fatal(err)
	}
	sessions := newTaskSessionStore(paths)
	session, err := sessions.New(w.root)
	if err != nil {
		t.Fatal(err)
	}
	if err := sessions.Save(session); err != nil {
		t.Fatal(err)
	}
	e := &taskExecution{paths: paths, workspace: w, session: session, config: task.Config{Model: "offline", Provider: "fixture", Workspace: w.root}}
	w.approvalPending = e.pendingApproval
	w.approvalRecord = e.approval
	if err := e.begin(context.Background(), "replace before with after"); err != nil {
		t.Fatal(err)
	}
	checkpoint := recoveryCheckpoint{TaskID: session.ID, RunID: e.run.ID}
	mark := func() {
		data, err := json.Marshal(checkpoint)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(os.Getenv("MELDRA_RECOVERY_FAULT_READY"), data, 0600); err != nil {
			t.Fatal(err)
		}
		for {
			time.Sleep(time.Hour)
		}
	}
	if stage == "before_intent" {
		mark()
	}
	call, err := e.db.PlanTool(context.Background(), e.lease, task.ToolCall{TaskID: session.ID, RunID: e.run.ID, ProviderCallID: "crash-call", Name: "edit_file", Arguments: json.RawMessage(recoveryEditArguments), Effect: task.Write})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint.CallID = call.ID
	if stage == "intent_before_handler" {
		mark()
	}
	if err := e.db.StartTool(context.Background(), e.lease, call.ID); err != nil {
		t.Fatal(err)
	}
	e.current = &call
	if stage == "pending_approval" || stage == "approved_before_effect" {
		changes, err := w.prepare([]changeInput{{Path: "evidence.txt", OldStr: "before", NewStr: "after"}})
		if err != nil {
			t.Fatal(err)
		}
		request := ApprovalRequest{Kind: ApprovalChanges, Title: "replace evidence", WorkspaceState: changeFingerprints(changes, false)}
		if err := e.pendingApproval(context.Background(), request); err != nil {
			t.Fatal(err)
		}
		if stage == "approved_before_effect" {
			if err := e.approval(context.Background(), request, true); err != nil {
				t.Fatal(err)
			}
		}
		mark()
	}
	registry, err := tool.New(w.ToolDefinitions())
	if err != nil {
		t.Fatal(err)
	}
	result, err := registry.Invoke(context.WithValue(context.Background(), executionContextKey{}, e), "edit_file", json.RawMessage(recoveryEditArguments))
	if err != nil || result.Status != tool.Succeeded {
		t.Fatalf("edit = %+v %v", result, err)
	}
	if stage == "effect_before_result" {
		mark()
	}
	if err := e.db.FinishTool(context.Background(), e.lease, call.ID, task.Result{Status: task.ToolSucceeded, Output: result.Output}); err != nil {
		t.Fatal(err)
	}
	if stage == "result_before_followup" {
		mark()
	}
	t.Fatalf("unknown crash stage %q", stage)
}

func killedTaskAtCheckpoint(t *testing.T, stage string) (ConfigPaths, string, recoveryCheckpoint) {
	t.Helper()
	paths, err := ConfigPathsForHome(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "evidence.txt"), []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(t.TempDir(), "ready")
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestTaskRecoveryCrashProcessHelper$")
	cmd.Env = append(os.Environ(), "MELDRA_RECOVERY_FAULT_STAGE="+stage, "MELDRA_RECOVERY_FAULT_HOME="+paths.Home, "MELDRA_RECOVERY_FAULT_WORKSPACE="+workspace, "MELDRA_RECOVERY_FAULT_READY="+ready)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	deadline := time.Now().Add(15 * time.Second)
	var checkpoint recoveryCheckpoint
	for {
		if raw, err := os.ReadFile(ready); err == nil && json.Unmarshal(raw, &checkpoint) == nil && checkpoint.TaskID != "" {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatalf("child never reached %s: %s", stage, output.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("crash fixture exited normally")
	}
	return paths, workspace, checkpoint
}

func recoveryExecution(t *testing.T, paths ConfigPaths, workspace, sessionID string) *taskExecution {
	t.Helper()
	session, err := NewSessionStore(paths).Load(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	w, err := NewWorkspace(workspace, bufio.NewReader(strings.NewReader("")), io.Discard, true)
	if err != nil {
		t.Fatal(err)
	}
	e := &taskExecution{paths: paths, workspace: w, session: session, resume: true, config: task.Config{Model: "offline", Provider: "fixture", Workspace: w.root}}
	w.approvalPending = e.pendingApproval
	w.approvalRecord = e.approval
	t.Cleanup(func() { _ = e.close() })
	return e
}

func TestTaskRecoveryHardKillWindows(t *testing.T) {
	for _, test := range []struct {
		stage    string
		before   task.ToolStatus
		after    task.ToolStatus
		contents string
	}{
		{"before_intent", "", "", "before"},
		{"intent_before_handler", task.ToolPlanned, task.ToolCancelled, "before"},
		{"pending_approval", task.ToolRunning, task.ToolCancelled, "before"},
		{"approved_before_effect", task.ToolRunning, task.ToolFailed, "before"},
		{"effect_before_result", task.ToolRunning, task.ToolSucceeded, "after"},
		{"result_before_followup", task.ToolSucceeded, task.ToolSucceeded, "after"},
	} {
		t.Run(test.stage, func(t *testing.T) {
			paths, workspace, checkpoint := killedTaskAtCheckpoint(t, test.stage)
			db := openTaskDB(t, paths)
			original, err := db.GetRun(t.Context(), checkpoint.RunID)
			if err != nil || original.Status.Terminal() {
				t.Fatalf("inspection recovered without consent: %+v %v", original, err)
			}
			if checkpoint.CallID != "" {
				c, err := db.GetToolCall(t.Context(), checkpoint.CallID)
				if err != nil || c.Status != test.before {
					t.Fatalf("pre-recovery call %+v %v", c, err)
				}
			}
			e := recoveryExecution(t, paths, workspace, checkpoint.TaskID)
			if err := e.begin(t.Context(), "continue after explicit recovery"); err != nil {
				t.Fatal(err)
			}
			if checkpoint.CallID != "" {
				c, err := db.GetToolCall(t.Context(), checkpoint.CallID)
				if err != nil || c.Status != test.after {
					t.Fatalf("recovered call %+v %v", c, err)
				}
				calls := 0
				registry, err := tool.New([]ToolDefinition{{Name: "edit_file", Function: func(context.Context, json.RawMessage) (string, error) {
					calls++
					return "", errors.New("replayed side effect")
				}}})
				if err != nil {
					t.Fatal(err)
				}
				_, _ = e.invoke(t.Context(), registry, "crash-call", "edit_file", json.RawMessage(recoveryEditArguments))
				if calls != 0 {
					t.Fatal("recovery re-executed recorded operation")
				}
			}
			if err := e.finish(t.Context(), nil); err != nil {
				t.Fatal(err)
			}
			old, err := db.GetRun(t.Context(), checkpoint.RunID)
			if err != nil || old.Status != task.RunInterrupted || old.Reason != "execution_owner_lost" {
				t.Fatalf("old history overwritten: %+v %v", old, err)
			}
			contents, err := os.ReadFile(filepath.Join(workspace, "evidence.txt"))
			if err != nil || string(contents) != test.contents {
				t.Fatalf("file = %q %v", contents, err)
			}
			if test.stage == "pending_approval" {
				approvals, err := db.Approvals(t.Context(), checkpoint.TaskID)
				if err != nil || len(approvals) != 1 || approvals[0].Decision != task.Expired {
					t.Fatalf("stale approval: %+v %v", approvals, err)
				}
			}
		})
	}
}

func TestTaskRecoveryHardKillPreservesUserEdits(t *testing.T) {
	paths, workspace, checkpoint := killedTaskAtCheckpoint(t, "effect_before_result")
	if err := os.WriteFile(filepath.Join(workspace, "evidence.txt"), []byte("user changed after crash"), 0600); err != nil {
		t.Fatal(err)
	}
	e := recoveryExecution(t, paths, workspace, checkpoint.TaskID)
	if err := e.begin(t.Context(), "continue"); !errors.Is(err, task.ErrUnresolved) {
		t.Fatalf("user edit should require resolution: %v", err)
	}
	contents, err := os.ReadFile(filepath.Join(workspace, "evidence.txt"))
	if err != nil || string(contents) != "user changed after crash" {
		t.Fatalf("user edit overwritten: %q %v", contents, err)
	}
	db := openTaskDB(t, paths)
	c, err := db.GetToolCall(t.Context(), checkpoint.CallID)
	if err != nil || c.Status != task.ToolUnknown {
		t.Fatalf("unknown outcome fabricated: %+v %v", c, err)
	}
	runs, err := db.Runs(t.Context(), checkpoint.TaskID)
	if err != nil || len(runs) != 1 || runs[0].Status != task.RunInterrupted {
		t.Fatalf("new run started prematurely: %+v %v", runs, err)
	}
	lease, err := db.Acquire(t.Context(), checkpoint.TaskID, workspace)
	if err != nil {
		t.Fatalf("failed resume leaked ownership: %v", err)
	}
	_ = lease.Close()
}

func TestTaskRecoverySessionMutationRemainsUnknown(t *testing.T) {
	for name, args := range map[string]string{"update_plan": `{"steps":["persisted before crash"]}`, "save_summary": `{"summary":"persisted before crash"}`} {
		t.Run(name, func(t *testing.T) {
			agent, paths := recordedAgent(t, nil, nil)
			e := agent.execution
			if err := e.begin(t.Context(), "persist session metadata"); err != nil {
				t.Fatal(err)
			}
			call, err := e.db.PlanTool(t.Context(), e.lease, task.ToolCall{TaskID: e.session.ID, RunID: e.run.ID, Name: name, Arguments: json.RawMessage(args), Effect: task.Write})
			if err != nil {
				t.Fatal(err)
			}
			if err := e.db.StartTool(t.Context(), e.lease, call.ID); err != nil {
				t.Fatal(err)
			}
			registry, err := tool.New(NewSessionTools(e.session, agent.store).ToolDefinitions())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := registry.Invoke(t.Context(), name, json.RawMessage(args)); err != nil {
				t.Fatal(err)
			}
			if err := e.close(); err != nil {
				t.Fatal(err)
			}
			// Metadata saved before tool-result commit is not sufficient evidence
			// to claim that arbitrary prior session mutations all succeeded.
			restored := recoveryExecution(t, paths, e.workspace.root, e.session.ID)
			if err := restored.begin(t.Context(), "continue"); !errors.Is(err, task.ErrUnresolved) {
				t.Fatalf("session outcome guessed: %v", err)
			}
			saved, err := NewSessionStore(paths).Load(e.session.ID)
			if err != nil {
				t.Fatal(err)
			}
			if name == "update_plan" && (len(saved.Plan) != 1 || saved.Plan[0] != "persisted before crash") {
				t.Fatal("saved plan lost")
			}
			if name == "save_summary" && saved.Summary != "persisted before crash" {
				t.Fatal("saved summary lost")
			}
		})
	}
}

func TestTaskRecoveryFailedCallIsNeverAutomaticallyRetried(t *testing.T) {
	agent, _ := recordedAgent(t, nil, nil)
	e := agent.execution
	if err := e.begin(t.Context(), "failure recovery"); err != nil {
		t.Fatal(err)
	}
	executed := 0
	registry, err := tool.New([]ToolDefinition{{Name: "fixture", Function: func(context.Context, json.RawMessage) (string, error) {
		executed++
		return "failed", fmt.Errorf("known validation failure")
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.invoke(t.Context(), registry, "stable-failure", "fixture", json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected error")
	}
	if err := e.finish(t.Context(), errors.New("tool failed")); err != nil {
		t.Fatal(err)
	}
	e.resume = true
	if err := e.begin(t.Context(), "continue explicitly"); err != nil {
		t.Fatal(err)
	}
	result, err := e.invoke(t.Context(), registry, "stable-failure", "fixture", json.RawMessage(`{}`))
	if err == nil || result.Status != tool.Failed || executed != 1 {
		t.Fatalf("failure replay result %+v, err %v, count %d", result, err, executed)
	}
	if err := e.finish(t.Context(), errors.New("known failure retained")); err != nil {
		t.Fatal(err)
	}
}
