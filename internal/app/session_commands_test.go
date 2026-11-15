package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"meldra/internal/provider"
	"meldra/internal/task"
)

func TestSessionCommandsSwitchForkAndResume(t *testing.T) {
	var requests []provider.Request
	a, paths := recordedAgent(t, inferenceFunc(func(_ context.Context, r provider.Request, _ provider.Options, _ provider.Observer) (provider.Result, error) {
		requests = append(requests, r)
		return provider.Result{Response: finishResponse()}, nil
	}), nil)
	a.workspace = a.execution.workspace
	unsavedID := a.session.ID
	if err := a.handleSessionCommand(t.Context(), "fork", ""); err != nil {
		t.Fatal(err)
	}
	if a.session.ID != unsavedID || a.session.ParentSessionID != "" {
		t.Fatal("fork created a reference to an unsaved parent")
	}
	if err := a.RunTurn(t.Context(), "original intent"); err != nil {
		t.Fatal(err)
	}
	parentID := a.session.ID
	a.contextWindowTokens = 123456
	a.recordPromptCalibration(1000, 500)
	input := []string{"/model fast-model", "/fork", "alternative approach"}
	a.getUserMessage = func() (string, bool) {
		if len(input) == 0 {
			return "", false
		}
		s := input[0]
		input = input[1:]
		return s, true
	}
	if err := a.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 || requests[1].Model != "fast-model" || requests[1].PreviousResponseID != "" {
		t.Fatalf("requests=%+v", requests)
	}
	if a.session.ID == parentID || a.session.ParentSessionID != parentID || a.contextWindowTokens != 0 {
		t.Fatalf("session=%+v", a.session)
	}
	parent, err := newTaskSessionStore(paths).Load(parentID)
	if err != nil {
		t.Fatal(err)
	}
	if len(parent.Messages) != 2 || parent.Messages[0].Content != "original intent" {
		t.Fatalf("parent modified: %+v", parent)
	}
	child, err := newTaskSessionStore(paths).Load(a.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(child.Messages) != 4 || child.Model != "fast-model" || child.ParentSessionID != parentID {
		t.Fatalf("child=%+v", child)
	}
	items, err := NewSessionStore(paths).List()
	if err != nil || len(items) != 2 {
		t.Fatalf("sessions=%+v err=%v", items, err)
	}
	a.session = child
	a.execution.session = child
	a.execution.resume = true
	if err := a.RunTurn(t.Context(), "continue branch"); err != nil {
		t.Fatal(err)
	}
	if len(a.session.Messages) != 6 {
		t.Fatalf("fork replayed parent messages: %+v", a.session.Messages)
	}
}

func TestSessionCommandsResetNativeContinuation(t *testing.T) {
	for _, command := range []string{"model", "fork"} {
		t.Run(command, func(t *testing.T) {
			var requests []provider.Request
			a, _ := recordedAgent(t, inferenceFunc(func(_ context.Context, r provider.Request, _ provider.Options, _ provider.Observer) (provider.Result, error) {
				requests = append(requests, r)
				return provider.Result{Response: finishResponse()}, nil
			}), nil)
			for _, input := range []string{"original intent", "establish continuation"} {
				if err := a.RunTurn(t.Context(), input); err != nil {
					t.Fatal(err)
				}
			}
			if requests[1].PreviousResponseID == "" {
				t.Fatal("fixture did not establish native continuation")
			}
			value := ""
			if command == "model" {
				value = "replacement-model"
			}
			if err := a.handleSessionCommand(t.Context(), command, value); err != nil {
				t.Fatal(err)
			}
			if err := a.RunTurn(t.Context(), "continue independently"); err != nil {
				t.Fatal(err)
			}
			if requests[2].PreviousResponseID != "" || !strings.Contains(fmt.Sprintf("%+v", requests[2].Input), "original intent") {
				t.Fatalf("continuation was not rebuilt: %+v", requests[2])
			}
		})
	}
}

func TestForkContinuesSessionControlsAndCompaction(t *testing.T) {
	for _, scenario := range []string{"live", "resumed", "legacy", "legacy-compact", "legacy-mismatch", "legacy-missing-event"} {
		t.Run(scenario, func(t *testing.T) {
			backend := inferenceFunc(func(_ context.Context, r provider.Request, _ provider.Options, _ provider.Observer) (provider.Result, error) {
				return provider.Result{Response: finishResponse()}, nil
			})
			a, paths := recordedAgent(t, backend, nil)
			if err := a.RunTurn(t.Context(), "inherited intent"); err != nil {
				t.Fatal(err)
			}
			parentID := a.session.ID
			if err := a.handleSessionCommand(t.Context(), "fork", ""); err != nil {
				t.Fatal(err)
			}
			if err := a.RunTurn(t.Context(), "child intent"); err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(scenario, "legacy") {
				for i := range a.session.Messages {
					a.session.Messages[i].Inherited = false
				}
				if scenario == "legacy-mismatch" {
					a.session.Messages[0].Content = "not in the parent snapshot"
				}
				if scenario == "legacy-missing-event" {
					parent, err := newTaskSessionStore(paths).Load(parentID)
					if err != nil {
						t.Fatal(err)
					}
					parent.ID, parent.hasSavedRevision = "unrelated-parent", false
					if err := newTaskSessionStore(paths).Save(parent); err != nil {
						t.Fatal(err)
					}
					a.session.ParentSessionID = parent.ID
				}
				if err := a.store.Save(a.session); err != nil {
					t.Fatal(err)
				}
			}
			if scenario != "live" {
				runtime, err := newChatRuntime(t.Context(), paths, Settings{Model: "fixture", BaseURL: defaultBaseURL}, ChatOptions{Resume: a.session.ID}, func(root string, auto bool) (*Workspace, error) {
					w, _ := testWorkspace(t, root, "", auto)
					return w, nil
				}, nil, io.Discard)
				if err != nil {
					t.Fatal(err)
				}
				defer runtime.mcp.close()
				a = runtime.agent
				a.backend = backend
			}
			if scenario == "legacy-mismatch" || scenario == "legacy-missing-event" {
				model := a.modelName()
				if err := a.handleSessionCommand(t.Context(), "model", "replacement-model"); err == nil || a.modelName() != model {
					t.Fatalf("unverified inherited history accepted: %v", err)
				}
				return
			}
			if scenario == "legacy-compact" {
				a.compactKeepMessages = 3
				if err := a.handleCompactCommand(t.Context(), ""); err != nil {
					t.Fatal(err)
				}
				if a.session.ContextCompaction == nil || !a.session.Messages[0].Inherited {
					t.Fatal("legacy provenance was not retained through compaction")
				}
			}
			if err := a.handleSessionCommand(t.Context(), "model", "replacement-model"); err != nil {
				t.Fatal(err)
			}
			if err := a.handleSessionCommand(t.Context(), "fork", ""); err != nil {
				t.Fatal(err)
			}
			if err := a.RunTurn(t.Context(), "grandchild intent"); err != nil {
				t.Fatal(err)
			}
			a.compactKeepMessages = 3 // The retained tail starts with an inherited assistant.
			if err := a.handleCompactCommand(t.Context(), ""); err != nil {
				t.Fatal(err)
			}
			if a.session.ContextCompaction == nil || len(a.session.Messages) != 3 {
				t.Fatal("fork history was not compacted")
			}
			if err := a.handleSessionCommand(t.Context(), "model", "after-compaction"); err != nil {
				t.Fatal(err)
			}
			parent, err := newTaskSessionStore(paths).Load(parentID)
			if err != nil || len(parent.Messages) != 2 {
				t.Fatalf("parent changed: %+v err=%v", parent, err)
			}
		})
	}
}

func TestModelSaveFailurePreservesRuntime(t *testing.T) {
	a, _ := recordedAgent(t, nil, nil)
	old := a.modelName()
	a.store = saveSessionFunc(func(*Session) error { return errors.New("disk full") })
	if err := a.handleSessionCommand(t.Context(), "model", "new-model"); err == nil {
		t.Fatal("save failure ignored")
	}
	if a.modelName() != old || a.session.Model != "" {
		t.Fatal("failed save switched model")
	}
}

func TestModelSelectionBeforeFirstTurnSurvivesRuntimeRestart(t *testing.T) {
	a, paths := recordedAgent(t, nil, nil)
	if err := a.handleSessionCommand(t.Context(), "model", "new-model"); err != nil {
		t.Fatal(err)
	}
	runtime, err := newChatRuntime(t.Context(), paths, Settings{Model: "configured-model", BaseURL: defaultBaseURL, ContextWindowTokens: 12345}, ChatOptions{Resume: a.session.ID}, func(root string, auto bool) (*Workspace, error) {
		w, _ := testWorkspace(t, root, "", auto)
		return w, nil
	}, nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.mcp.close()
	if runtime.agent.modelName() != "new-model" || runtime.agent.contextWindowTokens != 0 {
		t.Fatal("saved model was not restored")
	}
	runtime.agent.backend = inferenceFunc(func(_ context.Context, request provider.Request, _ provider.Options, _ provider.Observer) (provider.Result, error) {
		if request.Model != "new-model" {
			t.Fatal(request.Model)
		}
		return provider.Result{Response: finishResponse()}, nil
	})
	if err := runtime.agent.RunTurn(t.Context(), "first actual request"); err != nil {
		t.Fatal(err)
	}
}

func TestIdleCommandsRejectBusyAndUnresolvedWork(t *testing.T) {
	a, paths := recordedAgent(t, inferenceFunc(func(context.Context, provider.Request, provider.Options, provider.Observer) (provider.Result, error) {
		return provider.Result{Response: finishResponse()}, nil
	}), nil)
	if err := a.RunTurn(t.Context(), "start"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	if err := a.beginControl(ctx, cancel); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"model", "fork", "init"} {
		if err := a.SubmitControl(name, ""); err == nil || !strings.Contains(err.Error(), "only while idle") {
			t.Fatalf("%s: %v", name, err)
		}
	}
	a.endControl()
	db := openTaskDB(t, paths)
	lease, err := db.Acquire(t.Context(), a.session.ID, a.session.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	run, err := db.StartRun(t.Context(), lease, task.Run{TaskID: a.session.ID, Config: a.execution.config, Executor: "test"})
	if err != nil {
		t.Fatal(err)
	}
	call, err := db.PlanTool(t.Context(), lease, task.ToolCall{TaskID: a.session.ID, RunID: run.ID, Name: "git_commit", Arguments: []byte(`{}`), Effect: task.Command})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.StartTool(t.Context(), lease, call.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishRun(t.Context(), lease, run.ID, task.RunInterrupted, "interrupted"); err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	id, model := a.session.ID, a.modelName()
	for _, command := range []struct{ kind, value string }{{"fork", ""}, {"model", "replacement"}} {
		if err := a.handleSessionCommand(t.Context(), command.kind, command.value); err != nil {
			t.Fatal(err)
		}
		if a.session.ID != id || a.modelName() != model {
			t.Fatal("unresolved effects escaped into another session")
		}
	}
}

func TestTUIModelSwitchUpdatesHeaderAndClearsWaterline(t *testing.T) {
	m := newTUIModel(newTUIController(nil), tuiInitialState{model: "old-model"})
	m.width, m.height = 90, 24
	m.resize()
	m.applyEvent(UIEvent{Kind: UIEventMetrics, Metrics: &UIMetrics{InputTokens: 1000, ContextTokens: 1000, ContextLimitTokens: 2000}})
	m.applyEvent(UIEvent{Kind: UIEventModel, Text: "fast-model"})
	view := m.View().Content
	if !strings.Contains(view, "fast-model") || strings.Contains(view, "old-model") || strings.Contains(view, "ctx 50%") {
		t.Fatal(view)
	}
}

func TestInitUsesNormalEditApproval(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		mode   ExecutionMode
		accept bool
	}{{"plan", ModePlan, true}, {"declined", ModeBuild, false}, {"approved", ModeBuild, true}} {
		t.Run(scenario.name, func(t *testing.T) {
			calls := 0
			a, paths := recordedAgent(t, inferenceFunc(func(_ context.Context, r provider.Request, _ provider.Options, _ provider.Observer) (provider.Result, error) {
				calls++
				if calls == 1 {
					return provider.Result{Response: callResponse("init", "edit_file", `{"path":"AGENTS.md","old_str":"","new_str":"# Project rules\n"}`)}, nil
				}
				return provider.Result{Response: finishResponse()}, nil
			}), nil)
			a.execution.workspace.policy = a.policy
			a.execution.workspace.autoApprove = false
			approvals := 0
			a.execution.workspace.SetApprovalFunc(func(context.Context, ApprovalRequest) bool {
				approvals++
				return scenario.accept
			})
			if err := a.SetMode(scenario.mode); err != nil {
				t.Fatal(err)
			}
			inputs := []string{"/init"}
			a.getUserMessage = func() (string, bool) {
				if len(inputs) == 0 {
					return "", false
				}
				s := inputs[0]
				inputs = nil
				return s, true
			}
			if err := a.Run(t.Context()); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(a.session.Workspace, "AGENTS.md"))
			wrote := scenario.mode == ModeBuild && scenario.accept
			if wrote && (err != nil || string(data) != "# Project rules\n") || !wrote && !os.IsNotExist(err) {
				t.Fatalf("file=%q err=%v", data, err)
			}
			recorded, err := openTaskDB(t, paths).Approvals(t.Context(), a.session.ID)
			if err != nil {
				t.Fatal(err)
			}
			if scenario.mode == ModePlan {
				if approvals != 0 || len(recorded) != 0 {
					t.Fatal("Plan offered write approval")
				}
			} else {
				want := task.Declined
				if scenario.accept {
					want = task.Approved
				}
				if approvals != 1 || len(recorded) != 1 || recorded[0].Decision != want {
					t.Fatalf("approvals=%d records=%+v", approvals, recorded)
				}
			}
		})
	}
}
