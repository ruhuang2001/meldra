package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
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
	} {
		if _, err := parseChatOptions(args); err == nil {
			t.Fatalf("parseChatOptions(%#v) accepted a flag as a value", args)
		}
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
