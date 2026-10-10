package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"meldra/internal/task"
)

func TestRequestEventsPageOnlyRequestsInTaskOrder(t *testing.T) {
	f := setup(t)
	if _, err := f.s.LatestRequestEvent(t.Context(), f.task.ID); !errors.Is(err, task.ErrNotFound) {
		t.Fatalf("empty latest=%v", err)
	}
	for i := range 103 {
		raw, _ := json.Marshal(map[string]string{"request": fmt.Sprintf("request-%d", i)})
		if err := f.s.AppendEvent(t.Context(), f.l, task.Event{TaskID: f.task.ID, RunID: f.run.ID, Kind: "turn.started", Data: raw}); err != nil {
			t.Fatal(err)
		}
		if err := f.s.AppendEvent(t.Context(), f.l, task.Event{TaskID: f.task.ID, RunID: f.run.ID, Kind: "model.completed"}); err != nil {
			t.Fatal(err)
		}
	}
	page, err := f.s.RequestEvents(t.Context(), f.task.ID, 0, 100)
	if err != nil || len(page) != 100 {
		t.Fatalf("page=%d err=%v", len(page), err)
	}
	next, err := f.s.RequestEvents(t.Context(), f.task.ID, page[99].Sequence, 100)
	if err != nil || len(next) != 3 {
		t.Fatalf("next=%d err=%v", len(next), err)
	}
	latest, err := f.s.LatestRequestEvent(t.Context(), f.task.ID)
	if err != nil || latest.Sequence != next[2].Sequence {
		t.Fatalf("latest=%+v err=%v", latest, err)
	}
	if latest.Kind != "turn.started" {
		t.Fatal("latest event included model output")
	}
	second, err := f.s.EnsureTask(t.Context(), task.Task{ID: NewID(), Goal: "second task", Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := f.s.Acquire(t.Context(), second.ID, second.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if err := f.s.AppendEvent(t.Context(), lease, task.Event{TaskID: second.ID, Kind: "turn.started", Data: json.RawMessage(`{"request":"other-task request"}`)}); err != nil {
		t.Fatal(err)
	}
	other, err := f.s.RequestEvents(t.Context(), second.ID, 0, 100)
	if err != nil || len(other) != 1 || !strings.Contains(string(other[0].Data), "other-task request") {
		t.Fatalf("cross-task results=%v err=%v", other, err)
	}
	latest, err = f.s.LatestRequestEvent(t.Context(), f.task.ID)
	if err != nil || latest.Sequence != next[2].Sequence {
		t.Fatalf("second task contaminated latest request: %+v %v", latest, err)
	}
	if _, err := f.s.RequestEvents(t.Context(), f.task.ID, -1, 100); err == nil {
		t.Fatal("negative cursor accepted")
	}
}
