package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"meldra/internal/provider"
	"meldra/internal/task"
	"meldra/internal/tool"
)

func TestTaskEditConflictRemainsRecoverableWithoutUnknownOutcome(t *testing.T) {
	requests := 0
	backend := inferenceFunc(func(context.Context, provider.Request, provider.Options, provider.Observer) (provider.Result, error) {
		requests++
		switch requests {
		case 1:
			return provider.Result{Response: callResponse("edit-stale", "edit_file", `{"path":"file.txt","old_str":"original","new_str":"agent edit"}`)}, nil
		case 2:
			return provider.Result{Response: callResponse("edit-revised", "edit_file", `{"path":"file.txt","old_str":"editor change","new_str":"reconciled edit"}`)}, nil
		default:
			return provider.Result{Response: finishResponse()}, nil
		}
	})
	agent, paths := recordedAgent(t, backend, nil)
	w := agent.execution.workspace
	path := filepath.Join(w.root, "file.txt")
	if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	approvals := 0
	w.autoApprove = false
	w.SetApprovalFunc(func(context.Context, ApprovalRequest) bool {
		approvals++
		if approvals == 1 {
			if err := os.WriteFile(path, []byte("editor change"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		return true
	})
	if err := agent.RunTurn(t.Context(), "edit file"); err != nil {
		t.Fatal(err)
	}
	if requests != 3 || approvals != 2 {
		t.Fatalf("requests=%d approvals=%d", requests, approvals)
	}
	db := openTaskDB(t, paths)
	calls, err := db.ToolCalls(t.Context(), agent.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0].Status != task.ToolFailed || !strings.Contains(calls[0].Result.Error, "changed since diff") || calls[1].Status != task.ToolSucceeded {
		t.Fatalf("calls=%+v", calls)
	}
	contents, err := os.ReadFile(path)
	if err != nil || string(contents) != "reconciled edit" {
		t.Fatalf("contents=%q err=%v", contents, err)
	}
}

func TestUndoPreimageConflictIsKnownFailure(t *testing.T) {
	w, _ := testWorkspace(t, t.TempDir(), "", true)
	path := filepath.Join(w.root, "file.txt")
	if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := w.applyInputs([]changeInput{{Path: "file.txt", OldStr: "original", NewStr: "changed"}}); err != nil {
		t.Fatal(err)
	}
	w.autoApprove = false
	w.SetApprovalFunc(func(context.Context, ApprovalRequest) bool {
		if err := os.WriteFile(path, []byte("external"), 0600); err != nil {
			t.Fatal(err)
		}
		return true
	})
	registry, err := tool.New(w.ToolDefinitions())
	if err != nil {
		t.Fatal(err)
	}
	result, err := registry.Invoke(t.Context(), "undo_last_change", []byte(`{}`))
	if err == nil || result.Status != tool.Failed {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	contents, err := os.ReadFile(path)
	if err != nil || string(contents) != "external" {
		t.Fatal("undo overwrote external edit")
	}
}

func TestWriteFailureAfterMutationStillRequiresReconciliation(t *testing.T) {
	w, _ := testWorkspace(t, t.TempDir(), "", true)
	if err := os.WriteFile(filepath.Join(w.root, "file.txt"), []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	w.syncDir = func(string) error { return os.ErrPermission }
	registry, err := tool.New(w.ToolDefinitions())
	if err != nil {
		t.Fatal(err)
	}
	result, err := registry.Invoke(t.Context(), "edit_file", []byte(`{"path":"file.txt","old_str":"original","new_str":"changed"}`))
	if err == nil || result.Status != tool.Unknown {
		t.Fatalf("uncertain mutation incorrectly classified: %+v %v", result, err)
	}
}
