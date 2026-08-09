package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSessionStoreSaveLoadLatestAndPermissions(t *testing.T) {
	paths, err := ConfigPathsForHome(filepath.Join(t.TempDir(), "meldra-home"))
	if err != nil {
		t.Fatal(err)
	}
	store := NewSessionStore(paths)
	session, err := store.Create(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session.Plan = []string{"inspect", "edit", "verify"}
	session.Summary = "Work in progress"
	session.appendMessage("user", "continue the implementation")
	if err := store.Save(session); err != nil {
		t.Fatal(err)
	}

	loaded, err := store.Load("latest")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ID != session.ID || loaded.Summary != session.Summary || len(loaded.Plan) != 3 || !loaded.resumed {
		t.Fatalf("loaded session = %#v", loaded)
	}
	for _, path := range []string{filepath.Join(paths.Home, sessionsDirName), store.path(session.ID)} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		want := os.FileMode(0o700)
		if !info.IsDir() {
			want = 0o600
		}
		if info.Mode().Perm() != want {
			t.Errorf("permissions for %s = %04o, want %04o", path, info.Mode().Perm(), want)
		}
	}
}

func TestSessionStoreRejectsTraversalAndSymlink(t *testing.T) {
	paths, err := ConfigPathsForHome(filepath.Join(t.TempDir(), "meldra-home"))
	if err != nil {
		t.Fatal(err)
	}
	store := NewSessionStore(paths)
	if _, err := store.Load("../secret"); err == nil {
		t.Fatal("accepted invalid session ID")
	}
	if err := store.ensureDir(); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outside, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, store.path("linked")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load("linked"); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("Load error = %v", err)
	}
}

func TestSessionToolsPersistPlanAndSummary(t *testing.T) {
	paths, err := ConfigPathsForHome(filepath.Join(t.TempDir(), "meldra-home"))
	if err != nil {
		t.Fatal(err)
	}
	store := NewSessionStore(paths)
	session, err := store.Create(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tools := NewSessionTools(session, store)
	if _, err := tools.updatePlan(toolInput(t, map[string]any{"steps": []string{"inspect", "verify"}})); err != nil {
		t.Fatal(err)
	}
	if _, err := tools.saveSummary(toolInput(t, map[string]any{"summary": "Tests pass"})); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Plan) != 2 || loaded.Summary != "Tests pass" {
		t.Fatalf("persisted session = %#v", loaded)
	}
}

func TestAllStrictToolSchemasUseArrayRequired(t *testing.T) {
	w, _ := testWorkspace(t, t.TempDir(), "", true)
	paths, err := ConfigPathsForHome(filepath.Join(t.TempDir(), "meldra-home"))
	if err != nil {
		t.Fatal(err)
	}
	store := NewSessionStore(paths)
	session, err := store.New(w.root)
	if err != nil {
		t.Fatal(err)
	}
	tools := append(w.ToolDefinitions(), NewSessionTools(session, store).ToolDefinitions()...)
	for _, tool := range tools {
		encoded, err := json.Marshal(tool.Parameters)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), `"required":null`) {
			t.Errorf("tool %s has null required: %s", tool.Name, encoded)
		}
	}
}

func TestSessionListSkipsCorruptFiles(t *testing.T) {
	paths, err := ConfigPathsForHome(filepath.Join(t.TempDir(), "meldra-home"))
	if err != nil {
		t.Fatal(err)
	}
	store := NewSessionStore(paths)
	valid, err := store.Create(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	valid.appendMessage("user", "list sessions")
	if err := store.Save(valid); err != nil {
		t.Fatal(err)
	}
	corruptPath := store.path("corrupt")
	corruptContents := []byte(`{"id":"corrupt","workspace":"contains-a-secret","unexpected":true}`)
	if err := os.WriteFile(corruptPath, corruptContents, 0o600); err != nil {
		t.Fatal(err)
	}
	sessions, diagnostics, err := store.ListWithDiagnostics()
	if err != nil || len(sessions) != 1 || sessions[0].ID != valid.ID {
		t.Fatalf("sessions = %#v, error = %v", sessions, err)
	}
	if diagnostics.SkippedFiles != 1 {
		t.Fatalf("skipped files = %d, want 1", diagnostics.SkippedFiles)
	}
	after, err := os.ReadFile(corruptPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, corruptContents) {
		t.Fatalf("ListWithDiagnostics modified corrupt session file:\nbefore: %q\nafter: %q", corruptContents, after)
	}
}

func TestSessionListSkipsEmptySessionsWithoutDeleting(t *testing.T) {
	paths, err := ConfigPathsForHome(filepath.Join(t.TempDir(), "meldra-home"))
	if err != nil {
		t.Fatal(err)
	}
	store := NewSessionStore(paths)
	empty, err := store.Create(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	withMessages, err := store.Create(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	withMessages.appendMessage("user", "hello")
	if err := store.Save(withMessages); err != nil {
		t.Fatal(err)
	}
	sessions, err := store.List()
	if err != nil || len(sessions) != 1 || sessions[0].ID != withMessages.ID {
		t.Fatalf("sessions = %#v, error = %v", sessions, err)
	}
	if _, err := os.Stat(store.path(empty.ID)); err != nil {
		t.Fatalf("empty session file was deleted, stat error = %v", err)
	}
}

func TestSessionListMarksMissingWorkspacesUnavailableWithoutDeleting(t *testing.T) {
	paths, err := ConfigPathsForHome(filepath.Join(t.TempDir(), "meldra-home"))
	if err != nil {
		t.Fatal(err)
	}
	store := NewSessionStore(paths)
	missingWorkspace := filepath.Join(t.TempDir(), "removed-workspace")
	session, err := store.Create(missingWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	session.appendMessage("user", "work in the removed workspace")
	if err := store.Save(session); err != nil {
		t.Fatal(err)
	}

	before, err := os.ReadFile(store.path(session.ID))
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].ID != session.ID || !sessions[0].WorkspaceUnavailable {
		t.Fatalf("sessions = %#v, want unavailable session %q", sessions, session.ID)
	}
	after, err := os.ReadFile(store.path(session.ID))
	if err != nil {
		t.Fatalf("session file was deleted, read error = %v", err)
	}
	if !bytes.Equal(after, before) {
		t.Fatalf("List modified session file:\nbefore: %s\nafter: %s", before, after)
	}
}

func TestSessionListWorkspaceFiltersByCurrentRoot(t *testing.T) {
	paths, err := ConfigPathsForHome(filepath.Join(t.TempDir(), "meldra-home"))
	if err != nil {
		t.Fatal(err)
	}
	store := NewSessionStore(paths)
	currentWorkspace := t.TempDir()
	otherWorkspace := t.TempDir()
	current, err := store.Create(currentWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	current.appendMessage("user", "current project")
	if err := store.Save(current); err != nil {
		t.Fatal(err)
	}
	other, err := store.Create(otherWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	other.appendMessage("user", "other project")
	if err := store.Save(other); err != nil {
		t.Fatal(err)
	}

	sessions, err := store.ListWorkspace(currentWorkspace)
	if err != nil || len(sessions) != 1 || sessions[0].ID != current.ID {
		t.Fatalf("workspace sessions = %#v, error = %v", sessions, err)
	}
}

func TestSessionResumeContextIncludesBoundedHistoryPlanAndSummary(t *testing.T) {
	session := &Session{
		Summary: "Implemented safe workspace tools.",
		Plan:    []string{"inspect", "edit", "verify"},
	}
	for index := 0; index < 15; index++ {
		session.appendMessage("user", fmt.Sprintf("message-%02d", index))
	}
	context := session.resumeContext()
	for _, want := range []string{"Previous session summary:", session.Summary, "1. inspect", "3. verify", "message-03", "message-14"} {
		if !strings.Contains(context, want) {
			t.Errorf("resume context missing %q:\n%s", want, context)
		}
	}
	if strings.Contains(context, "message-02") {
		t.Fatalf("resume context included more than 12 recent messages:\n%s", context)
	}

	long := strings.Repeat("x", maxSessionMessageBytes+100)
	session.appendMessage("assistant", long)
	last := session.Messages[len(session.Messages)-1].Content
	if len(last) > maxSessionMessageBytes || !strings.HasSuffix(last, sessionMessageTruncationSuffix) {
		t.Fatalf("long message was not bounded: length=%d", len(last))
	}
}

func TestSessionMessageTruncationPreservesUTF8AndByteLimit(t *testing.T) {
	for _, character := range []string{"\u00e9", "\u754c", "\U0001F642"} {
		t.Run(fmt.Sprintf("%d-byte rune", len(character)), func(t *testing.T) {
			prefix := strings.Repeat("x", maxSessionMessageBytes-len(sessionMessageTruncationSuffix)-1)
			content := prefix + character + strings.Repeat("y", len(sessionMessageTruncationSuffix)+1)
			session := &Session{}
			session.appendMessage("assistant", content)
			got := session.Messages[0].Content
			if !utf8.ValidString(got) {
				t.Fatalf("truncated message is invalid UTF-8: %q", got)
			}
			if len(got) > maxSessionMessageBytes {
				t.Fatalf("truncated message length = %d, want at most %d", len(got), maxSessionMessageBytes)
			}
			want := prefix + sessionMessageTruncationSuffix
			if got != want {
				t.Fatalf("truncated message = %q, want %q", got, want)
			}
		})
	}
}

func TestSessionMessageTruncationNormalizesInvalidUTF8(t *testing.T) {
	session := &Session{}
	session.appendMessage("assistant", "before"+string([]byte{0xff})+"after")
	got := session.Messages[0].Content
	if !utf8.ValidString(got) {
		t.Fatalf("message contains invalid UTF-8: %q", got)
	}
	if !strings.ContainsRune(got, utf8.RuneError) {
		t.Fatalf("message did not replace invalid UTF-8: %q", got)
	}
}

func TestSessionStoreRejectsTrailingJSON(t *testing.T) {
	paths, err := ConfigPathsForHome(filepath.Join(t.TempDir(), "meldra-home"))
	if err != nil {
		t.Fatal(err)
	}
	store := NewSessionStore(paths)
	session, err := store.Create(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	contents, err := json.Marshal(session)
	if err != nil {
		t.Fatal(err)
	}
	contents = append(contents, []byte("\n{}")...)
	if err := os.WriteFile(store.path(session.ID), contents, privateFilePerm); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(session.ID); err == nil || !strings.Contains(err.Error(), "trailing data") {
		t.Fatalf("Load error = %v, want trailing data error", err)
	}
}

func TestRunSessionsCommandMarksUnavailableWorkspace(t *testing.T) {
	paths := mustConfigPaths(t)
	t.Setenv(MeldraHomeEnv, paths.Home)
	store := NewSessionStore(paths)
	workspace := filepath.Join(t.TempDir(), "unavailable-workspace")
	session, err := store.Create(workspace)
	if err != nil {
		t.Fatal(err)
	}
	session.appendMessage("user", "resume this later")
	if err := store.Save(session); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := runSessionsCommand(&output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), workspace+" [unavailable]") {
		t.Fatalf("session list = %q, want unavailable workspace", output.String())
	}
}

func BenchmarkSessionResumeContext(b *testing.B) {
	session := &Session{
		Summary: strings.Repeat("completed work ", 100),
		Plan:    []string{"inspect", "edit", "verify"},
	}
	for index := range 100 {
		session.Messages = append(session.Messages, SessionMessage{
			Role:    "assistant",
			Content: fmt.Sprintf("message %d: %s", index, strings.Repeat("context ", 20)),
		})
	}
	b.ReportAllocs()
	for b.Loop() {
		if context := session.resumeContext(); context == "" {
			b.Fatal("resumeContext returned an empty context")
		}
	}
}

func TestSessionStoreRejectsInsecureAndInvalidFiles(t *testing.T) {
	paths, err := ConfigPathsForHome(filepath.Join(t.TempDir(), "meldra-home"))
	if err != nil {
		t.Fatal(err)
	}
	store := NewSessionStore(paths)
	if _, err := store.Load("latest"); err == nil || !strings.Contains(err.Error(), "no saved sessions") {
		t.Fatalf("empty latest error = %v", err)
	}
	if err := store.ensureDir(); err != nil {
		t.Fatal(err)
	}
	path := store.path("insecure")
	if err := os.WriteFile(path, []byte(`{"id":"insecure","workspace":"/tmp"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load("insecure"); err == nil || !strings.Contains(err.Error(), "permissions") {
		t.Fatalf("insecure session error = %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"id":"different","workspace":"/tmp"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load("insecure"); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("mismatched session ID error = %v", err)
	}
}

func TestSessionToolValidation(t *testing.T) {
	paths, err := ConfigPathsForHome(filepath.Join(t.TempDir(), "meldra-home"))
	if err != nil {
		t.Fatal(err)
	}
	store := NewSessionStore(paths)
	session, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tools := NewSessionTools(session, store)
	if _, err := tools.updatePlan(toolInput(t, map[string]any{"steps": []string{"valid", "  "}})); err == nil {
		t.Fatal("accepted an empty plan step")
	}
	if _, err := tools.updatePlan(toolInput(t, map[string]any{"steps": make([]string, 21)})); err == nil {
		t.Fatal("accepted more than 20 plan steps")
	}
	if _, err := tools.saveSummary(toolInput(t, map[string]any{"summary": "  "})); err == nil {
		t.Fatal("accepted empty summary")
	}
	status, err := tools.status(toolInput(t, map[string]any{}))
	if err != nil || !strings.Contains(status, session.ID) || !strings.Contains(status, session.Workspace) {
		t.Fatalf("session status = %q, %v", status, err)
	}
}
