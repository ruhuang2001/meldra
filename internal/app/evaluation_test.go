package app

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"meldra/internal/provider"
)

type evaluationStep struct {
	name string
	args any
	want string
}
type evaluationScenario struct {
	name          string
	files         map[string]string
	steps         []evaluationStep
	wantFiles     map[string]string
	absent        []string
	deny          bool
	staleApproval bool
	summary       string
}

// Runtime contract coverage uses a scripted provider without network requests.
// A scripted provider proposes operations, while real tools, approvals, file
// writes and session persistence run and are graded by independent assertions.
func TestOfflineEvaluation(t *testing.T) {
	edit := func(path, old, new string) map[string]any {
		return map[string]any{"path": path, "old_str": old, "new_str": new}
	}
	cases := []evaluationScenario{
		{name: "read_unicode", files: map[string]string{"note.txt": "你好\nworld\n"}, steps: []evaluationStep{{"read_file", map[string]any{"path": "note.txt"}, "你好"}}},
		{name: "list_nested", files: map[string]string{"src/a.txt": "a", "src/b.txt": "b"}, steps: []evaluationStep{{"list_files", map[string]any{}, "src/a.txt"}}},
		{name: "search_literal", files: map[string]string{"a.txt": "first\nneedle.value\n"}, steps: []evaluationStep{{"search_files", map[string]any{"query": "needle.value"}, "a.txt:2:needle.value"}}},
		{name: "edit_existing", files: map[string]string{"a.txt": "before\n"}, steps: []evaluationStep{{"edit_file", edit("a.txt", "before", "after"), "Applied successfully"}}, wantFiles: map[string]string{"a.txt": "after\n"}},
		{name: "create_nested", steps: []evaluationStep{{"edit_file", edit("src/new.txt", "", "created\n"), "Applied successfully"}}, wantFiles: map[string]string{"src/new.txt": "created\n"}},
		{name: "patch_multiple_files", files: map[string]string{"a.txt": "one\n", "b.txt": "two\n"}, steps: []evaluationStep{{"apply_patch", map[string]any{"changes": []any{edit("a.txt", "one", "ONE"), edit("b.txt", "two", "TWO")}}, "Applied successfully"}}, wantFiles: map[string]string{"a.txt": "ONE\n", "b.txt": "TWO\n"}},
		{name: "unified_patch", files: map[string]string{"a.txt": "before\n"}, steps: []evaluationStep{{"apply_patch", map[string]any{"patch": "--- a/a.txt\n+++ b/a.txt\n@@ -1 +1 @@\n-before\n+after\n"}, "Applied successfully"}}, wantFiles: map[string]string{"a.txt": "after\n"}},
		{name: "undo_edit", files: map[string]string{"a.txt": "before\n"}, steps: []evaluationStep{{"edit_file", edit("a.txt", "before", "after"), "Applied successfully"}, {"undo_last_change", map[string]any{}, "Undo successful"}}, wantFiles: map[string]string{"a.txt": "before\n"}},
		{name: "undo_created_directories", steps: []evaluationStep{{"edit_file", edit("nested/new.txt", "", "created"), "Applied successfully"}, {"undo_last_change", map[string]any{}, "Undo successful"}}, absent: []string{"nested"}},
		{name: "deny_edit", deny: true, files: map[string]string{"a.txt": "before\n"}, steps: []evaluationStep{{"edit_file", edit("a.txt", "before", "after"), "Declined"}}, wantFiles: map[string]string{"a.txt": "before\n"}},
		{name: "deny_command", deny: true, steps: []evaluationStep{{"run_command", map[string]any{"command": "go", "args": []string{"test", "./..."}}, "Declined"}}},
		{name: "reject_parent_escape", steps: []evaluationStep{{"edit_file", edit("../outside.txt", "", "escape"), "path escapes workspace"}}},
		{name: "reject_git_access", steps: []evaluationStep{{"read_file", map[string]any{"path": ".git/config"}, "access to .git is forbidden"}}},
		{name: "reject_missing_parameter", steps: []evaluationStep{{"read_file", map[string]any{}, "missing required parameter"}}},
		{name: "reject_ambiguous_edit", files: map[string]string{"a.txt": "same same"}, steps: []evaluationStep{{"edit_file", edit("a.txt", "same", "new"), "old_str must occur exactly once (found 2)"}}, wantFiles: map[string]string{"a.txt": "same same"}},
		{name: "reject_partial_patch", files: map[string]string{"a.txt": "one", "b.txt": "two"}, steps: []evaluationStep{{"apply_patch", map[string]any{"changes": []any{edit("a.txt", "one", "ONE"), edit("b.txt", "missing", "TWO")}}, "old_str must occur exactly once (found 0)"}}, wantFiles: map[string]string{"a.txt": "one", "b.txt": "two"}},
		{name: "reject_stale_approval", staleApproval: true, files: map[string]string{"a.txt": "before"}, steps: []evaluationStep{{"edit_file", edit("a.txt", "before", "after"), "changed since diff was prepared"}}, wantFiles: map[string]string{"a.txt": "external update"}},
		{name: "session_plan_summary", steps: []evaluationStep{{"update_plan", map[string]any{"steps": []string{"inspect", "verify"}}, "inspect"}, {"save_summary", map[string]any{"summary": "verified milestone"}, "Session summary saved"}, {"session_status", map[string]any{}, "verified milestone"}}, summary: "verified milestone"},
	}
	for _, scenario := range cases {
		t.Run(scenario.name, func(t *testing.T) { runEvaluationScenario(t, scenario) })
	}
}

func runEvaluationScenario(t *testing.T, scenario evaluationScenario) {
	t.Helper()
	root := t.TempDir()
	for name, text := range scenario.files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	workspace, err := NewWorkspace(root, bufio.NewReader(strings.NewReader("n\n")), io.Discard, !scenario.deny && !scenario.staleApproval)
	if err != nil {
		t.Fatal(err)
	}
	if scenario.staleApproval {
		workspace.SetApprovalFunc(func(context.Context, ApprovalRequest) bool {
			if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("external update"), 0o644); err != nil {
				t.Fatal(err)
			}
			return true
		})
	}
	paths, err := ConfigPathsForHome(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	store := NewSessionStore(paths)
	session, err := store.New(root)
	if err != nil {
		t.Fatal(err)
	}
	definitions := append(workspace.ToolDefinitions(), NewSessionTools(session, store).ToolDefinitions()...)
	var results []string
	for i := range definitions {
		handler := definitions[i].Function
		definitions[i].Function = func(ctx context.Context, input json.RawMessage) (string, error) {
			result, err := handler(ctx, input)
			captured := result
			if err != nil {
				captured = "Error: " + err.Error()
			}
			results = append(results, captured)
			return result, err
		}
	}
	requests := 0
	backend := inferenceFunc(func(ctx context.Context, request provider.Request, _ provider.Options, _ provider.Observer) (provider.Result, error) {
		index := requests
		requests++
		if index > len(scenario.steps) {
			t.Fatal("unexpected extra inference")
		}
		if index > 0 && request.PreviousResponseID != fmt.Sprintf("step-%d", index-1) {
			t.Fatal("tool continuation lost")
		}
		response := &provider.Response{ID: fmt.Sprintf("step-%d", index), Status: "completed"}
		if index == len(scenario.steps) {
			response.Text = "scenario complete"
		} else {
			step := scenario.steps[index]
			args, err := json.Marshal(step.args)
			if err != nil {
				t.Fatal(err)
			}
			response.Output = []provider.OutputItem{{Type: "function_call", CallID: fmt.Sprint(index), Name: step.name, Arguments: string(args)}}
		}
		return provider.Result{Response: response}, nil
	})
	agent := NewAgent(backend, nil, definitions)
	agent.output = io.Discard
	agent.session = session
	agent.store = store
	if err := agent.RunTurn(t.Context(), "offline scenario: "+scenario.name); err != nil {
		t.Fatal(err)
	}
	if len(results) != len(scenario.steps) {
		t.Fatalf("results = %d, want %d", len(results), len(scenario.steps))
	}
	for i, step := range scenario.steps {
		if !strings.Contains(results[i], step.want) {
			t.Errorf("%s result %q missing %q", step.name, results[i], step.want)
		}
	}
	for name, want := range scenario.wantFiles {
		got, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want %q", name, got, err, want)
		}
	}
	for _, name := range scenario.absent {
		if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Errorf("%s should not exist: %v", name, err)
		}
	}
	loaded, err := store.Load(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.resumed || len(loaded.Messages) != 2 || loaded.Messages[1].Content != "scenario complete" {
		t.Fatal("completed session did not survive reload")
	}
	if loaded.Summary != scenario.summary {
		t.Fatalf("summary = %q, want %q", loaded.Summary, scenario.summary)
	}
}
