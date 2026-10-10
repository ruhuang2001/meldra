package store

import (
	"strings"
	"testing"

	"meldra/internal/task"
)

func TestRunPolicyValidationRejectsMalformedMetadataWithoutMutation(t *testing.T) {
	f := setup(t)
	if err := f.s.FinishRun(t.Context(), f.l, f.run.ID, task.RunSucceeded, "finished"); err != nil {
		t.Fatal(err)
	}
	before, err := f.s.Events(t.Context(), f.task.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, config := range []task.Config{
		{Mode: "PLAN"},
		{Mode: "unsafe"},
		{Permissions: "allow-all"},
		{ApprovedPlanDigest: "short"},
		{ApprovedPlanDigest: strings.Repeat("z", 64)},
	} {
		if _, err := f.s.StartRun(t.Context(), f.l, task.Run{TaskID: f.task.ID, Config: config}); err == nil {
			t.Fatalf("malformed run config accepted: %+v", config)
		}
	}
	runs, err := f.s.Runs(t.Context(), f.task.ID)
	if err != nil || len(runs) != 1 {
		t.Fatalf("malformed policy changed runs: %v err=%v", runs, err)
	}
	after, err := f.s.Events(t.Context(), f.task.ID, 0, 100)
	if err != nil || len(after) != len(before) {
		t.Fatalf("malformed policy changed events: before=%d after=%d err=%v", len(before), len(after), err)
	}
	config := task.Config{Mode: "plan", Permissions: "workspace-edit", PolicyGeneration: 12, ApprovedPlanDigest: strings.Repeat("a", 64)}
	started, err := f.s.StartRun(t.Context(), f.l, task.Run{TaskID: f.task.ID, Config: config})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := f.s.GetRun(t.Context(), started.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Config.Mode != config.Mode || loaded.Config.Permissions != config.Permissions || loaded.Config.PolicyGeneration != config.PolicyGeneration || loaded.Config.ApprovedPlanDigest != config.ApprovedPlanDigest {
		t.Fatalf("policy metadata round trip=%+v", loaded.Config)
	}
}
