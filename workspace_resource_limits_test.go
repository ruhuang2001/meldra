package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestWorkspaceRejectsOversizedPatch(t *testing.T) {
	w, _ := testWorkspace(t, t.TempDir(), "", true)
	_, err := callTool(t, w, "apply_patch", map[string]any{
		"patch":   strings.Repeat(" ", maxPatchBytes+1),
		"changes": nil,
	})
	if err == nil || !strings.Contains(err.Error(), "patch exceeds") {
		t.Fatalf("oversized patch error = %v", err)
	}
}

func TestWorkspaceRejectsTooManyChangesBeforeWriting(t *testing.T) {
	root := t.TempDir()
	w, _ := testWorkspace(t, root, "", true)
	changes := make([]map[string]any, maxChangeFiles+1)
	for i := range changes {
		changes[i] = map[string]any{
			"path":    filepath.Join("new", strconv.Itoa(i)+".txt"),
			"old_str": "",
			"new_str": "new file\n",
		}
	}
	_, err := callTool(t, w, "apply_patch", map[string]any{"patch": nil, "changes": changes})
	if err == nil || !strings.Contains(err.Error(), "file limit") {
		t.Fatalf("too-many-changes error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "new", "0.txt")); !os.IsNotExist(err) {
		t.Fatalf("over-limit changes wrote a file: %v", err)
	}
}

func TestWorkspaceRejectsOversizedEditableFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "large.txt")
	contents := strings.Repeat("x", maxEditableFileBytes+1)
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	w, _ := testWorkspace(t, root, "", true)
	_, err := callTool(t, w, "edit_file", map[string]any{
		"path":    "large.txt",
		"old_str": "x",
		"new_str": "y",
	})
	if err == nil || !strings.Contains(err.Error(), "editable-file limit") {
		t.Fatalf("oversized file error = %v", err)
	}
	if info, err := os.Stat(path); err != nil || info.Size() != int64(len(contents)) {
		t.Fatalf("oversized file was changed: info=%v, error=%v", info, err)
	}
}

func TestWorkspaceRejectsCombinedChangeStateOverLimit(t *testing.T) {
	root := t.TempDir()
	w, _ := testWorkspace(t, root, "", true)
	content := strings.Repeat("x", maxChangeContentBytes/3+1)
	changes := []map[string]any{
		{"path": "first.txt", "old_str": "", "new_str": content},
		{"path": "second.txt", "old_str": "", "new_str": content},
		{"path": "third.txt", "old_str": "", "new_str": content},
	}
	_, err := callTool(t, w, "apply_patch", map[string]any{"patch": nil, "changes": changes})
	if err == nil || !strings.Contains(err.Error(), "combined before/after limit") {
		t.Fatalf("combined-state error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "first.txt")); !os.IsNotExist(err) {
		t.Fatalf("over-limit changes wrote a file: %v", err)
	}
}

func TestWorkspaceRejectsUnpreviewableChanges(t *testing.T) {
	root := t.TempDir()
	w, output := testWorkspace(t, root, "", true)
	_, err := callTool(t, w, "edit_file", map[string]any{
		"path":    "preview.txt",
		"old_str": "",
		"new_str": strings.Repeat("x", maxApprovalPreviewBytes+1),
	})
	if err == nil || !strings.Contains(err.Error(), "full diff cannot be shown") {
		t.Fatalf("approval-preview error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "preview.txt")); !os.IsNotExist(err) {
		t.Fatalf("unpreviewable change wrote a file: %v", err)
	}
	if output.Len() != 0 {
		t.Fatalf("unpreviewable change was presented for approval: %q", output.String())
	}
}

func TestWorkspaceDiffSizeMatchesRenderedPreview(t *testing.T) {
	root := t.TempDir()
	w, _ := testWorkspace(t, root, "", true)
	changes := []fileChange{
		{
			path:        filepath.Join(root, "changed.txt"),
			before:      []byte("before\n"),
			after:       []byte("after"),
			existed:     true,
			afterExists: true,
		},
		{
			path:        filepath.Join(root, "created.txt"),
			after:       []byte("created\n"),
			afterExists: true,
		},
	}
	size, err := w.diffSize(changes, false)
	if err != nil {
		t.Fatal(err)
	}
	diff, err := w.diff(changes, false)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(diff)) != size {
		t.Fatalf("diff size = %d, rendered size = %d", size, len(diff))
	}
}

func TestTruncateUTF8TextPreservesRuneBoundaries(t *testing.T) {
	limit := len(outputTruncationSuffix) + 1
	for name, input := range map[string]string{
		"two-byte":   "a\u00a2after",
		"three-byte": "a界after",
		"four-byte":  "a😀after",
	} {
		t.Run(name, func(t *testing.T) {
			got := truncateUTF8Text(input+strings.Repeat("x", limit), limit, false)
			if got != "a"+outputTruncationSuffix {
				t.Fatalf("truncated text = %q", got)
			}
			if len(got) != limit || !utf8.ValidString(got) {
				t.Fatalf("truncated output is not bounded valid UTF-8: %q", got)
			}
		})
	}
}

func TestTruncateUTF8TextNormalizesInvalidInputAndBoundsSuffix(t *testing.T) {
	if got := truncateUTF8Text("a\xff", 4, false); got != "a�" || !utf8.ValidString(got) {
		t.Fatalf("invalid UTF-8 normalization = %q", got)
	}
	if got := truncateUTF8Text("abcdef", 4, true); got != "a..." || len(got) != 4 {
		t.Fatalf("small-budget truncation = %q", got)
	}
	got := capText(strings.Repeat("x", maxToolOutput+1))
	if len(got) != maxToolOutput || !strings.HasSuffix(got, outputTruncationSuffix) || !utf8.ValidString(got) {
		t.Fatalf("capText output = %q", got[maxToolOutput-len(outputTruncationSuffix):])
	}
}
