package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var benchmarkSanitizedText string

func TestSanitizeTerminalTextRemovesControlsAndPreservesNewlines(t *testing.T) {
	input := "first\n\x1b[31mred\x1b[0m\n\x1b]0;untrusted-title\aafter\n" +
		"before\x1b]8;;https://example.test\x1b\\link\x1b]8;;\x1b\\\n" +
		"nul\x00tab\tcarriage\rreturn\x7f\n"
	want := "first\nred\nafter\nbeforelink\nnultabcarriagereturn\n"

	if got := sanitizeTerminalText(input); got != want {
		t.Fatalf("sanitized text = %q, want %q", got, want)
	}
}

func TestSanitizeTerminalTextKeepsNewlinesAfterIncompleteEscape(t *testing.T) {
	input := "before\x1b[31\nafter"
	want := "before[31\nafter"

	if got := sanitizeTerminalText(input); got != want {
		t.Fatalf("sanitized text = %q, want %q", got, want)
	}
}

func BenchmarkSanitizeTerminalText(b *testing.B) {
	for _, size := range []int{1 << 10, 64 << 10} {
		b.Run(fmt.Sprintf("%dKiB", size>>10), func(b *testing.B) {
			input := strings.Repeat("plain世界\x1b[31mred\x1b[0m\n", size/24+1)[:size]
			b.SetBytes(int64(len(input)))
			b.ReportAllocs()
			for b.Loop() {
				benchmarkSanitizedText = sanitizeTerminalText(input)
			}
		})
	}
}

func TestTUIRenderingSanitizesUntrustedPresentationText(t *testing.T) {
	csi := "\x1b[999z"
	osc := "\x1b]0;untrusted-title\a"
	model := newTUIModel(newTUIController(nil), tuiInitialState{
		workspace: "/tmp/project" + osc,
		sessionID: "session" + csi,
		model:     "model" + osc,
	})
	model.width = 100
	model.height = 24
	model.status = "ready" + csi
	model.entries = []tuiEntry{
		{kind: tuiEntryUser, text: "user\nmessage" + osc},
		{kind: tuiEntryAssistant, text: "assistant\x00" + csi},
		{kind: tuiEntryTool, name: "tool" + osc, detail: "detail\nkept" + csi},
		{kind: tuiEntryNotice, text: "notice" + osc},
		{kind: tuiEntryError, text: "error" + csi},
	}
	model.pending = &tuiApprovalMsg{request: ApprovalRequest{
		Title:  "review" + osc,
		Detail: "--- a/file\n+++ b/file\n+kept" + csi,
	}}
	model.resize()

	content := model.renderTimeline() + "\n" + model.View().Content
	for _, sequence := range []string{csi, osc, "\x00", "\a"} {
		if strings.Contains(content, sequence) {
			t.Fatalf("TUI rendered untrusted terminal sequence %q in %q", sequence, content)
		}
	}
	for _, expected := range []string{"user\nmessage", "detail", "kept", "+++ b/file", "+kept"} {
		if !strings.Contains(content, expected) {
			t.Fatalf("TUI did not retain safe text %q in %q", expected, content)
		}
	}
}

func TestWorkspaceApprovalDiffSanitizesTerminalSequences(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "target.txt")
	if err := os.WriteFile(path, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	workspace, output := testWorkspace(t, root, "n\n", false)
	_, err := callTool(t, workspace, "edit_file", map[string]any{
		"path":    "target.txt",
		"old_str": "old\n",
		"new_str": "\x1b[31mred\x1b[0m\n\x1b]0;untrusted-title\aplain\ncontrol\x00text\n",
	})
	if err != nil {
		t.Fatal(err)
	}

	presentation := output.String()
	assertNoTerminalControls(t, presentation)
	for _, sequence := range []string{"\x1b[31m", "\x1b]0;untrusted-title\a", "\x00", "\a"} {
		if strings.Contains(presentation, sequence) {
			t.Fatalf("approval diff rendered terminal sequence %q in %q", sequence, presentation)
		}
	}
	for _, expected := range []string{"+red\n", "+plain\n", "+controltext\n"} {
		if !strings.Contains(presentation, expected) {
			t.Fatalf("approval diff did not retain safe line %q in %q", expected, presentation)
		}
	}
}

func TestSessionListSanitizesSavedSummary(t *testing.T) {
	paths := mustConfigPaths(t)
	t.Setenv(MeldraHomeEnv, paths.Home)
	store := NewSessionStore(paths)
	session, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session.appendMessage("user", "show summary")
	session.Summary = "safe\x1b]0;untrusted-title\asummary\x1b[31m"
	if err := store.Save(session); err != nil {
		t.Fatal(err)
	}

	var output strings.Builder
	if err := runSessionsCommand(&output); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); strings.Contains(got, "\x1b") || strings.Contains(got, "\a") {
		t.Fatalf("sessions output contains a terminal sequence: %q", got)
	}
	if got := output.String(); !strings.Contains(got, "safesummary") {
		t.Fatalf("sessions output lost summary text: %q", got)
	}
}

func assertNoTerminalControls(t *testing.T, text string) {
	t.Helper()
	for _, character := range text {
		if character != '\n' && terminalControlRune(character) {
			t.Fatalf("terminal control %U reached presentation %q", character, text)
		}
	}
}
