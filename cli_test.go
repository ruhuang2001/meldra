package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
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
			name: "latest session",
			args: nil,
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
