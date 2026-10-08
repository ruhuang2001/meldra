package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"meldra/internal/provider"
	taskstore "meldra/internal/store"
	"meldra/internal/task"
	"meldra/internal/tool"
)

func TestFailedProcessCreationHasKnownOutcome(t *testing.T) {
	w, _ := testWorkspace(t, t.TempDir(), "", false)
	dir := t.TempDir()
	executable := filepath.Join(dir, "go")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	w.SetApprovalFunc(func(context.Context, ApprovalRequest) bool {
		if err := os.Remove(executable); err != nil {
			t.Fatal(err)
		}
		return true
	})
	registry, err := tool.New(w.ToolDefinitions())
	if err != nil {
		t.Fatal(err)
	}
	result, err := registry.Invoke(t.Context(), "run_command", json.RawMessage(`{"command":"go","args":["test"],"timeout":5}`))
	if err == nil || result.Status != tool.Failed {
		t.Fatalf("unexpected process outcome %+v %v", result, err)
	}
}

func TestCancellationBeforeProcessStartHasKnownOutcome(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	w, _ := testWorkspace(t, t.TempDir(), "", false)
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	if err := os.WriteFile(filepath.Join(dir, "go"), []byte("#!/bin/sh\ntouch \"${0%/*}/ran\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	w.SetApprovalFunc(func(context.Context, ApprovalRequest) bool { cancel(); return true })
	registry, err := tool.New(w.ToolDefinitions())
	if err != nil {
		t.Fatal(err)
	}
	result, _ := registry.Invoke(ctx, "run_command", json.RawMessage(`{"command":"go","args":["test"],"timeout":5}`))
	if result.Status != tool.Cancelled {
		t.Fatalf("unstarted cancellation=%+v", result)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("cancelled child ran")
	}
}

func TestAuxiliaryEffectsPreventAutomaticFileRecovery(t *testing.T) {
	for _, mode := range []string{"parent_directory", "temporary_file"} {
		t.Run(mode, func(t *testing.T) {
			w, _ := testWorkspace(t, t.TempDir(), "", true)
			path := "nested/new.txt"
			if mode == "temporary_file" {
				path = "new.txt"
			}
			changes, err := w.prepare([]changeInput{{Path: path, NewStr: "after"}})
			if err != nil {
				t.Fatal(err)
			}
			evidence := changeFingerprints(changes, false)
			if mode == "parent_directory" {
				if err := os.Mkdir(filepath.Join(w.root, "nested"), 0755); err != nil {
					t.Fatal(err)
				}
			} else {
				file, err := os.CreateTemp(w.root, ".meldra-write-*.tmp")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := file.WriteString("partial"); err != nil {
					t.Fatal(err)
				}
				file.Close()
			}
			state, err := w.reconcileFiles(evidence)
			if err != nil || state != "changed" {
				t.Fatalf("state=%q error=%v", state, err)
			}
		})
	}
}

func TestAuxiliaryDirectoryEvidenceSupportsCompleteWriteAndUndo(t *testing.T) {
	w, _ := testWorkspace(t, t.TempDir(), "", true)
	changes, err := w.prepare([]changeInput{{Path: "a/b/new.txt", NewStr: "content"}})
	if err != nil {
		t.Fatal(err)
	}
	evidence := changeFingerprints(changes, false)
	assertState := func(raw json.RawMessage, want string) {
		t.Helper()
		state, err := w.reconcileFiles(raw)
		if err != nil || state != want {
			t.Fatalf("state=%s want=%s err=%v", state, want, err)
		}
	}
	assertState(evidence, "before")
	if err := w.writeChanges(changes, false); err != nil {
		t.Fatal(err)
	}
	assertState(evidence, "after")
	undoEvidence := changeFingerprints(changes, true)
	assertState(undoEvidence, "before")
	if err := w.writeChanges(changes, true); err != nil {
		t.Fatal(err)
	}
	assertState(undoEvidence, "after")
	var legacy []fileFingerprint
	if err := json.Unmarshal(evidence, &legacy); err != nil {
		t.Fatal(err)
	}
	legacy[0].AuxiliaryVersion = 0
	old, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	assertState(old, "changed")
}

func TestSnapshotReconstructionReturnsCorruptRecordError(t *testing.T) {
	a, paths := recordedAgent(t, nil, nil)
	if err := a.execution.begin(t.Context(), "unsaved"); err != nil {
		t.Fatal(err)
	}
	if err := a.execution.close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(taskDirectory(paths), "tasks.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec("UPDATE tasks SET record='invalid-json' WHERE id=?", a.session.ID)
	_ = db.Close()
	if err != nil {
		t.Fatal(err)
	}
	_, err = loadSessionForResume(paths, a.session.ID)
	if err == nil || !strings.Contains(err.Error(), "corrupt task record") {
		t.Fatalf("record error hidden: %v", err)
	}
}

func TestSnapshotReconstructionReturnsStorageFailure(t *testing.T) {
	a, paths := recordedAgent(t, nil, nil)
	if err := a.execution.begin(t.Context(), "unsaved"); err != nil {
		t.Fatal(err)
	}
	if err := a.execution.close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(taskDirectory(paths), "tasks.db")
	if err := os.Truncate(path, taskstore.MaxDatabaseBytes+1); err != nil {
		t.Fatal(err)
	}
	_, err := loadSessionForResume(paths, a.session.ID)
	if !errors.Is(err, task.ErrLimit) {
		t.Fatalf("missing storage error: %v", err)
	}
}

func TestClockRollbackPreservesUncheckpointedRequest(t *testing.T) {
	a, paths := recordedAgent(t, inferenceFunc(func(context.Context, provider.Request, provider.Options, provider.Observer) (provider.Result, error) {
		return provider.Result{Response: finishResponse()}, nil
	}), nil)
	if err := a.RunTurn(t.Context(), "old request"); err != nil {
		t.Fatal(err)
	}
	a.session.LastRequestSequence = 0
	if err := a.store.Save(a.session); err != nil {
		t.Fatal(err)
	}
	snapshotTime := a.session.UpdatedAt
	const lost = "new intent after snapshot, before crash"
	if err := a.execution.begin(t.Context(), lost); err != nil {
		t.Fatal(err)
	}
	sequence := a.execution.requestSequence
	if err := a.execution.close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(taskDirectory(paths), "tasks.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec("UPDATE events SET record=json_set(CAST(record AS TEXT),'$.time',?) WHERE task_id=? AND seq=?", snapshotTime.Add(-time.Hour).Format(time.RFC3339Nano), a.session.ID, sequence)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	recovered := recoveryExecution(t, paths, a.execution.workspace.root, a.session.ID)
	if err := recovered.begin(t.Context(), "continue"); err != nil {
		t.Fatal(err)
	}
	saved, err := NewSessionStore(paths).Load(a.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.LastRequestSequence < sequence || !strings.Contains(saved.resumeContext(), lost) {
		t.Fatalf("request lost: %+v", saved)
	}
	if err := recovered.finish(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
}

func TestMissingResponseIdentitySavesPartialStream(t *testing.T) {
	a, paths := recordedAgent(t, inferenceFunc(func(_ context.Context, _ provider.Request, _ provider.Options, o provider.Observer) (provider.Result, error) {
		o.Text("partial explanation", true)
		response := callResponse("call", "list_files", `{}`)
		response.ID = ""
		return provider.Result{Response: response, StreamedText: "partial explanation", StreamedTextShown: true, ReceivedTextDelta: true}, nil
	}), nil)
	a.customProvider = true
	sink := &followupEvents{}
	a.events = sink
	if err := a.RunTurn(t.Context(), "inspect"); err == nil {
		t.Fatal("missing ID accepted")
	}
	saved, err := NewSessionStore(paths).Load(a.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(saved.resumeContext(), "partial explanation") {
		t.Fatal("partial text not persisted")
	}
	finished := false
	for _, e := range sink.events {
		finished = finished || e.Kind == UIEventAssistantDone
	}
	if !finished {
		t.Fatal("stream not finalized")
	}
}

func TestMissingResponseIdentityDisplaysNonstreamedText(t *testing.T) {
	a, paths := recordedAgent(t, inferenceFunc(func(context.Context, provider.Request, provider.Options, provider.Observer) (provider.Result, error) {
		response := callResponse("call", "list_files", `{}`)
		response.ID = ""
		response.Text = "explanation without deltas"
		return provider.Result{Response: response}, nil
	}), nil)
	a.customProvider = true
	sink := &followupEvents{}
	a.events = sink
	if err := a.RunTurn(t.Context(), "inspect"); err == nil {
		t.Fatal("missing identity accepted")
	}
	shown := false
	for _, event := range sink.events {
		shown = shown || (event.Kind == UIEventAssistantMessage && event.Text == "explanation without deltas")
	}
	if !shown {
		t.Fatal("completed text was not displayed")
	}
	saved, err := NewSessionStore(paths).Load(a.session.ID)
	if err != nil || !strings.Contains(saved.resumeContext(), "explanation without deltas") {
		t.Fatalf("text not saved: %+v %v", saved, err)
	}
}

func TestCompletedCommandDoesNotMakeLaterCancellationUnknown(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	w, _ := testWorkspace(t, t.TempDir(), "", false)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	approvals := 0
	w.SetApprovalFunc(func(context.Context, ApprovalRequest) bool {
		approvals++
		if approvals == 2 {
			cancel()
		}
		return true
	})
	registry, err := tool.New([]ToolDefinition{{Name: "composite", Function: w.bindTool(func(json.RawMessage) (string, error) {
		if _, err := w.execute("go", []string{"test"}, 5); err != nil {
			return "", err
		}
		return w.execute("go", []string{"test"}, 5)
	})}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := registry.Invoke(ctx, "composite", json.RawMessage(`{}`))
	if err != nil || approvals != 2 || result.Status != tool.Cancelled {
		t.Fatalf("result=%+v approvals=%d err=%v", result, approvals, err)
	}
}

func TestVerificationFailureDoesNotMaskLaterUncertainProcess(t *testing.T) {
	w, _ := testWorkspace(t, t.TempDir(), "", true)
	if err := os.WriteFile(filepath.Join(w.root, "go.mod"), []byte("module fixture\n\ngo 1.26\n"), 0600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	// The second process exits while a child retains stdout; WaitDelay reports
	// an incomplete process outcome, and runCommandProcess kills its group.
	script := "#!/bin/sh\nif [ \"$1\" = vet ]; then exit 7; fi\nsleep 30 &\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "go"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	registry, err := tool.New(w.ToolDefinitions())
	if err != nil {
		t.Fatal(err)
	}
	result, err := registry.Invoke(t.Context(), "verify", json.RawMessage(`{"preset":"check"}`))
	if err == nil || result.Status != tool.Unknown {
		t.Fatalf("earlier failure masked uncertain result: %+v %v", result, err)
	}
}

func TestRecoveryIgnoresUserNamesResemblingWriteTemporaries(t *testing.T) {
	w, _ := testWorkspace(t, t.TempDir(), "", true)
	changes, err := w.prepare([]changeInput{{Path: "new.txt", NewStr: "new"}})
	if err != nil {
		t.Fatal(err)
	}
	evidence := changeFingerprints(changes, false)
	if err := os.Mkdir(filepath.Join(w.root, ".meldra-write-folder.tmp"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.root, ".meldra-write-notes"), []byte("user notes"), 0600); err != nil {
		t.Fatal(err)
	}
	state, err := w.reconcileFiles(evidence)
	if err != nil || state != "before" {
		t.Fatalf("ordinary user paths blocked recovery: %s %v", state, err)
	}
}

type followupEvents struct{ events []UIEvent }

func (s *followupEvents) Emit(e UIEvent) { s.events = append(s.events, e) }
