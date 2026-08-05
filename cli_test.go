package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
)

func TestParseResumeOptionsAcceptsChatFlags(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want ChatOptions
	}{
		{
			name: "session ID followed by yes",
			args: []string{"session-123", "--yes"},
			want: ChatOptions{Resume: "session-123", AutoApprove: true},
		},
		{
			name: "session picker when ID is omitted",
			args: nil,
			want: ChatOptions{selectResume: true},
		},
		{
			name: "explicit latest session",
			args: []string{"latest"},
			want: ChatOptions{Resume: "latest"},
		},
		{
			name: "flags before session ID",
			args: []string{"--yes", "session-123"},
			want: ChatOptions{Resume: "session-123", AutoApprove: true},
		},
		{
			name: "workspace flag and session ID",
			args: []string{"--workspace", "./project", "session-123"},
			want: ChatOptions{Workspace: "./project", Resume: "session-123", workspaceExplicit: true},
		},
		{
			name: "prompt without an explicit session ID",
			args: []string{"--prompt", "fix the bug"},
			want: ChatOptions{Prompt: "fix the bug", selectResume: true},
		},
		{
			name: "prompt before session ID",
			args: []string{"--prompt", "fix the bug", "session-123"},
			want: ChatOptions{Resume: "session-123", Prompt: "fix the bug"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseResumeOptions(test.args)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("options = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestSelectResumeSession(t *testing.T) {
	paths := mustConfigPaths(t)
	store := NewSessionStore(paths)
	first, err := store.Create(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	unsafeWorkspace := filepath.Join(t.TempDir(), "safe\x1b]0;untrusted-title\aworkspace\nnext")
	if err := os.Mkdir(unsafeWorkspace, 0o755); err != nil {
		t.Fatal(err)
	}
	first.Workspace = unsafeWorkspace
	first.Summary = "safe\x1b[31msummary\x1b[0m\nnext"
	first.appendMessage("user", "show sessions")
	if err := store.Save(first); err != nil {
		t.Fatal(err)
	}
	second, err := store.Create(first.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	second.appendMessage("user", "continue")
	if err := store.Save(second); err != nil {
		t.Fatal(err)
	}
	sessions, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 {
		t.Fatalf("sessions = %#v", sessions)
	}

	var output bytes.Buffer
	selected, err := selectResumeSession(strings.NewReader("not-a-number\n3\n1\n"), &output, store, first.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	if selected != sessions[0].ID {
		t.Fatalf("selected session = %q, want %q", selected, sessions[0].ID)
	}
	if got := strings.Count(output.String(), "Invalid session selection."); got != 2 {
		t.Fatalf("invalid selection notices = %d, output = %q", got, output.String())
	}
	for _, unsafe := range []string{"\x1b", "\a"} {
		if strings.Contains(output.String(), unsafe) {
			t.Fatalf("picker output contains terminal control sequence %q: %q", unsafe, output.String())
		}
	}
	for _, want := range []string{"safeworkspace next", "safesummary next", "Select a session [1-2]"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("picker output missing %q: %q", want, output.String())
		}
	}
}

func TestSelectResumeSessionRejectsEOF(t *testing.T) {
	paths := mustConfigPaths(t)
	store := NewSessionStore(paths)
	session, err := store.Create(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session.appendMessage("user", "resume this")
	if err := store.Save(session); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	_, err = selectResumeSession(strings.NewReader(""), &output, store, session.Workspace)
	if err == nil || !strings.Contains(err.Error(), "session selection cancelled") {
		t.Fatalf("selection error = %v", err)
	}
	if !strings.Contains(output.String(), "Select a session [1-1]") {
		t.Fatalf("picker did not prompt before EOF: %q", output.String())
	}
	if strings.Contains(output.String(), "Invalid session selection.") {
		t.Fatalf("picker reported an invalid selection after immediate EOF: %q", output.String())
	}
}

func TestSelectResumeSessionRejectsEmptyStore(t *testing.T) {
	var output bytes.Buffer
	_, err := selectResumeSession(strings.NewReader("1\n"), &output, NewSessionStore(mustConfigPaths(t)), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "no saved sessions") {
		t.Fatalf("selection error = %v", err)
	}
	if output.Len() != 0 {
		t.Fatalf("picker output = %q, want no prompt", output.String())
	}
}

func TestBareResumePromptsForSessionSelection(t *testing.T) {
	paths := mustConfigPaths(t)
	t.Setenv(MeldraHomeEnv, paths.Home)
	t.Setenv("OPENAI_API_KEY", "")
	store := NewSessionStore(paths)
	currentWorkspace, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	currentWorkspace, err = canonicalWorkspacePath(currentWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(currentWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	session.appendMessage("user", "resume this")
	if err := store.Save(session); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	err = runCLI([]string{"resume"}, strings.NewReader("1\n"), &output)
	if err == nil || !strings.Contains(err.Error(), "OPENAI_API_KEY is not configured") {
		t.Fatalf("runCLI error = %v", err)
	}
	if got := output.String(); !strings.Contains(got, "Saved sessions:") || !strings.Contains(got, "Select a session [1-1]") {
		t.Fatalf("bare resume did not prompt for selection: %q", got)
	}
}

func TestSessionPickerModelNavigatesAndPages(t *testing.T) {
	sessions := make([]Session, 9)
	for index := range sessions {
		sessions[index] = Session{
			ID:        fmt.Sprintf("session-%d", index+1),
			UpdatedAt: time.Date(2026, 8, 5, 12, index, 0, 0, time.UTC),
			Messages:  []SessionMessage{{Role: "user", Content: fmt.Sprintf("request-%d", index+1)}},
		}
	}
	model := &sessionPickerModel{sessions: sessions, height: 10}

	model.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	if model.cursor != 1 || model.offset != 0 {
		t.Fatalf("after down: cursor=%d offset=%d", model.cursor, model.offset)
	}
	model.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyPgDown}))
	if model.cursor != 5 || model.offset != 2 {
		t.Fatalf("after page-down: cursor=%d offset=%d", model.cursor, model.offset)
	}
	if content := model.View().Content; !strings.Contains(content, "session-6") || strings.Contains(content, "session-1") {
		t.Fatalf("paged view = %q", content)
	}
	model.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyPgUp}))
	if model.cursor != 1 || model.offset != 1 {
		t.Fatalf("after page-up: cursor=%d offset=%d", model.cursor, model.offset)
	}
	model.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyUp}))
	if model.cursor != 0 || model.offset != 0 {
		t.Fatalf("after up: cursor=%d offset=%d", model.cursor, model.offset)
	}
}

func TestSessionPickerModelSelectsAndCancels(t *testing.T) {
	sessions := []Session{{ID: "session-1"}, {ID: "session-2"}}
	model := &sessionPickerModel{sessions: sessions, height: 10}
	model.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	model.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	if model.selected != "session-2" {
		t.Fatalf("selected session = %q, want session-2", model.selected)
	}

	cancelled := &sessionPickerModel{sessions: sessions, height: 10}
	cancelled.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEscape}))
	if cancelled.selected != "" {
		t.Fatalf("cancelled picker selected %q", cancelled.selected)
	}
}

func TestParseChatOptionsRejectsAnotherFlagAsAnOptionValue(t *testing.T) {
	for _, args := range [][]string{
		{"--workspace", "--yes"},
		{"--resume", "--yes"},
		{"--prompt", "--yes"},
	} {
		if _, err := parseChatOptions(args); err == nil {
			t.Fatalf("parseChatOptions(%#v) accepted a flag as a value", args)
		}
	}
}

func TestParseChatOptionsAcceptsPrompt(t *testing.T) {
	got, err := parseChatOptions([]string{"--workspace", "./project", "--yes", "--prompt", "fix the bug"})
	if err != nil {
		t.Fatal(err)
	}
	want := ChatOptions{Workspace: "./project", Prompt: "fix the bug", AutoApprove: true, workspaceExplicit: true}
	if got != want {
		t.Fatalf("options = %#v, want %#v", got, want)
	}
}

func TestPrintCLIErrorSanitizesTerminalSequences(t *testing.T) {
	var output bytes.Buffer
	printCLIError(&output, errors.New("request failed: \x1b]0;untrusted-title\aunsafe\x1b[31m"))
	if got := output.String(); strings.Contains(got, "\x1b") || strings.Contains(got, "\a") {
		t.Fatalf("terminal control sequence reached stderr: %q", got)
	}
	if got := output.String(); !strings.Contains(got, "Error: request failed: unsafe") {
		t.Fatalf("sanitized error = %q", got)
	}
}

func TestRunSessionsCommandTruncatesUTF8OnRuneBoundary(t *testing.T) {
	paths := mustConfigPaths(t)
	t.Setenv(MeldraHomeEnv, paths.Home)
	store := NewSessionStore(paths)
	session, err := store.Create(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session.appendMessage("user", "show the recent sessions")
	session.Summary = strings.Repeat("\u754c", 81)
	if err := store.Save(session); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := runSessionsCommand(&output); err != nil {
		t.Fatal(err)
	}
	want := strings.Repeat("\u754c", 80) + "..."
	if got := output.String(); !utf8.ValidString(got) || !strings.Contains(got, want) {
		t.Fatalf("session list = %q, want valid UTF-8 containing %q", got, want)
	}
}
