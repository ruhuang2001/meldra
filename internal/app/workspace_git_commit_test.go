package app

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"meldra/internal/tool"
)

func commitFixture(t *testing.T) (string, func(...string) string) {
	t.Helper()
	root := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
		return string(output)
	}
	git("init", "-q")
	git("config", "user.name", "Meldra Test")
	git("config", "user.email", "test@example.invalid")
	for _, name := range []string{"selected.txt", "other.txt", "deleted.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("before\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("add", ".")
	git("-c", "core.hooksPath=/dev/null", "commit", "-qm", "initial")
	for _, name := range []string{"selected.txt", "other.txt", "new file.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name+" after\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root, git
}

func TestGitCommitApprovesOnlySelectedFiles(t *testing.T) {
	root, git := commitFixture(t)
	if err := os.Remove(filepath.Join(root, "deleted.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "hooks", "pre-commit"), []byte("#!/bin/sh\ntouch hook-ran\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	w, _ := testWorkspace(t, root, "", false)
	approvals := 0
	w.SetApprovalFunc(func(_ context.Context, request ApprovalRequest) bool {
		approvals++
		for _, want := range []string{"selected.txt after", "new file.txt after", "deleted.txt", "checkpoint"} {
			if !strings.Contains(request.Detail, want) {
				t.Errorf("missing review evidence %q", want)
			}
		}
		return true
	})
	registry, err := tool.New(w.ToolDefinitions())
	if err != nil {
		t.Fatal(err)
	}
	result, err := registry.Invoke(t.Context(), "git_commit", json.RawMessage(`{"paths":["selected.txt","new file.txt","deleted.txt"],"message":"checkpoint"}`))
	if err != nil || result.Status != tool.Succeeded {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if approvals != 1 || strings.TrimSpace(git("log", "-1", "--format=%s")) != "checkpoint" {
		t.Fatal("commit or approval missing")
	}
	if got := git("show", "HEAD:other.txt"); got != "before\n" {
		t.Fatalf("unselected content committed: %q", got)
	}
	if got := git("show", "HEAD:new file.txt"); got != "new file.txt after\n" {
		t.Fatalf("new file=%q", got)
	}
	if got := git("diff-tree", "--no-commit-id", "--name-status", "-r", "HEAD"); !strings.Contains(got, "D\tdeleted.txt") {
		t.Fatal(got)
	}
	if _, err := os.Stat(filepath.Join(root, "hook-ran")); !os.IsNotExist(err) {
		t.Fatal("hook executed")
	}
}

func TestGitCommitRejectsUnapprovedOrChangedState(t *testing.T) {
	for _, scenario := range []string{"decline", "changed", "staged", "plan", "directory", "outside", "duplicate"} {
		t.Run(scenario, func(t *testing.T) {
			root, git := commitFixture(t)
			head := git("rev-parse", "HEAD")
			w, _ := testWorkspace(t, root, "", false)
			paths := []string{"selected.txt"}
			switch scenario {
			case "staged":
				git("add", "other.txt")
			case "plan":
				w.policy, _ = newRuntimePolicy(ModePlan, PermissionInteractive)
				w.autoApprove = true
			case "directory":
				paths = []string{"."}
			case "outside":
				paths = []string{"../outside"}
			case "duplicate":
				paths = []string{"selected.txt", "./selected.txt"}
			}
			index := git("diff", "--cached")
			w.SetApprovalFunc(func(context.Context, ApprovalRequest) bool {
				if scenario == "changed" {
					if err := os.WriteFile(filepath.Join(root, "selected.txt"), []byte("unreviewed"), 0o644); err != nil {
						t.Fatal(err)
					}
					return true
				}
				return false
			})
			_, err := callTool(t, w, "git_commit", gitCommitInput{paths, "checkpoint"})
			if scenario != "decline" && err == nil {
				t.Fatal("unsafe commit accepted")
			}
			if git("rev-parse", "HEAD") != head || git("diff", "--cached") != index {
				t.Fatal("rejected commit mutated repository")
			}
		})
	}
}

func TestGitCommitRejectsFiltersBeforeApproval(t *testing.T) {
	root, git := commitFixture(t)
	git("config", "filter.replace.clean", "touch filter-ran; sed s/after/transformed/g")
	git("config", "filter.replace.required", "true")
	if err := os.WriteFile(filepath.Join(root, ".gitattributes"), []byte("selected.txt filter=replace\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	head := git("rev-parse", "HEAD")
	w, _ := testWorkspace(t, root, "", false)
	w.SetApprovalFunc(func(context.Context, ApprovalRequest) bool {
		t.Error("unsafe preflight reached approval")
		return false
	})
	_, err := callTool(t, w, "git_commit", gitCommitInput{[]string{"selected.txt"}, "checkpoint"})
	if err == nil || !strings.Contains(err.Error(), "filters require manual review") {
		t.Fatalf("err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "filter-ran")); !os.IsNotExist(err) {
		t.Fatal("Git filter executed before approval")
	}
	if git("rev-parse", "HEAD") != head {
		t.Fatal("filter output committed")
	}
}

func TestMCPGitCommitBindsReviewedContent(t *testing.T) {
	root, git := commitFixture(t)
	t.Setenv(MeldraHomeEnv, t.TempDir())
	c := newPermissionReviewMCP(t, root, true, PermissionWorkspaceEdit)
	if result := c.call("read_project_context", map[string]any{"path": nil}); result["isError"] == true {
		t.Fatal(result)
	}
	args := map[string]any{"paths": []string{"selected.txt"}, "message": "checkpoint"}
	first := c.call("git_commit", args)
	encoded, _ := json.Marshal(first)
	if first["requestState"] == nil || !strings.Contains(string(encoded), "selected.txt after") {
		t.Fatalf("missing reviewed diff: %s", encoded)
	}
	if err := os.WriteFile(filepath.Join(root, "selected.txt"), []byte("changed after preview\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	approve := func(state any) map[string]any {
		return c.request("tools/call", map[string]any{"name": "git_commit", "arguments": args, "requestState": state, "inputResponses": map[string]any{"approval": map[string]any{"action": "accept", "content": map[string]any{"approve": true}}}})
	}
	if result := approve(first["requestState"]); result["isError"] != true {
		t.Fatal("stale preview accepted", result)
	}
	if strings.TrimSpace(git("log", "-1", "--format=%s")) != "initial" {
		t.Fatal("stale approval committed")
	}
	second := c.call("git_commit", args)
	if result := approve(second["requestState"]); result["isError"] == true {
		t.Fatal(result)
	}
	if git("show", "HEAD:selected.txt") != "changed after preview\n" {
		t.Fatal("fresh approval did not commit reviewed content")
	}
}
