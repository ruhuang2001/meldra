package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"meldra/internal/provider"
)

func contextWorkspace(t *testing.T) *Workspace {
	t.Helper()
	w, err := NewWorkspace(t.TempDir(), bufio.NewReader(strings.NewReader("")), io.Discard, true)
	if err != nil {
		t.Fatal(err)
	}
	w.projectContext = NewProjectContext(w)
	return w
}

func writeContextFixture(t *testing.T, w *Workspace, path, content string) {
	t.Helper()
	full := filepath.Join(w.root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestProjectContextScopesRefreshAndWriteGate(t *testing.T) {
	w := contextWorkspace(t)
	writeContextFixture(t, w, "AGENTS.md", "root rules")
	writeContextFixture(t, w, "web/AGENTS.md", "web rules")
	writeContextFixture(t, w, "server/AGENTS.md", "server rules")
	writeContextFixture(t, w, "web/ui/AGENTS.md", "UI rules")
	if _, err := w.projectContext.Instructions(); err != nil {
		t.Fatal(err)
	}
	err := w.projectContext.BeforeWrite([]string{"web/ui/new.go", "server/new.go"})
	if _, ok := errors.AsType[*ProjectContextChangedError](err); !ok {
		t.Fatalf("new rules must reject generated write: %v", err)
	}
	snapshot, err := w.projectContext.Refresh()
	if err != nil {
		t.Fatal(err)
	}
	var scopes []string
	for _, rule := range snapshot.Rules {
		scopes = append(scopes, rule.Scope)
	}
	if !slices.Equal(scopes, []string{".", "server", "web", "web/ui"}) {
		t.Fatalf("scopes=%v", scopes)
	}
	if err := w.projectContext.BeforeWrite([]string{"web/ui/new.go"}); err == nil {
		t.Fatal("discovery alone acknowledged new rules")
	}
	instructions, err := w.projectContext.Instructions()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(instructions, `"scope":"web"`) || !strings.Contains(instructions, "only inside that directory") {
		t.Fatalf("lost nested scope: %s", instructions)
	}
	if err := w.projectContext.BeforeWrite([]string{"web/ui/new.go", "server/new.go"}); err != nil {
		t.Fatal(err)
	}
	writeContextFixture(t, w, "web/AGENTS.md", "changed after approval")
	if err := w.projectContext.BeforeWrite([]string{"web/ui/new.go"}); err == nil {
		t.Fatal("changed instructions did not invalidate write")
	}
	if err := os.Remove(filepath.Join(w.root, "web/AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	if err := w.projectContext.BeforeWrite([]string{"web/ui/new.go"}); err == nil {
		t.Fatal("deleted instructions did not invalidate write")
	}
}

func TestProjectContextMissingRulesAndSelfEdit(t *testing.T) {
	w := contextWorkspace(t)
	if _, err := w.projectContext.Instructions(); err != nil {
		t.Fatal(err)
	}
	if err := w.projectContext.BeforeWrite([]string{"new/deep/file.go"}); err != nil {
		t.Fatal(err)
	}
	writeContextFixture(t, w, "AGENTS.md", "existing rules")
	if err := w.projectContext.BeforeWrite([]string{"AGENTS.md"}); err == nil {
		t.Fatal("new root rules were not discovered")
	}
	if _, err := w.projectContext.Instructions(); err != nil {
		t.Fatal(err)
	}
	if err := w.projectContext.BeforeWrite([]string{"AGENTS.md"}); err != nil {
		t.Fatal(err)
	}
}

func TestProjectContextRejectsUnsafeRuleSources(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(*testing.T, *Workspace)
	}{
		{"symlink", func(t *testing.T, w *Workspace) {
			outside := filepath.Join(t.TempDir(), "rules")
			if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(w.root, "AGENTS.md")); err != nil {
				t.Fatal(err)
			}
		}},
		{"binary", func(t *testing.T, w *Workspace) { writeContextFixture(t, w, "AGENTS.md", "a\x00b") }},
		{"encoding", func(t *testing.T, w *Workspace) { writeContextFixture(t, w, "AGENTS.md", string([]byte{0xff})) }},
		{"large", func(t *testing.T, w *Workspace) {
			writeContextFixture(t, w, "AGENTS.md", strings.Repeat("x", maxProjectRuleBytes+1))
		}},
		{"directory", func(t *testing.T, w *Workspace) {
			if err := os.Mkdir(filepath.Join(w.root, "AGENTS.md"), 0o700); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			w := contextWorkspace(t)
			test.setup(t, w)
			if _, err := w.projectContext.Instructions(); err == nil {
				t.Fatal("unsafe rules accepted")
			}
		})
	}
	w := contextWorkspace(t)
	writeContextFixture(t, w, "private/AGENTS.md", "private")
	if err := w.ProtectPath(filepath.Join(w.root, "private")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"../outside/file.go", ".git/config", "private/file.go"} {
		if err := w.projectContext.BeforeWrite([]string{path}); err == nil {
			t.Fatalf("unsafe path accepted: %s", path)
		}
	}
}

func TestProjectContextRejectsAggregateQuotaAndAncestorSymlink(t *testing.T) {
	w := contextWorkspace(t)
	var paths []string
	for i := range 5 {
		dir := fmt.Sprintf("area%d", i)
		writeContextFixture(t, w, dir+"/AGENTS.md", strings.Repeat("r", maxProjectRuleBytes))
		paths = append(paths, dir+"/new.txt")
	}
	if _, err := w.projectContext.Inspect(paths); err == nil || !strings.Contains(err.Error(), "total limit") {
		t.Fatalf("aggregate limit err=%v", err)
	}
	w = contextWorkspace(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "AGENTS.md"), []byte("must not read"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(w.root, "linked")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.projectContext.Inspect([]string{"linked/new.txt"}); err == nil {
		t.Fatal("ancestor symlink accepted")
	}
}

func TestProjectContextLimitsEncodedInstructions(t *testing.T) {
	for _, source := range []string{"escaped content", "path metadata"} {
		t.Run(source, func(t *testing.T) {
			w := contextWorkspace(t)
			var paths []string
			if source == "escaped content" {
				writeContextFixture(t, w, "AGENTS.md", strings.Repeat("<", maxProjectRuleBytes))
			} else {
				for i := range 300 {
					dir := fmt.Sprintf("%03d-%s", i, strings.Repeat("x", 200))
					writeContextFixture(t, w, dir+"/AGENTS.md", "")
					paths = append(paths, dir+"/new.txt")
				}
			}
			for _, path := range paths {
				if err := w.projectContext.ObservePath(path); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := w.projectContext.Instructions(); err == nil || !strings.Contains(err.Error(), "total limit") {
				t.Fatalf("oversized encoded instructions accepted: %v", err)
			}
			if w.projectContext.presented != "" {
				t.Error("rejected instructions changed acknowledged context")
			}
			if _, err := w.projectContext.ContextTool().Function(t.Context(), json.RawMessage(`{"path":"."}`)); err == nil {
				t.Error("context tool bypassed encoded instruction limit")
			}
			if err := w.projectContext.BeforeWrite([]string{"new.txt"}); err == nil {
				t.Error("write gate bypassed encoded instruction limit")
			}
		})
	}
}

func TestProjectContextEncodedInstructionLimitBoundary(t *testing.T) {
	w := contextWorkspace(t)
	var paths []string
	for i := range 4 {
		path := fmt.Sprintf("area%d/AGENTS.md", i)
		writeContextFixture(t, w, path, "")
		paths = append(paths, path)
	}
	snapshot, err := w.projectContext.Inspect(paths)
	if err != nil {
		t.Fatal(err)
	}
	remaining := maxProjectContextBytes - len(snapshot.Instructions())
	var lastSize int
	for _, path := range paths {
		lastSize = min(remaining, maxProjectRuleBytes)
		writeContextFixture(t, w, path, strings.Repeat("r", lastSize))
		remaining -= lastSize
	}
	if remaining != 0 || lastSize >= maxProjectRuleBytes {
		t.Fatal("fixture cannot reach the precise encoded context limit")
	}
	instructions, err := w.projectContext.Instructions()
	if err != nil || len(instructions) != maxProjectContextBytes {
		t.Fatalf("exact-limit instructions: size = %d, err = %v", len(instructions), err)
	}
	presented := w.projectContext.presented
	writeContextFixture(t, w, paths[len(paths)-1], strings.Repeat("r", lastSize+1))
	if _, err := w.projectContext.Instructions(); err == nil || !strings.Contains(err.Error(), "total limit") {
		t.Fatalf("one-byte overflow accepted: %v", err)
	}
	if w.projectContext.presented != presented {
		t.Error("failed refresh changed acknowledged context")
	}
}

func TestContextCommandShowsExactApplicableSources(t *testing.T) {
	w := contextWorkspace(t)
	writeContextFixture(t, w, "AGENTS.md", "root")
	writeContextFixture(t, w, "src/AGENTS.md", "child")
	writeContextFixture(t, w, "other/AGENTS.md", "unrelated")
	var out strings.Builder
	if err := runContextCommand(t.Context(), []string{"--workspace", w.root, "--path", "src/new.go", "--json"}, &out); err != nil {
		t.Fatal(err)
	}
	var result ProjectContextSnapshot
	if err := json.Unmarshal([]byte(out.String()), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Rules) != 2 || result.Rules[1].Scope != "src" || len(result.Digest) != 64 {
		t.Fatalf("unexpected context: %+v", result)
	}
}

func TestIdleContextCommandDoesNotInvokeProviderOrAcknowledgeRules(t *testing.T) {
	w := contextWorkspace(t)
	writeContextFixture(t, w, "nested/AGENTS.md", "local context")
	requests := []string{"/context nested/new.txt", "/context ../outside"}
	var output strings.Builder
	a := NewAgent(inferenceFunc(func(context.Context, provider.Request, provider.Options, provider.Observer) (provider.Result, error) {
		t.Fatal("context inspector invoked provider")
		return provider.Result{}, nil
	}), func() (string, bool) {
		if len(requests) == 0 {
			return "", false
		}
		next := requests[0]
		requests = requests[1:]
		return next, true
	}, nil)
	a.projectContext = w.projectContext
	a.output = &output
	if _, err := w.projectContext.Instructions(); err != nil {
		t.Fatal(err)
	}
	if err := a.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "nested/AGENTS.md (scope: nested; sha256:") || !strings.Contains(output.String(), "escapes workspace") {
		t.Fatalf("context output=%s", output.String())
	}
	if err := w.projectContext.BeforeWrite([]string{"nested/new.txt"}); err == nil {
		t.Fatal("idle inspection acknowledged model instructions")
	}
}

func TestProjectContextToolRequiresFreshNativeInference(t *testing.T) {
	w := contextWorkspace(t)
	writeContextFixture(t, w, "nested/AGENTS.md", "scoped rule")
	if _, err := w.projectContext.Instructions(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.projectContext.ContextTool().Function(t.Context(), json.RawMessage(`{"path":"nested/new.txt"}`)); err != nil {
		t.Fatal(err)
	}
	if err := w.projectContext.BeforeWrite([]string{"nested/new.txt"}); err == nil {
		t.Fatal("native model bypassed fresh inference by reading context in its old tool batch")
	}
	if _, err := w.projectContext.Instructions(); err != nil {
		t.Fatal(err)
	}
	if err := w.projectContext.BeforeWrite([]string{"nested/new.txt"}); err != nil {
		t.Fatal(err)
	}

	external := NewProjectContext(w)
	if err := external.BeforeWrite([]string{"nested/new.txt"}); err == nil {
		t.Fatal("external client wrote before loading rules")
	}
	if _, err := external.ContextTool().Function(t.Context(), json.RawMessage(`{"path":"nested/new.txt"}`)); err != nil {
		t.Fatal(err)
	}
	if err := external.BeforeWrite([]string{"nested/new.txt"}); err != nil {
		t.Fatal(err)
	}
}

func TestProjectRulesChangedDuringApprovalBlocksWriteAndUndo(t *testing.T) {
	for _, undo := range []bool{false, true} {
		t.Run(fmt.Sprint(undo), func(t *testing.T) {
			w := contextWorkspace(t)
			writeContextFixture(t, w, "AGENTS.md", "original")
			writeContextFixture(t, w, "file.txt", "before")
			if _, err := w.projectContext.Instructions(); err != nil {
				t.Fatal(err)
			}
			if undo {
				if _, err := w.editFile(json.RawMessage(`{"path":"file.txt","old_str":"before","new_str":"after"}`)); err != nil {
					t.Fatal(err)
				}
			}
			w.autoApprove = false
			w.SetApprovalFunc(func(context.Context, ApprovalRequest) bool {
				writeContextFixture(t, w, "AGENTS.md", "changed at approval")
				return true
			})
			var err error
			if undo {
				_, err = w.undo(json.RawMessage(`{}`))
			} else {
				_, err = w.editFile(json.RawMessage(`{"path":"file.txt","old_str":"before","new_str":"after"}`))
			}
			if err == nil {
				t.Fatal("stale approval admitted write")
			}
			got, err := os.ReadFile(filepath.Join(w.root, "file.txt"))
			if err != nil {
				t.Fatal(err)
			}
			want := "before"
			if undo {
				want = "after"
			}
			if string(got) != want {
				t.Fatalf("got=%s want=%s", got, want)
			}
		})
	}
}

func TestProjectRulesChangedDuringCommandApprovalIsRecordedAsDeclined(t *testing.T) {
	w := contextWorkspace(t)
	writeContextFixture(t, w, "AGENTS.md", "original")
	if _, err := w.projectContext.Instructions(); err != nil {
		t.Fatal(err)
	}
	w.autoApprove = false
	w.SetApprovalFunc(func(context.Context, ApprovalRequest) bool {
		writeContextFixture(t, w, "AGENTS.md", "new rule")
		return true
	})
	recorded := true
	w.approvalRecord = func(_ context.Context, _ ApprovalRequest, approved bool) error { recorded = approved; return nil }
	if w.requestApproval(ApprovalRequest{Kind: ApprovalCommand, Title: "run project code"}) {
		t.Fatal("changed rules admitted command")
	}
	if recorded {
		t.Fatal("stale command approval was recorded as granted")
	}
}

func TestAgentRejectsAllOldBatchWritesAfterDiscoveringRules(t *testing.T) {
	w := contextWorkspace(t)
	writeContextFixture(t, w, "nested/AGENTS.md", "nested-marker")
	requests := 0
	a := NewAgent(inferenceFunc(func(_ context.Context, request provider.Request, _ provider.Options, _ provider.Observer) (provider.Result, error) {
		requests++
		switch requests {
		case 1:
			return provider.Result{Response: &provider.Response{ID: "discover", Status: "completed", Output: []provider.OutputItem{
				{Type: "function_call", CallID: "first", Name: "edit_file", Arguments: `{"path":"nested/a.txt","old_str":"","new_str":"must-not-exist"}`},
				{Type: "function_call", CallID: "second", Name: "edit_file", Arguments: `{"path":"other.txt","old_str":"","new_str":"must-not-exist"}`},
			}}}, nil
		case 2:
			for _, file := range []string{"nested/a.txt", "other.txt"} {
				if _, err := os.Stat(filepath.Join(w.root, file)); !os.IsNotExist(err) {
					t.Fatalf("old batch wrote %s", file)
				}
			}
			if !strings.Contains(request.Instructions, "nested-marker") {
				t.Fatal("fresh inference missing scoped instructions")
			}
			return provider.Result{Response: callResponse("new", "edit_file", `{"path":"nested/a.txt","old_str":"","new_str":"fresh"}`)}, nil
		default:
			return provider.Result{Response: finishResponse()}, nil
		}
	}), nil, w.ToolDefinitions())
	a.projectContext = w.projectContext
	a.output = io.Discard
	if err := a.RunTurn(t.Context(), "make files"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(w.root, "nested/a.txt"))
	if err != nil || string(got) != "fresh" {
		t.Fatalf("fresh write=%s err=%v", got, err)
	}
}
