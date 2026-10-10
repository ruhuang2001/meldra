package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/charmbracelet/x/ansi"
	"github.com/openai/openai-go/v3/responses"
	"io"
	"meldra/internal/provider"
	taskstore "meldra/internal/store"
	"meldra/internal/task"
	"meldra/internal/tool"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestArtifactFailurePreservesCompletedEdit(t *testing.T) {
	for _, mode := range []string{"quota", "io"} {
		t.Run(mode, func(t *testing.T) {
			requests := 0
			a, paths := recordedAgent(t, inferenceFunc(func(context.Context, provider.Request, provider.Options, provider.Observer) (provider.Result, error) {
				requests++
				if requests == 1 {
					return provider.Result{Response: callResponse("edit", "edit_file", `{"path":"created.txt","old_str":"","new_str":"written"}`)}, nil
				}
				return provider.Result{Response: finishResponse()}, nil
			}), nil)
			dir := filepath.Join(taskDirectory(paths), "artifacts")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if mode == "quota" {
				f, err := os.Create(filepath.Join(dir, "quota-fixture"))
				if err != nil {
					t.Fatal(err)
				}
				err = f.Truncate(taskstore.MaxArtifactTotalBytes)
				f.Close()
				if err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Remove(dir); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(t.TempDir(), dir); err != nil {
					t.Fatal(err)
				}
			}
			runErr := a.RunTurn(t.Context(), "create file")
			if mode == "quota" && (runErr != nil || requests != 2) {
				t.Fatalf("quota stopped execution: %v requests=%d", runErr, requests)
			}
			if mode == "io" && (runErr == nil || requests != 1) {
				t.Fatalf("IO failure did not stop: %v requests=%d", runErr, requests)
			}
			data, err := os.ReadFile(filepath.Join(a.execution.workspace.root, "created.txt"))
			if err != nil || string(data) != "written" {
				t.Fatalf("effect=%q %v", data, err)
			}
			calls, err := openTaskDB(t, paths).ToolCalls(t.Context(), a.session.ID)
			if err != nil || len(calls) != 1 || calls[0].Status != task.ToolSucceeded || !calls[0].Result.Truncated {
				t.Fatalf("calls=%+v %v", calls, err)
			}
		})
	}
}

func TestStreamTextIsRetainedInCustomToolReplay(t *testing.T) {
	requests := 0
	var followup string
	client := &provider.Client{CreateStream: func(_ context.Context, params responses.ResponseNewParams) provider.Stream {
		requests++
		if requests == 1 {
			return &scriptedResponseStream{events: []responses.ResponseStreamEventUnion{
				{Type: "response.output_text.done", Text: "preserve this explanation"},
				{Type: "response.completed", Response: *responseFromJSON(t, `{"id":"text-call","status":"completed","output":[{"type":"function_call","id":"fc","call_id":"call","name":"list_files","arguments":"{}","status":"completed"}]}`)},
			}}
		}
		data, err := json.Marshal(params)
		if err != nil {
			t.Fatal(err)
		}
		followup = string(data)
		return &scriptedResponseStream{events: []responses.ResponseStreamEventUnion{{Type: "response.completed", Response: streamedCompletedResponse(t, "finished")}}}
	}}
	a, _ := recordedAgent(t, client, nil)
	a.customProvider = true
	if err := a.RunTurn(t.Context(), "inspect"); err != nil {
		t.Fatal(err)
	}
	if requests != 2 || !strings.Contains(followup, "preserve this explanation") {
		t.Fatalf("text missing from replay: %s", followup)
	}
}

func TestNarrowHeaderKeepsCursorOnComposer(t *testing.T) {
	m := newTUIModel(newTUIController(nil), tuiInitialState{workspace: "/tmp/long-long-workspace", model: strings.Repeat("model", 20), sessionID: strings.Repeat("id", 30)})
	m.input.Focus()
	m.width = 30
	m.height = 24
	m.resize()
	view := m.View()
	header, _, _ := strings.Cut(view.Content, "\n")
	if ansi.StringWidth(header) > m.width {
		t.Fatalf("header wraps: %q", header)
	}
	if view.Cursor == nil {
		t.Fatal("missing focused cursor")
	}
	if view.Cursor.Y < tuiHeaderHeight+m.viewport.Height() {
		t.Fatalf("cursor above composer: %+v", view.Cursor)
	}
}

func TestTaskTextOutputFailureIsReturned(t *testing.T) {
	a, paths := recordedAgent(t, inferenceFunc(func(context.Context, provider.Request, provider.Options, provider.Observer) (provider.Result, error) {
		return provider.Result{Response: finishResponse()}, nil
	}), nil)
	if err := a.RunTurn(t.Context(), "inspect"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MELDRA_HOME", paths.Home)
	if err := runTasksCommand(t.Context(), nil, failedTaskWriter{}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("listing error=%v", err)
	}
	if err := runTaskCommand(t.Context(), []string{"show", a.session.ID}, nil, failedTaskWriter{}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("show error=%v", err)
	}
}

type failedTaskWriter struct{}

func (failedTaskWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestProviderIdentityPreservesPortWithoutCredentials(t *testing.T) {
	if got := providerIdentity("http://secret:password@[::1]:8080/v1?key=secret#private"); got != "http://[::1]:8080/v1" {
		t.Fatal(got)
	}
}

func TestFailedToolKeepsPartialOutputForProvider(t *testing.T) {
	a := NewAgent(nil, nil, []ToolDefinition{{Name: "partial", Function: func(context.Context, json.RawMessage) (string, error) {
		return "diagnostic before failure", errors.New("command failed")
	}}})
	a.output = io.Discard
	items := a.executeToolCallsContext(t.Context(), []provider.OutputItem{{Type: "function_call", CallID: "partial-call", Name: "partial", Arguments: "{}"}})
	data, err := json.Marshal(items)
	if err != nil || !strings.Contains(string(data), "diagnostic before failure") || !strings.Contains(string(data), "Error: command failed") {
		t.Fatalf("output=%s err=%v", data, err)
	}
}

func TestCancelledToolPlanningIsNotPersistenceFailure(t *testing.T) {
	a, paths := recordedAgent(t, nil, nil)
	e := a.execution
	if err := e.begin(t.Context(), "cancel"); err != nil {
		t.Fatal(err)
	}
	registry, err := tool.New(e.workspace.ToolDefinitions())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = e.invoke(ctx, registry, "cancelled", "list_files", json.RawMessage(`{}`))
	if !errors.Is(err, context.Canceled) || e.err != nil {
		t.Fatalf("cancel misclassified: %v %v", err, e.err)
	}
	if err := e.finish(ctx, err); err != nil {
		t.Fatal(err)
	}
	runs, err := openTaskDB(t, paths).Runs(t.Context(), a.session.ID)
	if err != nil || len(runs) != 1 || runs[0].Status != task.RunCancelled {
		t.Fatalf("runs=%+v %v", runs, err)
	}
}

func TestMalformedArgumentsRemainRecoverable(t *testing.T) {
	requests := 0
	a, paths := recordedAgent(t, inferenceFunc(func(context.Context, provider.Request, provider.Options, provider.Observer) (provider.Result, error) {
		requests++
		switch requests {
		case 1:
			return provider.Result{Response: callResponse("bad", "list_files", `{"path":`)}, nil
		case 2:
			return provider.Result{Response: callResponse("fixed", "list_files", `{"path":"."}`)}, nil
		default:
			return provider.Result{Response: finishResponse()}, nil
		}
	}), nil)
	if err := a.RunTurn(t.Context(), "inspect"); err != nil {
		t.Fatal(err)
	}
	calls, err := openTaskDB(t, paths).ToolCalls(t.Context(), a.session.ID)
	if err != nil || len(calls) != 1 || calls[0].Status != task.ToolSucceeded || requests != 3 {
		t.Fatalf("calls=%+v requests=%d %v", calls, requests, err)
	}
	events, err := openTaskDB(t, paths).Events(t.Context(), a.session.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range events {
		found = found || e.Kind == "tool.rejected"
	}
	if !found {
		t.Fatal("missing rejection evidence")
	}
}

func TestResolveTypoDoesNotMutateRun(t *testing.T) {
	a, paths := recordedAgent(t, nil, nil)
	e := a.execution
	if err := e.begin(t.Context(), "inspect"); err != nil {
		t.Fatal(err)
	}
	runID := e.run.ID
	if err := e.close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MELDRA_HOME", paths.Home)
	err := runTaskCommand(t.Context(), []string{"resolve", a.session.ID, "missing-call", "--outcome", "failed", "--reason", "typo"}, nil, io.Discard)
	if !errors.Is(err, task.ErrNotFound) {
		t.Fatal(err)
	}
	run, err := openTaskDB(t, paths).GetRun(t.Context(), runID)
	if err != nil || run.Status != task.RunRunning {
		t.Fatalf("run=%+v %v", run, err)
	}
}

func TestGitReviewInterruptedReadRecovers(t *testing.T) {
	a, paths := recordedAgent(t, nil, nil)
	e := a.execution
	if err := e.begin(t.Context(), "inspect"); err != nil {
		t.Fatal(err)
	}
	if err := a.store.Save(a.session); err != nil {
		t.Fatal(err)
	}
	call, err := e.db.PlanTool(t.Context(), e.lease, task.ToolCall{TaskID: a.session.ID, RunID: e.run.ID, Name: "git_review", Arguments: json.RawMessage(`{}`), Effect: task.Command})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.db.StartTool(t.Context(), e.lease, call.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.close(); err != nil {
		t.Fatal(err)
	}
	recovered := recoveryExecution(t, paths, e.workspace.root, a.session.ID)
	if err := recovered.begin(t.Context(), "continue"); err != nil {
		t.Fatal(err)
	}
	if err := recovered.finish(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
}

func TestBeginEventFailureEndsRun(t *testing.T) {
	a, paths := recordedAgent(t, nil, nil)
	_ = openTaskDB(t, paths)
	db, err := sql.Open("sqlite", filepath.Join(taskDirectory(paths), "tasks.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`CREATE TRIGGER reject_turn BEFORE INSERT ON events WHEN json_extract(CAST(NEW.record AS TEXT),'$.kind')='turn.started' BEGIN SELECT RAISE(ABORT,'injected turn event failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.execution.begin(t.Context(), "begin fault"); err == nil {
		t.Fatal("fault not injected")
	}
	runs, err := openTaskDB(t, paths).Runs(t.Context(), a.session.ID)
	if err != nil || len(runs) != 1 || runs[0].Status != task.RunInterrupted {
		t.Fatalf("runs=%+v %v", runs, err)
	}
	if _, err := db.Exec("DROP TRIGGER reject_turn"); err != nil {
		t.Fatal(err)
	}
	if err := a.execution.begin(t.Context(), "retry"); err != nil {
		t.Fatal(err)
	}
	if err := a.execution.finish(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
}

func TestPlanSavedBeforeCancellationKeepsKnownSuccess(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	session := &Session{ID: "test"}
	saver := saveSessionFunc(func(*Session) error { cancel(); return nil })
	registry, err := tool.New(NewSessionTools(session, saver).ToolDefinitions())
	if err != nil {
		t.Fatal(err)
	}
	result, err := registry.Invoke(ctx, "update_plan", json.RawMessage(`{"steps":["saved"]}`))
	if result.Status != tool.Succeeded || err != nil || result.Error != "" || len(session.Plan) != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestCustomCallReplayUsesResponseIdentity(t *testing.T) {
	a, paths := recordedAgent(t, nil, nil)
	a.customProvider = true
	effects := 0
	a.registry, _ = tool.New([]ToolDefinition{{Name: "count", Function: func(context.Context, json.RawMessage) (string, error) { effects++; return "done", nil }}})
	call := []provider.OutputItem{{Type: "function_call", CallID: "call_0", Name: "count", Arguments: `{}`}}
	for _, response := range []string{"response-1", "response-2", "response-2"} {
		if err := a.execution.begin(t.Context(), "count"); err != nil {
			t.Fatal(err)
		}
		a.executeToolCallsContext(t.Context(), call, response)
		if a.execution.err != nil {
			t.Fatal(a.execution.err)
		}
		if err := a.execution.finish(t.Context(), nil); err != nil {
			t.Fatal(err)
		}
	}
	calls, err := openTaskDB(t, paths).ToolCalls(t.Context(), a.session.ID)
	if err != nil || len(calls) != 2 || effects != 2 {
		t.Fatalf("calls=%d effects=%d err=%v", len(calls), effects, err)
	}
}

func TestCustomResponseWithoutIdentityDoesNotRunTools(t *testing.T) {
	effects := 0
	a, _ := recordedAgent(t, inferenceFunc(func(context.Context, provider.Request, provider.Options, provider.Observer) (provider.Result, error) {
		response := callResponse("valid-call", "effect", `{}`)
		response.ID = ""
		return provider.Result{Response: response}, nil
	}), []ToolDefinition{{Name: "effect", Function: func(context.Context, json.RawMessage) (string, error) { effects++; return "ran", nil }}})
	a.customProvider = true
	if err := a.RunTurn(t.Context(), "inspect"); err == nil || effects != 0 {
		t.Fatalf("missing response identity executed: effects=%d error=%v", effects, err)
	}
}

func TestCustomReplayPreservesLegacyProviderScope(t *testing.T) {
	a, paths := recordedAgent(t, nil, nil)
	a.customProvider = true
	effects := 0
	a.registry, _ = tool.New([]ToolDefinition{{Name: "count", Function: func(context.Context, json.RawMessage) (string, error) { effects++; return "done", nil }}})
	call := []provider.OutputItem{{Type: "function_call", CallID: "legacy-call", Name: "count", Arguments: `{}`}}
	for _, endpoint := range []string{"https://example.invalid", "https://example.invalid/v1"} {
		a.execution.config.Provider = endpoint
		if err := a.execution.begin(t.Context(), "count"); err != nil {
			t.Fatal(err)
		}
		a.executeToolCallsContext(t.Context(), call, "same-response")
		if a.execution.err != nil {
			t.Fatal(a.execution.err)
		}
		if err := a.execution.finish(t.Context(), nil); err != nil {
			t.Fatal(err)
		}
	}
	calls, err := openTaskDB(t, paths).ToolCalls(t.Context(), a.session.ID)
	if err != nil || len(calls) != 1 || effects != 1 {
		t.Fatalf("calls=%d effects=%d err=%v", len(calls), effects, err)
	}
}
