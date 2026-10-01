package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"meldra/internal/task"
)

func BenchmarkTaskStoreGet(b *testing.B) {
	f := setup(b)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := f.s.GetTask(b.Context(), f.task.ID); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkTaskStoreEventAppend(b *testing.B) {
	f := setup(b)
	event := task.Event{TaskID: f.task.ID, RunID: f.run.ID, Kind: "model.completed", Data: json.RawMessage(`{"tokens":1200}`)}
	b.ReportAllocs()
	for b.Loop() {
		if err := f.s.AppendEvent(b.Context(), f.l, event); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkTaskStoreEventPage(b *testing.B) {
	f := setup(b)
	for range 1000 {
		if err := f.s.AppendEvent(b.Context(), f.l, task.Event{TaskID: f.task.ID, Kind: "progress"}); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := f.s.Events(b.Context(), f.task.ID, 500, 100); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkTaskStoreToolLifecycle(b *testing.B) {
	f := setup(b)
	b.ReportAllocs()
	for b.Loop() {
		c, err := f.s.PlanTool(b.Context(), f.l, task.ToolCall{TaskID: f.task.ID, RunID: f.run.ID, Name: "read_file", Effect: task.Read, Arguments: json.RawMessage(`{"path":"README.md"}`)})
		if err != nil {
			b.Fatal(err)
		}
		if err := f.s.StartTool(b.Context(), f.l, c.ID); err != nil {
			b.Fatal(err)
		}
		if err := f.s.FinishTool(b.Context(), f.l, c.ID, task.Result{Status: task.ToolSucceeded, Output: "fixture"}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkTaskStoreRecoveryScan(b *testing.B) {
	f := setup(b)
	for range 100 {
		c := plan(b, f, "read_file", task.Read)
		if err := f.s.StartTool(b.Context(), f.l, c.ID); err != nil {
			b.Fatal(err)
		}
		if err := f.s.FinishTool(b.Context(), f.l, c.ID, task.Result{Status: task.ToolSucceeded}); err != nil {
			b.Fatal(err)
		}
	}
	if err := f.s.FinishRun(b.Context(), f.l, f.run.ID, task.RunSucceeded, "complete"); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if err := f.s.RecoverInterrupted(b.Context(), f.l, f.task.ID); err != nil {
			b.Fatal(err)
		}
		if _, err := f.s.ToolCalls(b.Context(), f.task.ID); err != nil {
			b.Fatal(err)
		}
	}
}

// Each operation stores the same fixed 1 MiB output payload. Content addressing
// bounds disk growth independently of how many model turns reference the log.
func BenchmarkTaskStoreArtifactDedup(b *testing.B) {
	f := setup(b)
	data := make([]byte, 1<<20)
	for i := range data {
		data[i] = byte(i % 251)
	}
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := f.s.PutArtifact(b.Context(), f.l, f.task.ID, "command.log", data); err != nil {
			b.Fatal(err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(f.s.dir, "artifacts"))
	if err != nil || len(entries) != 1 {
		b.Fatalf("artifact storage is not deduplicated: %d, %v", len(entries), err)
	}
	info, err := entries[0].Info()
	if err != nil {
		b.Fatal(err)
	}
	b.ReportMetric(float64(info.Size()), "artifact-disk-bytes")
}
