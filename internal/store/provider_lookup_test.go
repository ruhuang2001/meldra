package store

import (
	"errors"
	"strings"
	"testing"

	"meldra/internal/task"
)

func TestProviderCallLookupIsScopedBoundedAndIndexed(t *testing.T) {
	f := setup(t)
	first, err := f.s.PlanTool(t.Context(), f.l, task.ToolCall{TaskID: f.task.ID, RunID: f.run.ID, ProviderCallID: "same-provider-id", Name: "read_file", Effect: task.Read})
	if err != nil {
		t.Fatal(err)
	}
	other, err := f.s.EnsureTask(t.Context(), task.Task{ID: "other-task", Goal: "other task", Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	l, err := f.s.Acquire(t.Context(), other.ID, other.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	r, err := f.s.StartRun(t.Context(), l, task.Run{TaskID: other.ID})
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.s.PlanTool(t.Context(), l, task.ToolCall{TaskID: other.ID, RunID: r.ID, ProviderCallID: "same-provider-id", Name: "write_file", Effect: task.Write})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []task.ToolCall{first, second} {
		got, err := f.s.GetToolCallByProviderID(t.Context(), want.TaskID, want.ProviderCallID)
		if err != nil || got.ID != want.ID {
			t.Fatalf("lookup crossed task scope: %+v %v", got, err)
		}
	}
	for _, ids := range [][2]string{{f.task.ID, "absent"}, {"missing", "same-provider-id"}, {f.task.ID, ""}, {"../invalid", "same-provider-id"}} {
		if _, err := f.s.GetToolCallByProviderID(t.Context(), ids[0], ids[1]); !errors.Is(err, task.ErrNotFound) {
			t.Fatalf("missing identity %+v: %v", ids, err)
		}
	}
	if _, err := f.s.GetToolCallByProviderID(t.Context(), f.task.ID, strings.Repeat("x", 1025)); !errors.Is(err, task.ErrLimit) {
		t.Fatal(err)
	}
	rows, err := f.s.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+providerLookupSQL, f.task.ID, "same-provider-id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	indexed := false
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(detail, "USING INDEX tools_provider_call") {
			indexed = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !indexed {
		t.Fatal("provider identity lookup regressed to a history scan")
	}
}

func TestSchemaOneWithoutProviderIndexRemainsReadable(t *testing.T) {
	f := setup(t)
	if _, err := f.s.db.Exec("DROP INDEX tools_provider_call"); err != nil {
		t.Fatal(err)
	}
	if err := f.l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(f.s.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.GetTask(t.Context(), f.task.ID); err != nil {
		t.Fatal(err)
	}
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != task.DatabaseSchemaVersion {
		t.Fatal("index upgrade changed semantic schema")
	}
}
