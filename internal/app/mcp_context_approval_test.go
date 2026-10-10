package app

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMCPApprovalContextResolvesAllMutationScopesBeforeApproval(t *testing.T) {
	for _, test := range []struct {
		name, raw string
		want      []string
	}{
		{"edit_file", `{"path":"first/new/deep.txt","old_str":"","new_str":"new"}`, []string{".", "first"}},
		{"apply_patch", `{"patch":null,"changes":[{"path":"first/new.txt","old_str":"","new_str":"a"},{"path":"second/new.txt","old_str":"","new_str":"b"}]}`, []string{".", "first", "second"}},
		{"apply_patch", `{"patch":"--- a/first/old.txt\n+++ /dev/null\n@@ -1 +0,0 @@\n-old\n--- /dev/null\n+++ b/second/new.txt\n@@ -0,0 +1 @@\n+new\n","changes":null}`, []string{".", "first", "second"}},
		{"undo_last_change", `{}`, []string{".", "first", "second"}},
	} {
		t.Run(test.name+test.raw, func(t *testing.T) {
			w := contextWorkspace(t)
			writeContextFixture(t, w, "AGENTS.md", "root")
			writeContextFixture(t, w, "first/AGENTS.md", "first")
			writeContextFixture(t, w, "second/AGENTS.md", "second")
			w.last = []fileChange{{path: "first/deleted.txt"}, {path: "second/edited.txt"}}
			if _, err := w.projectContext.ContextTool().Function(t.Context(), json.RawMessage(`{"path":null}`)); err != nil {
				t.Fatal(err)
			}
			if _, err := w.mcpApprovalContextDigest(test.name, json.RawMessage(test.raw)); err == nil {
				t.Fatal("operation requested approval before target rules were shown")
			}
			snapshot, err := w.projectContext.Refresh()
			if err != nil {
				t.Fatal(err)
			}
			for _, scope := range test.want {
				found := false
				for _, rule := range snapshot.Rules {
					if rule.Scope == scope {
						found = true
					}
				}
				if !found {
					t.Fatalf("missing target scope %s in %+v", scope, snapshot)
				}
			}
			if _, err := w.projectContext.ContextTool().Function(t.Context(), json.RawMessage(`{"path":null}`)); err != nil {
				t.Fatal(err)
			}
			old, err := w.mcpApprovalContextDigest(test.name, json.RawMessage(test.raw))
			if err != nil {
				t.Fatal(err)
			}
			writeContextFixture(t, w, "first/AGENTS.md", "changed first")
			if _, err := w.projectContext.ContextTool().Function(t.Context(), json.RawMessage(`{"path":null}`)); err != nil {
				t.Fatal(err)
			}
			updated, err := w.mcpApprovalContextDigest(test.name, json.RawMessage(test.raw))
			if err != nil {
				t.Fatal(err)
			}
			if updated == old {
				t.Fatal("context reread preserved stale approval fingerprint")
			}
		})
	}
}

func TestMCPApprovalContextRejectsInvalidPathsAndPatchShapes(t *testing.T) {
	w := contextWorkspace(t)
	for _, test := range []struct{ name, raw string }{
		{"edit_file", `{"path":"../outside","old_str":"","new_str":"a"}`},
		{"apply_patch", `{"patch":null,"changes":null}`},
		{"apply_patch", `{"patch":"invalid","changes":null}`},
		{"apply_patch", `{"patch":"x","changes":[{"path":"a"}]}`},
		{"undo_last_change", `{}`},
	} {
		if _, err := w.mcpApprovalContextDigest(test.name, json.RawMessage(test.raw)); err == nil {
			t.Fatalf("invalid preapproval target accepted: %s %s", test.name, test.raw)
		}
	}
	writeContextFixture(t, w, "AGENTS.md", "root")
	if _, err := w.mcpApprovalContextDigest("run_command", json.RawMessage(`{"command":"python3"}`)); err == nil || !strings.Contains(err.Error(), "project instructions") {
		t.Fatalf("commands omitted root scope: %v", err)
	}
}
