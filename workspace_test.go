package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func testWorkspace(t *testing.T, root, answers string, approve bool) (*Workspace, *bytes.Buffer) {
	t.Helper()
	var out bytes.Buffer
	w, err := NewWorkspace(root, bufio.NewReader(strings.NewReader(answers)), &out, approve)
	if err != nil {
		t.Fatal(err)
	}
	return w, &out
}

func callTool(t *testing.T, w *Workspace, name string, in any) (string, error) {
	t.Helper()
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range w.ToolDefinitions() {
		if tool.Name == name {
			return tool.Function(b)
		}
	}
	t.Fatalf("tool %s not found", name)
	return "", nil
}

func TestWorkspacePathContainment(t *testing.T) {
	root := t.TempDir()
	w, _ := testWorkspace(t, root, "", true)
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"../outside", outside, ".git/config"} {
		if _, err := callTool(t, w, "read_file", map[string]any{"path": path}); err == nil {
			t.Errorf("read_file accepted forbidden path %q", path)
		}
	}
	if runtime.GOOS != "windows" {
		link := filepath.Join(root, "escape")
		if err := os.Symlink(filepath.Dir(outside), link); err != nil {
			t.Fatal(err)
		}
		if _, err := callTool(t, w, "read_file", map[string]any{"path": "escape/outside"}); err == nil {
			t.Error("read_file followed an escaping symlink")
		}
		if _, err := callTool(t, w, "edit_file", map[string]any{"path": "escape/new", "old_str": "", "new_str": "x"}); err == nil {
			t.Error("edit_file accepted a missing target beneath an escaping symlink")
		}
		if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(root, ".git"), filepath.Join(root, "git-alias")); err != nil {
			t.Fatal(err)
		}
		if _, err := callTool(t, w, "edit_file", map[string]any{"path": "git-alias/new", "old_str": "", "new_str": "x"}); err == nil {
			t.Error("edit_file accepted a missing target through a symlink into .git")
		}
	}
}

func TestWorkspaceProtectsMeldraConfiguration(t *testing.T) {
	root := t.TempDir()
	configHome := filepath.Join(root, ".meldra")
	if err := os.Mkdir(configHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configHome, "credentials.env"), []byte("OPENAI_API_KEY=secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, _ := testWorkspace(t, root, "", true)
	if err := w.ProtectPath(configHome); err != nil {
		t.Fatal(err)
	}
	if _, err := callTool(t, w, "read_file", map[string]any{"path": ".meldra/credentials.env"}); err == nil {
		t.Fatal("read_file exposed Meldra credentials")
	}
	if _, err := w.resolve(".MELDRA/credentials.env", false); err == nil {
		t.Fatal("case-variant Meldra config path bypassed protection")
	}
	listing, err := callTool(t, w, "list_files", map[string]any{"path": ""})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(listing, "credentials.env") {
		t.Fatalf("list_files exposed protected files: %s", listing)
	}
}

func TestWorkspaceReadSearchAndConfirmation(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "a.txt")
	if err := os.WriteFile(p, []byte("one\ntwo needle\nthree needle\nfour"), 0o640); err != nil {
		t.Fatal(err)
	}
	w, _ := testWorkspace(t, root, "n\n", false)
	got, err := callTool(t, w, "read_file", map[string]any{"path": "a.txt", "offset": 2, "limit": 2})
	if err != nil || !strings.Contains(got, "2\ttwo") || !strings.Contains(got, "truncated=true") {
		t.Fatalf("chunk read = %q, %v", got, err)
	}
	got, err = callTool(t, w, "search_files", map[string]any{"query": "needle", "max_results": 1})
	if err != nil || strings.Count(got, "a.txt:") != 1 || !strings.Contains(got, "truncated") {
		t.Fatalf("bounded search = %q, %v", got, err)
	}
	if _, err = callTool(t, w, "edit_file", map[string]any{"path": "a.txt", "old_str": "one", "new_str": "ONE"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if strings.HasPrefix(string(b), "ONE") {
		t.Fatal("declined edit changed file")
	}
}

func TestWorkspaceConfirmationHandlesTerminalPasteAndInvalidAnswers(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "created.txt")
	w, output := testWorkspace(t, root, "maybe\n\x1b[200~y\x1b[201~\n", false)
	result, err := callTool(t, w, "edit_file", map[string]any{
		"path": "created.txt", "old_str": "", "new_str": "created\n",
	})
	if err != nil || !strings.Contains(result, "Applied successfully") {
		t.Fatalf("edit result = %q, %v", result, err)
	}
	if contents, err := os.ReadFile(path); err != nil || string(contents) != "created\n" {
		t.Fatalf("created file = %q, %v", contents, err)
	}
	if !strings.Contains(output.String(), "Please enter y or n:") {
		t.Fatalf("invalid answer was not reprompted: %q", output.String())
	}
}

func TestWorkspaceAutoApprovalStillPrintsChanges(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "target.txt")
	if err := os.WriteFile(path, []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	workspace, output := testWorkspace(t, root, "", true)
	result, err := callTool(t, workspace, "edit_file", map[string]any{
		"path": "target.txt", "old_str": "before", "new_str": "after",
	})
	if err != nil || !strings.Contains(result, "Applied successfully") {
		t.Fatalf("edit result = %q, %v", result, err)
	}
	if got := output.String(); !strings.Contains(got, "--- a/target.txt") || !strings.Contains(got, "+after") {
		t.Fatalf("auto-approved edit did not print its diff: %q", got)
	}
}

func TestWorkspaceReadAndSearchValidation(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "visible.go"), []byte("package visible\n// needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ignored.txt"), []byte("needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "binary.go"), []byte("prefix\x00needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w, _ := testWorkspace(t, root, "", true)

	for name, input := range map[string]map[string]any{
		"zero offset":      {"path": "visible.go", "offset": -1, "limit": 1},
		"negative limit":   {"path": "visible.go", "offset": 1, "limit": -1},
		"empty query":      {"query": "", "path": ""},
		"bad result limit": {"query": "needle", "max_results": -1},
		"invalid glob":     {"query": "needle", "glob": "["},
	} {
		t.Run(name, func(t *testing.T) {
			tool := "read_file"
			if _, ok := input["query"]; ok {
				tool = "search_files"
			}
			if _, err := callTool(t, w, tool, input); err == nil {
				t.Fatalf("%s accepted invalid input %#v", tool, input)
			}
		})
	}

	result, err := callTool(t, w, "search_files", map[string]any{
		"query": "needle", "path": "", "glob": "*.go", "max_results": 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "visible.go:2") || strings.Contains(result, "ignored.txt") || strings.Contains(result, "binary.go") {
		t.Fatalf("glob/binary filtering result = %q", result)
	}
}

func TestWorkspaceApplyAndUndo(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a"), []byte("A"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, _ := testWorkspace(t, root, "", true)
	_, err := callTool(t, w, "apply_patch", map[string]any{"changes": []map[string]any{
		{"path": "a", "old_str": "A", "new_str": "AA"},
		{"path": "b", "old_str": "", "new_str": "B"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "a")); string(b) != "AA" {
		t.Fatalf("a = %q", b)
	}
	if _, err := callTool(t, w, "undo_last_change", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "a")); string(b) != "A" {
		t.Fatalf("undo a = %q", b)
	}
	if _, err := os.Stat(filepath.Join(root, "b")); !os.IsNotExist(err) {
		t.Fatal("undo did not remove newly created file")
	}
}

func TestWorkspaceAppliesUnifiedMultiFilePatchAndUndoesIt(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("one\ntwo\nthree\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	w, _ := testWorkspace(t, root, "", true)
	patch := `--- a/a.txt
+++ b/a.txt
@@ -1,3 +1,3 @@
 one
-two
+TWO
 three
--- /dev/null
+++ b/new.txt
@@ -0,0 +1,2 @@
+new
+file
`
	if _, err := callTool(t, w, "apply_patch", map[string]any{"patch": patch, "changes": nil}); err != nil {
		t.Fatal(err)
	}
	if contents, _ := os.ReadFile(filepath.Join(root, "a.txt")); string(contents) != "one\nTWO\nthree\n" {
		t.Fatalf("a.txt = %q", contents)
	}
	if contents, _ := os.ReadFile(filepath.Join(root, "new.txt")); string(contents) != "new\nfile\n" {
		t.Fatalf("new.txt = %q", contents)
	}
	if _, err := callTool(t, w, "undo_last_change", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if contents, _ := os.ReadFile(filepath.Join(root, "a.txt")); string(contents) != "one\ntwo\nthree\n" {
		t.Fatalf("undo a.txt = %q", contents)
	}
	if _, err := os.Stat(filepath.Join(root, "new.txt")); !os.IsNotExist(err) {
		t.Fatal("undo did not remove patch-created file")
	}
}

func BenchmarkParseUnifiedPatch(b *testing.B) {
	filePatch := "--- a/file.txt\n+++ b/file.txt\n@@ -1,3 +1,3 @@\n first\n-old\n+new\n last\n"
	for _, benchmark := range []struct {
		name  string
		patch string
		files int
	}{
		{name: "single-file", patch: filePatch, files: 1},
		{name: "25-files", patch: strings.Repeat(filePatch, 25), files: 25},
	} {
		b.Run(benchmark.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				files, err := parseUnifiedPatch(benchmark.patch)
				if err != nil || len(files) != benchmark.files {
					b.Fatalf("parseUnifiedPatch() = %d files, %v", len(files), err)
				}
			}
		})
	}
}

func TestWorkspaceAppliesFullDeletionAndUndoesIt(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "delete.txt")
	if err := os.WriteFile(path, []byte("one\ntwo\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	w, _ := testWorkspace(t, root, "", true)
	patch := "--- a/delete.txt\n+++ /dev/null\n@@ -1,2 +0,0 @@\n-one\n-two\n"
	if _, err := callTool(t, w, "apply_patch", map[string]any{"patch": patch, "changes": nil}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("deleted file still exists: %v", err)
	}
	if _, err := callTool(t, w, "undo_last_change", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil || string(contents) != "one\ntwo\n" {
		t.Fatalf("restored contents = %q, error = %v", contents, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("restored mode = %v, error = %v", info.Mode().Perm(), err)
	}
}

func TestWorkspaceToolSchemasRequireEveryProperty(t *testing.T) {
	w, _ := testWorkspace(t, t.TempDir(), "", true)
	for _, tool := range w.ToolDefinitions() {
		properties, _ := tool.Parameters["properties"].(map[string]any)
		required, _ := tool.Parameters["required"].([]string)
		if len(properties) != len(required) {
			t.Errorf("tool %s has %d properties but %d required fields", tool.Name, len(properties), len(required))
		}
	}
}

type mutateOnRead struct {
	mutate func()
	read   bool
}

func (r *mutateOnRead) Read(buffer []byte) (int, error) {
	if !r.read {
		r.read = true
		r.mutate()
		return copy(buffer, "y\n"), nil
	}
	return 0, io.EOF
}

func TestWorkspaceRefusesEditChangedDuringConfirmation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "changing.txt")
	if err := os.WriteFile(path, []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(&mutateOnRead{mutate: func() {
		if err := os.WriteFile(path, []byte("user edit\n"), 0o644); err != nil {
			t.Error(err)
		}
	}})
	w, err := NewWorkspace(root, reader, io.Discard, false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = callTool(t, w, "edit_file", map[string]any{"path": "changing.txt", "old_str": "before", "new_str": "meldra"})
	if err == nil || !strings.Contains(err.Error(), "changed since diff") {
		t.Fatalf("edit error = %v", err)
	}
	if contents, _ := os.ReadFile(path); string(contents) != "user edit\n" {
		t.Fatalf("user edit was overwritten: %q", contents)
	}
}

func TestWorkspaceRejectsAmbiguousPatchSemantics(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("first\nsecond\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w, _ := testWorkspace(t, root, "", true)
	patches := []string{
		"--- a/a.txt\n+++ /dev/null\n@@ -1,1 +0,0 @@\n-first\n",
		"--- a/a.txt\n+++ b/renamed.txt\n@@ -1,1 +1,1 @@\n-first\n+FIRST\n",
		"--- a/a.txt\n+++ b/a.txt\n@@ -1,1 +1,1 @@\n-first\n+FIRST\n\\ No newline at end of file\n",
	}
	for _, patch := range patches {
		if _, err := callTool(t, w, "apply_patch", map[string]any{"patch": patch, "changes": nil}); err == nil {
			t.Errorf("accepted ambiguous patch:\n%s", patch)
		}
	}
}

func TestWorkspacePatchInputAndUndoConflictValidation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "a.txt")
	if err := os.WriteFile(path, []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w, _ := testWorkspace(t, root, "", true)

	invalidInputs := []map[string]any{
		{"patch": nil, "changes": nil},
		{"patch": "--- a/a.txt\n+++ b/a.txt\n@@ -1,1 +1,1 @@\n-before\n+after\n", "changes": []map[string]any{{"path": "a.txt", "old_str": "before", "new_str": "after"}}},
		{"patch": nil, "changes": []map[string]any{{"path": "a.txt", "old_str": "before", "new_str": "after"}, {"path": "a.txt", "old_str": "before", "new_str": "other"}}},
	}
	for _, input := range invalidInputs {
		if _, err := callTool(t, w, "apply_patch", input); err == nil {
			t.Errorf("accepted invalid apply_patch input %#v", input)
		}
	}

	if _, err := callTool(t, w, "edit_file", map[string]any{"path": "a.txt", "old_str": "before", "new_str": "after"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("user changed it\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := callTool(t, w, "undo_last_change", map[string]any{}); err == nil || !strings.Contains(err.Error(), "changed since") {
		t.Fatalf("undo conflict error = %v", err)
	}
	if contents, _ := os.ReadFile(path); string(contents) != "user changed it\n" {
		t.Fatalf("conflicting undo overwrote file: %q", contents)
	}
}

func TestWorkspaceCommandPolicyAndNonzeroOutput(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "must-not-reach-child")
	w, _ := testWorkspace(t, t.TempDir(), "", true)
	if _, err := callTool(t, w, "run_command", map[string]any{"command": "sh", "args": []string{"-c", "true"}}); err == nil {
		t.Fatal("shell was accepted")
	}
	if _, err := callTool(t, w, "run_command", map[string]any{"command": "go", "args": []string{"test"}, "timeout": 121}); err == nil {
		t.Fatal("oversized timeout was accepted")
	}
	got, err := callTool(t, w, "run_command", map[string]any{"command": "go", "args": []string{"test", "./..."}})
	if err != nil || !strings.Contains(got, "status: 1") || !strings.Contains(got, "does not contain main module") {
		t.Fatalf("nonzero command = %q, %v", got, err)
	}
	for _, variable := range safeCommandEnvironment(t.TempDir(), t.TempDir()) {
		if strings.HasPrefix(variable, "OPENAI_API_KEY=") {
			t.Fatal("child environment contains OPENAI_API_KEY")
		}
	}
	if _, err := callTool(t, w, "run_command", map[string]any{"command": "go", "args": []string{"test", "-cpuprofile=/tmp/leak", "."}}); err == nil {
		t.Fatal("accepted a path-valued output flag")
	}
}

func TestWorkspaceExecutableCommandRequiresConfirmation(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.invalid/approval\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "approval.go"), []byte("package approval\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	declined, declinedOutput := testWorkspace(t, root, "n\n", false)
	result, err := callTool(t, declined, "run_command", map[string]any{
		"command": "go", "args": []string{"test", "./..."}, "timeout": 30,
	})
	if err != nil || result != "Declined; command not run." {
		t.Fatalf("declined command = %q, %v", result, err)
	}
	if got := declinedOutput.String(); got != `Run command with OS-user privileges? go "test" "./..." [y/N] ` {
		t.Fatalf("confirmation output = %q", declinedOutput.String())
	}

	approved, _ := testWorkspace(t, root, "yes\n", false)
	result, err = callTool(t, approved, "run_command", map[string]any{
		"command": "go", "args": []string{"test", "./..."}, "timeout": 30,
	})
	if err != nil || !strings.Contains(result, "status: 0") {
		t.Fatalf("approved command = %q, %v", result, err)
	}
}

func TestWorkspaceCommandUsesParentCancellation(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.invalid/cancel\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, "cmd", "sleeper")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	program := "package main\n\nimport \"time\"\n\nfunc main() { time.Sleep(30 * time.Second) }\n"
	if err := os.WriteFile(filepath.Join(directory, "main.go"), []byte(program), 0o644); err != nil {
		t.Fatal(err)
	}
	w, _ := testWorkspace(t, root, "", true)
	ctx, cancel := context.WithCancel(context.Background())
	timer := time.AfterFunc(200*time.Millisecond, cancel)
	defer timer.Stop()
	w.SetContext(ctx)

	started := time.Now()
	result, err := callTool(t, w, "run_command", map[string]any{
		"command": "go", "args": []string{"run", "./cmd/sleeper"}, "timeout": 30,
	})
	if err != nil || !strings.Contains(result, "status: -1") || !strings.Contains(result, "[cancelled]") {
		t.Fatalf("cancelled command = %q, %v", result, err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("command cancellation took %s", elapsed)
	}
}

func TestWorkspaceCommandAllowlistBoundaries(t *testing.T) {
	tests := []struct {
		command string
		args    []string
		allowed bool
	}{
		{"python3", []string{"./hello.py"}, true},
		{"python3", []string{"./hello.py", "extra"}, false},
		{"python3", []string{"../hello.py"}, false},
		{"python3", []string{"./hello.txt"}, false},
		{"python3", []string{"-m", "pytest"}, true},
		{"python3", []string{"-m", "pytest", "-v", "tests/test_foo.py"}, true},
		{"python3", []string{"-m", "pytest", "tests/test_foo.py::TestThing::test_case[param]"}, true},
		{"python3", []string{"-m", "pytest", "@args.txt"}, false},
		{"python3", []string{"-m", "pytest", "-k=foo and bar"}, true},
		{"python3", []string{"-m", "pytest", "--override-ini=cache_dir=.pytest-cache"}, true},
		{"python3", []string{"-m", "pytest", "--override-ini=console_output_style=classic"}, true},
		{"python3", []string{"-m", "pytest", "-c", "custom.cfg"}, false},
		{"python3", []string{"-m", "pytest", "../outside.py"}, false},
		{"python3", []string{"-m", "pytest", "--override-ini=cache_dir=/tmp/pytest-cache"}, false},
		{"python3", []string{"-m", "pytest", "--override-ini=cache_dir"}, false},
		{"python3", []string{"-m", "pytest", "--override-ini=addopts=-q"}, false},
		{"python3", []string{"-m", "pytest", "--tb=short", "-x", "test/"}, true},
		{"python3", []string{"-m", "pytest", "--co=pytest_collect", "tests/"}, false},
		{"go", []string{"test", "-race", "-run=TestOne", "./..."}, true},
		{"go", []string{"test", "-run=TestOne\nspoof", "./..."}, false},
		{"go", []string{"vet", "./..."}, true},
		{"go", []string{"build", "-trimpath", "./..."}, true},
		{"go", []string{"run", "./cmd/hello"}, true},
		{"go", []string{"run", "./hello.go"}, true},
		{"go", []string{"run", "./..."}, false},
		{"go", []string{"run", "./../outside.go"}, false},
		{"go", []string{"run", "https://example.test/program.go"}, false},
		{"go", []string{"test", "-coverprofile=coverage.out", "./..."}, false},
		{"npm", []string{"test"}, true},
		{"npm", []string{"run", "check"}, true},
		{"npm", []string{"install"}, false},
		{"npm", []string{"run", "prepare"}, false},
		{"pnpm", []string{"run", "format:check"}, true},
		{"pnpm", []string{"--dir", "../outside", "test"}, false},
		{"cargo", []string{"test"}, true},
		{"cargo", []string{"fmt", "--", "--check"}, true},
		{"cargo", []string{"run"}, false},
		{"cargo", []string{"test", "--manifest-path", "../Cargo.toml"}, false},
		{"gofmt", []string{"-w", "main.go"}, false},
		{"gofmt", []string{"-d", "-cpuprofile=profile.out", "main.go"}, false},
		{"gofmt", []string{"-d", "../outside.go"}, false},
		{"make", []string{"check"}, true},
		{"make", []string{"install"}, false},
		{"git", []string{"status", "--short"}, true},
		{"git", []string{"diff", "--cached", "--"}, true},
		{"git", []string{"log", "-5", "--oneline"}, true},
		{"git", []string{"show", "HEAD"}, false},
		{"git", []string{"diff", "--ext-diff"}, false},
		{"/usr/bin/git", []string{"status"}, false},
	}
	for _, test := range tests {
		name := test.command + " " + strings.Join(test.args, " ")
		t.Run(name, func(t *testing.T) {
			if got := allowed(test.command, test.args); got != test.allowed {
				t.Fatalf("allowed() = %t, want %t", got, test.allowed)
			}
		})
	}
}

func TestWorkspaceValidatesPytestPaths(t *testing.T) {
	root := t.TempDir()
	for _, directory := range []string{"tests", "src", "reports", "logs"} {
		if err := os.MkdirAll(filepath.Join(root, directory), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "tests", "test_sample.py"), []byte("def test_sample(): pass\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pytest.ini"), []byte("[pytest]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w, _ := testWorkspace(t, root, "", true)

	for _, args := range [][]string{
		{"tests/test_sample.py::TestSample::test_case[param]"},
		{"--ignore=tests/test_sample.py"},
		{"--rootdir=."},
		{"--config-file=pytest.ini"},
		{"--pythonpath=src"},
		{"--junitxml=reports/results.xml"},
		{"--log-file=logs/pytest.log"},
		{"--override-ini=cache_dir=.pytest-cache"},
		{"--override-ini=pythonpath=src"},
		{"--override-ini=testpaths=tests"},
		{"--override-ini=log_file=logs/pytest.log"},
	} {
		if err := w.validatePytestArgs(args); err != nil {
			t.Fatalf("validatePytestArgs(%q) = %v", args, err)
		}
	}

	for _, args := range [][]string{
		{"--junitxml=~/.meldra/credentials.env"},
		{"--junitxml=$HOME/.meldra/credentials.env"},
		{"--junitxml=%USERPROFILE%/.meldra/credentials.env"},
		{"--log-file=~/.meldra/pytest.log"},
		{"--override-ini=cache_dir=$HOME/.meldra/pytest-cache"},
		{"--override-ini=log_file=%USERPROFILE%/.meldra/pytest.log"},
		{"--override-ini=pythonpath=~/src"},
		{"--override-ini=testpaths=$HOME/tests"},
	} {
		t.Run("reject expansion "+strings.Join(args, " "), func(t *testing.T) {
			if _, err := w.execute("python3", append([]string{"-m", "pytest"}, args...), 30); err == nil {
				t.Fatalf("pytest path expansion was accepted: %q", args)
			}
		})
	}

	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "test_outside.py"), []byte("def test_outside(): pass\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "pytest.ini"), []byte("[pytest]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{
		{"escape/test_outside.py::test_outside"},
		{"--ignore=escape/test_outside.py"},
		{"--rootdir=escape"},
		{"--config-file=escape/pytest.ini"},
		{"--pythonpath=escape"},
		{"--junitxml=escape/results.xml"},
		{"--log-file=escape/pytest.log"},
		{"--override-ini=cache_dir=escape/.pytest-cache"},
		{"--override-ini=pythonpath=escape"},
		{"--override-ini=testpaths=escape"},
		{"--override-ini=log_file=escape/pytest.log"},
		{"--override-ini=cache_dir=/tmp/pytest-cache"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			if _, err := w.execute("python3", append([]string{"-m", "pytest"}, args...), 30); err == nil {
				t.Fatalf("pytest path escape was accepted: %q", args)
			}
		})
	}
}

func TestWorkspaceCommandApprovalClassification(t *testing.T) {
	for command, want := range map[string]bool{
		"python3": true, "go": true, "npm": true, "pnpm": true, "cargo": true, "make": true, "gofmt": false, "git": false,
	} {
		if got := commandRequiresApproval(command); got != want {
			t.Errorf("commandRequiresApproval(%q) = %t, want %t", command, got, want)
		}
	}
}

func TestWorkspaceRejectsAllowlistedExecutableFromWorkspace(t *testing.T) {
	root := t.TempDir()
	fakeGit := filepath.Join(root, "git")
	if err := os.WriteFile(fakeGit, []byte("#!/bin/sh\necho unexpected\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	w, _ := testWorkspace(t, root, "", true)
	if _, err := callTool(t, w, "run_command", map[string]any{
		"command": "git", "args": []string{"status", "--short"}, "timeout": 30,
	}); err == nil || !strings.Contains(err.Error(), "inside the workspace") {
		t.Fatalf("workspace executable error = %v", err)
	}
}

func TestWorkspaceRejectsExecutableFromExternalProtectedStorage(t *testing.T) {
	root := t.TempDir()
	protected := t.TempDir()
	fakeGofmt := filepath.Join(protected, "gofmt")
	if err := os.WriteFile(fakeGofmt, []byte("#!/bin/sh\necho unexpected\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", protected+string(os.PathListSeparator)+os.Getenv("PATH"))
	w, _ := testWorkspace(t, root, "", true)
	if err := w.ProtectPath(protected); err != nil {
		t.Fatal(err)
	}
	if _, err := callTool(t, w, "run_command", map[string]any{
		"command": "gofmt", "args": []string{"-d", "main.go"}, "timeout": 30,
	}); err == nil || !strings.Contains(err.Error(), "configuration storage") {
		t.Fatalf("protected executable error = %v", err)
	}
}

func TestWorkspaceGitDisablesConfiguredFSMonitor(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	marker := filepath.Join(root, "fsmonitor-ran")
	hook := filepath.Join(root, "fsmonitor.sh")
	hookContents := strings.Join([]string{"#!/bin/sh", "touch \"" + marker + "\"", "printf '{}'", ""}, "\n")
	if err := os.WriteFile(hook, []byte(hookContents), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"config", "core.fsmonitor", hook}} {
		command := exec.Command("git", args...)
		command.Dir = root
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	w, _ := testWorkspace(t, root, "", true)
	if _, err := callTool(t, w, "run_command", map[string]any{
		"command": "git", "args": []string{"status", "--short"}, "timeout": 30,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("configured fsmonitor executed: %v", err)
	}
}

func TestWorkspaceVerifyPresets(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.invalid/fixture\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "fixture.go"), []byte("package fixture\n\nfunc Value( )int{return 1}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w, _ := testWorkspace(t, root, "", true)

	for _, preset := range []string{"test", "check", "build"} {
		result, err := callTool(t, w, "verify", map[string]any{"preset": preset})
		if err != nil || !strings.Contains(result, "status: 0") {
			t.Fatalf("verify %s = %q, %v", preset, result, err)
		}
	}
	result, err := callTool(t, w, "verify", map[string]any{"preset": "format"})
	if err != nil || !strings.Contains(result, "func Value() int") {
		t.Fatalf("verify format = %q, %v", result, err)
	}
	contents, err := os.ReadFile(filepath.Join(root, "fixture.go"))
	if err != nil || string(contents) != "package fixture\n\nfunc Value( )int{return 1}\n" {
		t.Fatalf("format verification mutated source: %q, %v", contents, err)
	}
	if _, err := callTool(t, w, "verify", map[string]any{"preset": "unknown"}); err == nil {
		t.Fatal("verify accepted unknown preset")
	}

	emptyWorkspace, _ := testWorkspace(t, t.TempDir(), "", true)
	if _, err = callTool(t, emptyWorkspace, "verify", map[string]any{"preset": "format"}); err == nil || !strings.Contains(err.Error(), "no supported format verification") {
		t.Fatalf("empty format verification error = %v", err)
	}
}

func TestWorkspaceProjectAwareVerificationCommands(t *testing.T) {
	tests := []struct {
		name   string
		files  map[string]string
		preset string
		want   []verificationCommand
	}{
		{
			name: "Make target takes precedence over Go",
			files: map[string]string{
				"Makefile": "check:\n\tgo vet ./...\n",
				"go.mod":   "module example.invalid/make\n",
			},
			preset: "check",
			want:   []verificationCommand{{command: "make", args: []string{"check"}}},
		},
		{
			name:   "Python test",
			files:  map[string]string{"pyproject.toml": "[project]\nname = \"fixture\"\n"},
			preset: "test",
			want:   []verificationCommand{{command: "python3", args: []string{"-m", "pytest"}}},
		},
		{
			name: "pnpm scripts",
			files: map[string]string{
				"package.json":   `{"scripts":{"check":"tsc --noEmit","format:check":"prettier --check ."}}`,
				"pnpm-lock.yaml": "lockfileVersion: '9.0'\n",
			},
			preset: "check",
			want:   []verificationCommand{{command: "pnpm", args: []string{"run", "check"}}},
		},
		{
			name:   "npm test fallback for check",
			files:  map[string]string{"package.json": `{"scripts":{"test":"node --test"}}`},
			preset: "check",
			want:   []verificationCommand{{command: "npm", args: []string{"test"}}},
		},
		{
			name:   "Cargo format check",
			files:  map[string]string{"Cargo.toml": "[package]\nname = \"fixture\"\n"},
			preset: "format",
			want:   []verificationCommand{{command: "cargo", args: []string{"fmt", "--", "--check"}}},
		},
		{
			name: "mixed root verifies each project",
			files: map[string]string{
				"go.mod":       "module example.invalid/mixed\n",
				"package.json": `{"scripts":{"build":"tsc"}}`,
				"Cargo.toml":   "[package]\nname = \"fixture\"\n",
			},
			preset: "build",
			want: []verificationCommand{
				{command: "go", args: []string{"build", "./..."}},
				{command: "npm", args: []string{"run", "build"}},
				{command: "cargo", args: []string{"build"}},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			for name, contents := range test.files {
				if err := os.WriteFile(filepath.Join(root, name), []byte(contents), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			workspace, _ := testWorkspace(t, root, "", true)
			got, err := workspace.verificationCommands(test.preset)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(test.want) {
				t.Fatalf("verification commands = %#v, want %#v", got, test.want)
			}
			for index := range got {
				if got[index].command != test.want[index].command || strings.Join(got[index].args, "\x00") != strings.Join(test.want[index].args, "\x00") {
					t.Fatalf("verification commands = %#v, want %#v", got, test.want)
				}
			}
		})
	}
}

func TestWorkspaceProjectAwareVerificationRejectsInvalidManifest(t *testing.T) {
	for _, manifest := range []string{"{", `{"scripts":{"test":42}}`, `{"scripts":{"test":"node --test"}} {}`} {
		t.Run(manifest, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "package.json"), []byte(manifest), 0o644); err != nil {
				t.Fatal(err)
			}
			workspace, _ := testWorkspace(t, root, "", true)
			if _, err := workspace.verificationCommands("test"); err == nil || !strings.Contains(err.Error(), "parse package.json") {
				t.Fatalf("invalid package.json error = %v", err)
			}
		})
	}
}

func TestWorkspaceRunsWorkspaceLocalGoProgram(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.invalid/hello\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, "cmd", "hello")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	program := "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"hello from meldra\") }\n"
	if err := os.WriteFile(filepath.Join(directory, "main.go"), []byte(program), 0o644); err != nil {
		t.Fatal(err)
	}
	w, _ := testWorkspace(t, root, "", true)
	result, err := callTool(t, w, "run_command", map[string]any{
		"command": "go", "args": []string{"run", "./cmd/hello"}, "timeout": 30,
	})
	if err != nil || !strings.Contains(result, "status: 0") || !strings.Contains(result, "hello from meldra") {
		t.Fatalf("go run result = %q, %v", result, err)
	}
	if _, err := callTool(t, w, "run_command", map[string]any{
		"command": "go", "args": []string{"run", "./../outside.go"}, "timeout": 30,
	}); err == nil || !strings.Contains(err.Error(), "not allowlisted") {
		t.Fatalf("escaping go run error = %v", err)
	}
}

func TestWorkspaceRunsWorkspaceLocalPythonProgram(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not installed")
	}
	root := t.TempDir()
	program := filepath.Join(root, "hello.py")
	if err := os.WriteFile(program, []byte("print('hello from meldra')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w, _ := testWorkspace(t, root, "yes\n", false)
	result, err := callTool(t, w, "run_command", map[string]any{
		"command": "python3", "args": []string{"./hello.py"}, "timeout": 30,
	})
	if err != nil || !strings.Contains(result, "status: 0") || !strings.Contains(result, "hello from meldra") {
		t.Fatalf("python3 result = %q, %v", result, err)
	}
	if _, err := callTool(t, w, "run_command", map[string]any{
		"command": "python3", "args": []string{"../outside.py"}, "timeout": 30,
	}); err == nil || !strings.Contains(err.Error(), "not allowlisted") {
		t.Fatalf("escaping python3 error = %v", err)
	}
}

func TestLimitedBufferPreservesWriteContractAndMarksTruncation(t *testing.T) {
	buffer := limitedBuffer{limit: 4}
	if n, err := buffer.Write([]byte("abcdef")); err != nil || n != 6 {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if got := buffer.String(); got != "a..." {
		t.Fatalf("String = %q", got)
	}
	if n, err := buffer.Write([]byte("more")); err != nil || n != 4 {
		t.Fatalf("second Write = %d, %v", n, err)
	}
}

func TestWorkspaceGitReview(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "meldra@example.invalid"},
		{"config", "user.name", "Meldra Test"},
	} {
		command := exec.Command("git", args...)
		command.Dir = root
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "review.txt"), []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "review.txt"}, {"commit", "-qm", "initial"}} {
		command := exec.Command("git", args...)
		command.Dir = root
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "review.txt"), []byte("after\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w, _ := testWorkspace(t, root, "", true)
	result, err := callTool(t, w, "git_review", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "review.txt") || !strings.Contains(result, "initial") || !strings.Contains(result, "+after") {
		t.Fatalf("git review output = %q", result)
	}
}
