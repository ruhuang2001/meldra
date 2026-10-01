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
	"strings"
	"testing"

	"meldra/internal/provider"
	"meldra/internal/task"
)

func TestTaskCommandsInspectWithoutModelOrExecution(t *testing.T) {
	calls := 0
	agent, paths := recordedAgent(t, inferenceFunc(func(context.Context, provider.Request, provider.Options, provider.Observer) (provider.Result, error) {
		calls++
		return provider.Result{Response: finishResponse()}, nil
	}), nil)
	if err := agent.RunTurn(t.Context(), "original task"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MELDRA_HOME", paths.Home)
	t.Setenv("OPENAI_API_KEY", "")
	for _, args := range [][]string{{"tasks", "--json"}, {"task", "show", agent.session.ID, "--json"}, {"task", "events", agent.session.ID, "--json"}} {
		var out bytes.Buffer
		if err := runCLIContext(t.Context(), args, strings.NewReader(""), &out); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		decoder := json.NewDecoder(&out)
		records := 0
		for {
			var data any
			err := decoder.Decode(&data)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("mixed JSON/log output: %v", err)
			}
			records++
		}
		if records == 0 {
			t.Fatal("no JSON records")
		}
	}
	if calls != 1 {
		t.Fatal("inspection invoked inference")
	}
	var out bytes.Buffer
	if err := runTaskCommand(t.Context(), []string{"events", agent.session.ID, "--json", "--after", "99999"}, nil, &out); err != nil || out.Len() != 0 {
		t.Fatalf("cursor output=%q err=%v", out.String(), err)
	}
}

func TestTaskListEmptyDoesNotCreateDatabase(t *testing.T) {
	paths, err := ConfigPathsForHome(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MELDRA_HOME", paths.Home)
	var out bytes.Buffer
	if err := runTasksCommand(t.Context(), []string{"--json"}, &out); err != nil || out.String() != "[]\n" {
		t.Fatalf("output=%q err=%v", out.String(), err)
	}
	if _, err := os.Stat(taskDirectory(paths)); !os.IsNotExist(err) {
		t.Fatalf("inspection created store: %v", err)
	}
}

func TestLegacyTaskImportPreservesOriginalAndDeduplicates(t *testing.T) {
	paths, err := ConfigPathsForHome(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	legacy := NewSessionStore(paths)
	root := t.TempDir()
	session, err := legacy.New(root)
	if err != nil {
		t.Fatal(err)
	}
	session.appendMessage("user", "original request")
	session.appendMessage("assistant", "first answer")
	if err := legacy.Save(session); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(legacy.path(session.ID))
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		runtime, err := newChatRuntime(t.Context(), paths, Settings{Model: "test", BaseURL: defaultBaseURL}, ChatOptions{Resume: session.ID}, func(root string, approve bool) (*Workspace, error) {
			return NewWorkspace(root, bufio.NewReader(strings.NewReader("")), io.Discard, true)
		}, nil, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		runtime.agent.backend = inferenceFunc(func(context.Context, provider.Request, provider.Options, provider.Observer) (provider.Result, error) {
			return provider.Result{Response: finishResponse()}, nil
		})
		if err := runtime.agent.RunTurn(t.Context(), "continue"); err != nil {
			t.Fatal(err)
		}
	}
	unchanged, err := os.ReadFile(legacy.path(session.ID))
	if err != nil || !bytes.Equal(original, unchanged) {
		t.Fatal("legacy source changed")
	}
	db := openTaskDB(t, paths)
	tasks, err := db.ListTasks(t.Context(), "", 100)
	if err != nil || len(tasks) != 1 || !tasks[0].LegacyHistoryMissing || tasks[0].Goal != "original request" {
		t.Fatalf("import=%+v %v", tasks, err)
	}
	snapshots, err := legacy.List()
	if err != nil || len(snapshots) != 1 {
		t.Fatalf("session list duplicates=%d err=%v", len(snapshots), err)
	}
	loaded, err := legacy.Load(session.ID)
	if err != nil || !loaded.taskSnapshot || len(loaded.Messages) != 6 {
		t.Fatalf("loaded=%+v err=%v", loaded, err)
	}
}

func TestResolveUnknownClearsStaleFailureAndDoesNotExecute(t *testing.T) {
	agent, paths := recordedAgent(t, inferenceFunc(func(context.Context, provider.Request, provider.Options, provider.Observer) (provider.Result, error) {
		t.Fatal("resolve ran inference")
		return provider.Result{}, nil
	}), nil)
	e := agent.execution
	if err := e.begin(t.Context(), "task"); err != nil {
		t.Fatal(err)
	}
	call, err := e.db.PlanTool(t.Context(), e.lease, task.ToolCall{TaskID: e.session.ID, RunID: e.run.ID, Name: "run_command", Arguments: json.RawMessage(`{}`), Effect: task.Command})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.db.StartTool(t.Context(), e.lease, call.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.db.FinishTool(t.Context(), e.lease, call.ID, task.Result{Status: task.ToolUnknown, Error: "old error", ExitCode: new(-1)}); err != nil {
		t.Fatal(err)
	}
	if err := e.finish(t.Context(), errors.New("interrupted")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MELDRA_HOME", paths.Home)
	var out bytes.Buffer
	if err := runTaskCommand(t.Context(), []string{"resolve", e.session.ID, call.ID, "--outcome", "succeeded", "--reason", "verified external output"}, nil, &out); err != nil {
		t.Fatal(err)
	}
	db := openTaskDB(t, paths)
	resolved, err := db.GetToolCall(t.Context(), call.ID)
	if err != nil || resolved.Result.Error != "" || resolved.Result.ExitCode != nil || resolved.Status != task.ToolSucceeded {
		t.Fatalf("resolved=%+v err=%v", resolved, err)
	}
}

func TestCorruptTaskSnapshotDoesNotAdvertiseLegacyFallback(t *testing.T) {
	paths, err := ConfigPathsForHome(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	legacy := NewSessionStore(paths)
	session, err := legacy.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session.appendMessage("user", "request")
	if err := legacy.Save(session); err != nil {
		t.Fatal(err)
	}
	active := newTaskSessionStore(paths)
	if err := active.ensureDir(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(active.path(session.ID), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	sessions, diagnostics, err := legacy.ListWithDiagnostics()
	if err != nil || len(sessions) != 0 || diagnostics.SkippedFiles != 1 {
		t.Fatalf("list=%v diagnostics=%+v error=%v", sessions, diagnostics, err)
	}
	if _, err := legacy.Load(session.ID); err == nil {
		t.Fatal("corrupt active snapshot hidden by legacy source")
	}
}

func TestLegacyImportRejectsChangedSourceAfterLoad(t *testing.T) {
	paths, err := ConfigPathsForHome(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	legacy := NewSessionStore(paths)
	session, err := legacy.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session.appendMessage("user", "request")
	if err := legacy.Save(session); err != nil {
		t.Fatal(err)
	}
	runtime, err := newChatRuntime(t.Context(), paths, Settings{Model: "test", BaseURL: defaultBaseURL}, ChatOptions{Resume: session.ID}, func(root string, approve bool) (*Workspace, error) {
		return NewWorkspace(root, bufio.NewReader(strings.NewReader("")), io.Discard, true)
	}, nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	session.appendMessage("user", "concurrent edit")
	if err := legacy.Save(session); err != nil {
		t.Fatal(err)
	}
	if err := runtime.agent.RunTurn(t.Context(), "continue"); err == nil || !strings.Contains(err.Error(), "changed after loading") {
		t.Fatalf("import = %v", err)
	}
}
