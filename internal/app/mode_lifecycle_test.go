package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"meldra/internal/provider"
	"meldra/internal/task"
)

func TestLegacyResumePlanCommandImportsWithoutChangingSource(t *testing.T) {
	paths, err := ConfigPathsForHome(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	legacy := NewSessionStore(paths)
	session, err := legacy.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session.appendMessage("user", "original request")
	if err := legacy.Save(session); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(legacy.path(session.ID))
	if err != nil {
		t.Fatal(err)
	}
	messages := []string{"/plan", "inspect without editing"}
	getMessage := func() (string, bool) {
		if len(messages) == 0 {
			return "", false
		}
		next := messages[0]
		messages = messages[1:]
		return next, true
	}
	runtime, err := newChatRuntime(t.Context(), paths, Settings{Model: "fixture", BaseURL: defaultBaseURL}, ChatOptions{Resume: session.ID}, func(root string, auto bool) (*Workspace, error) {
		return NewWorkspace(root, bufio.NewReader(strings.NewReader("")), io.Discard, auto)
	}, getMessage, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.mcp.close()
	requests := 0
	runtime.agent.backend = inferenceFunc(func(_ context.Context, request provider.Request, _ provider.Options, _ provider.Observer) (provider.Result, error) {
		requests++
		if !strings.Contains(request.Instructions, "Runtime mode: plan") {
			t.Fatal("legacy /plan was not applied before inference")
		}
		return provider.Result{Response: finishResponse()}, nil
	})
	if err := runtime.agent.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	unchanged, err := os.ReadFile(legacy.path(session.ID))
	if err != nil || !bytes.Equal(original, unchanged) {
		t.Fatal("mode selection changed immutable legacy source")
	}
	loaded, err := legacy.Load(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if requests != 1 || !loaded.taskSnapshot || loaded.Mode != ModePlan || loaded.PolicyGeneration != 1 {
		t.Fatalf("requests=%d loaded=%+v", requests, loaded)
	}
	db := openTaskDB(t, paths)
	records, err := db.ListTasks(t.Context(), "", 10)
	if err != nil || len(records) != 1 || !records[0].LegacyHistoryMissing || records[0].Goal != "original request" {
		t.Fatalf("legacy import=%+v err=%v", records, err)
	}
}

func TestIdleModeChangePersistsGenerationAndExactPlanRevision(t *testing.T) {
	a, paths := recordedAgent(t, inferenceFunc(func(context.Context, provider.Request, provider.Options, provider.Observer) (provider.Result, error) {
		return provider.Result{Response: finishResponse()}, nil
	}), nil)
	if err := a.RunTurn(t.Context(), "establish task"); err != nil {
		t.Fatal(err)
	}
	if !a.session.taskSnapshot {
		t.Fatal("executed task did not become task-era snapshot")
	}
	a.session.Plan = []string{"Inspect implementation", "Apply reviewed change"}
	if err := a.applyMode(ModePlan); err != nil {
		t.Fatal(err)
	}
	planSession, err := newTaskSessionStore(paths).Load(a.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if planSession.Mode != ModePlan || planSession.PolicyGeneration != 1 || planSession.ApprovedPlanDigest != "" {
		t.Fatalf("Plan snapshot=%+v", planSession)
	}
	if err := a.applyMode(ModeBuild); err != nil {
		t.Fatal(err)
	}
	buildSession, err := newTaskSessionStore(paths).Load(a.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(a.session.Plan)
	if buildSession.Mode != ModeBuild || buildSession.PolicyGeneration != 2 || buildSession.ApprovedPlanDigest != digest(raw) {
		t.Fatalf("Build snapshot=%+v", buildSession)
	}
	oldDigest := buildSession.ApprovedPlanDigest
	a.session.Plan = append(a.session.Plan, "New unapproved step")
	if err := a.store.Save(a.session); err != nil {
		t.Fatal(err)
	}
	changed, err := newTaskSessionStore(paths).Load(a.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if changed.ApprovedPlanDigest != oldDigest {
		t.Fatal("editing plan silently approved its new revision")
	}
	for _, raw := range []string{`{"command":"go","args":["test","./..."]}`, `{"command":"make","args":["test"]}`} {
		_, _, generation := a.policy.snapshot()
		ctx := context.WithValue(t.Context(), operationGenerationKey{}, generation-1)
		if _, err := a.policy.invocation(ctx, "run_command", json.RawMessage(raw)); err == nil {
			t.Fatal("old Plan/Build generation admitted command")
		}
	}
}

func TestInferenceModeContractAndToolCatalogChangeTogether(t *testing.T) {
	w := contextWorkspace(t)
	tools := append(w.ToolDefinitions(), ToolDefinition{Name: "external_fake", Function: func(context.Context, json.RawMessage) (string, error) { return "", nil }}, ToolDefinition{Name: "update_plan", Function: func(context.Context, json.RawMessage) (string, error) { return "", nil }})
	var requests []provider.Request
	a := NewAgent(inferenceFunc(func(_ context.Context, request provider.Request, _ provider.Options, _ provider.Observer) (provider.Result, error) {
		requests = append(requests, request)
		return provider.Result{Response: finishResponse()}, nil
	}), nil, tools)
	a.output = io.Discard
	a.policy, _ = newRuntimePolicy(ModePlan, PermissionWorkspaceEdit)
	if _, err := a.runInference(t.Context(), provider.UserInput("inspect"), ""); err != nil {
		t.Fatal(err)
	}
	if err := a.applyMode(ModeBuild); err != nil {
		t.Fatal(err)
	}
	if _, err := a.runInference(t.Context(), provider.UserInput("implement"), ""); err != nil {
		t.Fatal(err)
	}
	for i, request := range requests {
		var names []string
		for _, tool := range request.Tools {
			names = append(names, tool.Name)
		}
		mode := "plan"
		if i == 1 {
			mode = "build"
		}
		if !strings.Contains(request.Instructions, "Runtime mode: "+mode) || !strings.Contains(request.Instructions, "Permission profile: workspace-edit") {
			t.Fatalf("mode contract=%s", request.Instructions)
		}
		for _, name := range []string{"edit_file", "apply_patch", "undo_last_change", "run_command", "external_fake"} {
			if slices.Contains(names, name) != (i == 1) {
				t.Fatalf("mode=%s tool=%s names=%v", mode, name, names)
			}
		}
		for _, name := range []string{"read_file", "list_files", "search_files", "git_review", "verify", "update_plan"} {
			if !slices.Contains(names, name) {
				t.Fatalf("mode=%s omitted %s", mode, name)
			}
		}
	}
}

func TestMalformedModeRejectedAtCLIAndSessionBoundaries(t *testing.T) {
	for _, args := range [][]string{{"--mode", "unsafe"}, {"--mode", "PLAN"}, {"--permissions", "allow-everything"}} {
		if _, err := parseChatOptions(args); err == nil {
			t.Fatalf("malformed options accepted: %v", args)
		}
	}
	a, _ := recordedAgent(t, nil, nil)
	before, _, generation := a.policy.snapshot()
	if err := a.applyMode("unsafe"); err == nil {
		t.Fatal("invalid runtime mode accepted")
	}
	after, _, gotGeneration := a.policy.snapshot()
	if after != before || gotGeneration != generation {
		t.Fatal("invalid mode changed runtime policy")
	}
	for _, mode := range []ExecutionMode{"unsafe", "PLAN", "build ", "\x00"} {
		session := *a.session
		session.Mode = mode
		if err := validateSession(&session, session.ID); err == nil {
			t.Fatalf("malformed stored mode accepted: %q", mode)
		}
	}
}

func TestHeadlessSetModePersistsTaskMode(t *testing.T) {
	a, paths := recordedAgent(t, inferenceFunc(func(context.Context, provider.Request, provider.Options, provider.Observer) (provider.Result, error) {
		return provider.Result{Response: finishResponse()}, nil
	}), nil)
	if err := a.RunTurn(t.Context(), "establish task"); err != nil {
		t.Fatal(err)
	}
	if err := a.SetMode(ModePlan); err != nil {
		t.Fatal(err)
	}
	loaded, err := newTaskSessionStore(paths).Load(a.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Mode != ModePlan || loaded.PolicyGeneration != 1 {
		t.Fatalf("headless mode not persisted: %+v", loaded)
	}
}

func TestRunPolicySnapshotsRemainImmutableAcrossModeChanges(t *testing.T) {
	a, paths := recordedAgent(t, inferenceFunc(func(context.Context, provider.Request, provider.Options, provider.Observer) (provider.Result, error) {
		return provider.Result{Response: finishResponse()}, nil
	}), nil)
	if err := a.RunTurn(t.Context(), "build first"); err != nil {
		t.Fatal(err)
	}
	if err := a.SetMode(ModePlan); err != nil {
		t.Fatal(err)
	}
	if err := a.RunTurn(t.Context(), "inspect second"); err != nil {
		t.Fatal(err)
	}
	a.session.Plan = []string{"Reviewed implementation step"}
	if err := a.SetMode(ModeBuild); err != nil {
		t.Fatal(err)
	}
	if err := a.RunTurn(t.Context(), "build third"); err != nil {
		t.Fatal(err)
	}
	db := openTaskDB(t, paths)
	runs, err := db.Runs(t.Context(), a.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 3 {
		t.Fatalf("runs=%d", len(runs))
	}
	slices.SortFunc(runs, func(a, b task.Run) int { return a.StartedAt.Compare(b.StartedAt) })
	for i, run := range runs {
		wantMode := "build"
		if i == 1 {
			wantMode = "plan"
		}
		if run.Config.Mode != wantMode || run.Config.Permissions != "interactive" || run.Config.PolicyGeneration != uint64(i) {
			t.Fatalf("run%d policy=%+v", i, run.Config)
		}
	}
	if runs[0].Config.ApprovedPlanDigest != "" || runs[1].Config.ApprovedPlanDigest != "" || runs[2].Config.ApprovedPlanDigest != a.session.ApprovedPlanDigest {
		t.Fatalf("plan approval snapshots changed: %+v", runs)
	}
}

func TestDirectMCPExecutionSnapshotsWorkspacePlanPolicy(t *testing.T) {
	a, _ := recordedAgent(t, nil, nil)
	a.execution.workspace.policy, _ = newRuntimePolicy(ModePlan, PermissionWorkspaceEdit)
	a.execution.workspace.policy.generation = 7
	if err := a.execution.begin(t.Context(), "MCP read"); err != nil {
		t.Fatal(err)
	}
	defer a.execution.finish(t.Context(), nil)
	config := a.execution.run.Config
	if config.Mode != "plan" || config.Permissions != "workspace-edit" || config.PolicyGeneration != 7 {
		t.Fatalf("MCP run lost policy: %+v", config)
	}
}

func TestModeSaveFailureRollsBackPolicyAndBlocksHeadlessReuse(t *testing.T) {
	for _, stage := range []string{"before_replace", "after_replace"} {
		t.Run(stage, func(t *testing.T) {
			requests := 0
			a, paths := recordedAgent(t, inferenceFunc(func(context.Context, provider.Request, provider.Options, provider.Observer) (provider.Result, error) {
				requests++
				return provider.Result{Response: finishResponse()}, nil
			}), nil)
			if err := a.RunTurn(t.Context(), "establish task"); err != nil {
				t.Fatal(err)
			}
			if err := a.SetMode(ModePlan); err != nil {
				t.Fatal(err)
			}
			a.session.Plan = []string{"reviewed work"}
			store := a.store.(*SessionStore)
			fault := errors.New("snapshot durability failed")
			if stage == "after_replace" {
				store.syncDir = func(string) error { return fault }
			} else {
				a.store = saveSessionFunc(func(*Session) error { return fault })
			}
			_, _, beforeGeneration := a.policy.snapshot()
			err := a.SetMode(ModeBuild)
			if _, ok := errors.AsType[*persistenceError](err); !ok || !errors.Is(err, fault) {
				t.Fatalf("save error=%v", err)
			}
			mode, _, generation := a.policy.snapshot()
			if mode != ModePlan || generation <= beforeGeneration || a.session.Mode != ModePlan || a.session.ApprovedPlanDigest != "" {
				t.Fatalf("failed transition leaked policy: mode=%s generation=%d session=%+v", mode, generation, a.session)
			}
			if err := a.RunTurn(t.Context(), "retry after failed mode save"); !errors.Is(err, fault) {
				t.Fatalf("headless reused uncertain snapshot: %v", err)
			}
			if err := a.SetMode(ModeBuild); !errors.Is(err, fault) {
				t.Fatalf("mode retry cleared fatal state: %v", err)
			}
			if requests != 1 {
				t.Fatalf("provider requests after failed transition=%d", requests)
			}
			loaded, err := newTaskSessionStore(paths).Load(a.session.ID)
			if err != nil {
				t.Fatal(err)
			}
			want := ModePlan
			if stage == "after_replace" {
				want = ModeBuild
			}
			if loaded.Mode != want {
				t.Fatalf("fault fixture failed to exercise %s: mode=%s", stage, loaded.Mode)
			}
		})
	}
}
