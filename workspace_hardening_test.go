package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

func environmentValue(environment []string, name string) (string, bool) {
	prefix := name + "="
	for _, variable := range environment {
		if strings.HasPrefix(variable, prefix) {
			return strings.TrimPrefix(variable, prefix), true
		}
	}
	return "", false
}

func TestCommandEnvironmentIsMinimalAndTemporary(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "must-not-reach-child")
	t.Setenv("DATABASE_URL", "must-not-reach-child")
	t.Setenv("MELDRA_HOME", "must-not-reach-child")

	environment, cleanup, err := newCommandEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	home, ok := environmentValue(environment, "HOME")
	if !ok || home == "" {
		t.Fatalf("command environment HOME = %q, present=%t", home, ok)
	}
	if home == os.Getenv("HOME") {
		t.Fatal("command environment reused the host HOME")
	}
	if info, err := os.Stat(home); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("temporary HOME = %v, %v", info, err)
	}
	temporary, ok := environmentValue(environment, "TMPDIR")
	if !ok || temporary == "" || temporary == os.TempDir() {
		t.Fatalf("command environment TMPDIR = %q, present=%t", temporary, ok)
	}
	for _, name := range []string{"OPENAI_API_KEY", "DATABASE_URL", "MELDRA_HOME", "PYTHONPATH", "SSH_AUTH_SOCK"} {
		if _, ok := environmentValue(environment, name); ok {
			t.Fatalf("command environment contains %s", name)
		}
	}
}

func TestWorkspaceCommandApprovalDescribesHostPrivileges(t *testing.T) {
	w, _ := testWorkspace(t, t.TempDir(), "", false)
	var request ApprovalRequest
	w.SetApprovalFunc(func(_ context.Context, got ApprovalRequest) bool {
		request = got
		return false
	})

	result, err := callTool(t, w, "run_command", map[string]any{
		"command": "go", "args": []string{"test", "./..."},
	})
	if err != nil || result != "Declined; command not run." {
		t.Fatalf("command result = %q, %v", result, err)
	}
	if request.Kind != ApprovalCommand || !strings.Contains(request.Title, "OS user privileges") || !strings.Contains(request.Detail, "filesystem and network") {
		t.Fatalf("approval request = %#v", request)
	}
}

func TestWorkspaceFileToolsObserveCancelledContext(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(root+"/visible.txt", []byte("needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w, _ := testWorkspace(t, root, "", true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w.SetContext(ctx)

	for name, input := range map[string]map[string]any{
		"read_file":    {"path": "visible.txt"},
		"list_files":   {"path": ""},
		"search_files": {"query": "needle", "path": ""},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := callTool(t, w, name, input); !errors.Is(err, context.Canceled) {
				t.Fatalf("%s error = %v, want context cancellation", name, err)
			}
		})
	}
}

func TestWorkspaceRollsBackCurrentChangeWhenDirectorySyncFails(t *testing.T) {
	root := t.TempDir()
	path := root + "/target.txt"
	if err := os.WriteFile(path, []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w, _ := testWorkspace(t, root, "", true)
	calls := 0
	syncFailure := errors.New("directory sync failed")
	w.syncDir = func(string) error {
		calls++
		if calls == 1 {
			return syncFailure
		}
		return nil
	}

	_, err := callTool(t, w, "edit_file", map[string]any{
		"path": "target.txt", "old_str": "before", "new_str": "after",
	})
	if !errors.Is(err, syncFailure) {
		t.Fatalf("edit error = %v, want directory sync failure", err)
	}
	contents, readErr := os.ReadFile(path)
	if readErr != nil || string(contents) != "before\n" {
		t.Fatalf("current change was not rolled back: %q, %v", contents, readErr)
	}
	if calls != 2 {
		t.Fatalf("directory sync calls = %d, want apply and rollback", calls)
	}
}

func TestWorkspaceRollsBackEveryFileWhenLaterDirectorySyncFails(t *testing.T) {
	root := t.TempDir()
	first := root + "/first.txt"
	second := root + "/second.txt"
	if err := os.WriteFile(first, []byte("first before\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("second before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, _ := testWorkspace(t, root, "", true)
	calls := 0
	syncFailure := errors.New("second directory sync failed")
	w.syncDir = func(string) error {
		calls++
		if calls == 2 {
			return syncFailure
		}
		return nil
	}

	_, err := callTool(t, w, "apply_patch", map[string]any{
		"patch": nil,
		"changes": []map[string]any{
			{"path": "first.txt", "old_str": "first before", "new_str": "first after"},
			{"path": "second.txt", "old_str": "second before", "new_str": "second after"},
		},
	})
	if !errors.Is(err, syncFailure) {
		t.Fatalf("apply error = %v, want directory sync failure", err)
	}
	for path, want := range map[string]string{
		first:  "first before\n",
		second: "second before\n",
	} {
		contents, readErr := os.ReadFile(path)
		if readErr != nil || string(contents) != want {
			t.Fatalf("rolled-back %s = %q, %v; want %q", path, contents, readErr, want)
		}
	}
	if calls != 4 {
		t.Fatalf("directory sync calls = %d, want two writes and two rollbacks", calls)
	}
}
