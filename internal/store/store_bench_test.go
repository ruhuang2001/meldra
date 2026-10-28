package store

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"meldra/internal/task"
)

// BenchmarkToolLifecycle benchmarks the complete lifecycle of a tool call.
// This measures the overhead of task storage and SQLite transactions.
func BenchmarkToolLifecycle(b *testing.B) {
	s, err := Open(filepath.Join(b.TempDir(), "bench"))
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()

	workspace := b.TempDir()
	ctx := context.Background()
	t, err := s.EnsureTask(ctx, task.Task{ID: NewID(), Goal: "benchmark", Workspace: workspace})
	if err != nil {
		b.Fatal(err)
	}
	l, err := s.Acquire(ctx, t.ID, workspace)
	if err != nil {
		b.Fatal(err)
	}
	defer l.Close()

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		run, err := s.StartRun(ctx, l, task.Run{TaskID: t.ID, Config: task.Config{Model: "bench", Provider: "test"}})
		if err != nil {
			b.Fatal(err)
		}

		call, err := s.PlanTool(ctx, l, task.ToolCall{
			TaskID:    t.ID,
			RunID:     run.ID,
			Name:      "read_file",
			Effect:    task.Read,
			Arguments: json.RawMessage(`{"path":"test.go"}`),
		})
		if err != nil {
			b.Fatal(err)
		}

		if err := s.StartTool(ctx, l, call.ID); err != nil {
			b.Fatal(err)
		}

		if err := s.FinishTool(ctx, l, call.ID, task.Result{Status: task.ToolSucceeded, Output: "ok"}); err != nil {
			b.Fatal(err)
		}

		if err := s.FinishRun(ctx, l, run.ID, task.RunSucceeded, "completed"); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkMultipleToolCalls benchmarks a run with multiple tool calls.
func BenchmarkMultipleToolCalls(b *testing.B) {
	s, err := Open(filepath.Join(b.TempDir(), "bench"))
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()

	workspace := b.TempDir()
	ctx := context.Background()
	t, err := s.EnsureTask(ctx, task.Task{ID: NewID(), Goal: "benchmark", Workspace: workspace})
	if err != nil {
		b.Fatal(err)
	}
	l, err := s.Acquire(ctx, t.ID, workspace)
	if err != nil {
		b.Fatal(err)
	}
	defer l.Close()

	const toolsPerRun = 10

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		run, _ := s.StartRun(ctx, l, task.Run{TaskID: t.ID, Config: task.Config{Model: "bench", Provider: "test"}})

		for j := 0; j < toolsPerRun; j++ {
			call, _ := s.PlanTool(ctx, l, task.ToolCall{
				TaskID:    t.ID,
				RunID:     run.ID,
				Name:      fmt.Sprintf("tool_%d", j),
				Effect:    task.Read,
				Arguments: json.RawMessage(`{}`),
			})
			s.StartTool(ctx, l, call.ID)
			s.FinishTool(ctx, l, call.ID, task.Result{Status: task.ToolSucceeded})
		}

		s.FinishRun(ctx, l, run.ID, task.RunSucceeded, "completed")
	}
}

// BenchmarkEventAppend benchmarks event append performance.
func BenchmarkEventAppend(b *testing.B) {
	s, err := Open(filepath.Join(b.TempDir(), "bench"))
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()

	workspace := b.TempDir()
	ctx := context.Background()
	t, err := s.EnsureTask(ctx, task.Task{ID: NewID(), Goal: "benchmark", Workspace: workspace})
	if err != nil {
		b.Fatal(err)
	}
	l, err := s.Acquire(ctx, t.ID, workspace)
	if err != nil {
		b.Fatal(err)
	}
	defer l.Close()

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		err := s.AppendEvent(ctx, l, task.Event{
			TaskID: t.ID,
			Kind:   "benchmark",
			Data:   json.RawMessage(`{"iteration":` + fmt.Sprintf("%d", i) + `}`),
		})
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkEventPagination benchmarks reading events with pagination.
func BenchmarkEventPagination(b *testing.B) {
	s, err := Open(filepath.Join(b.TempDir(), "bench"))
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()

	workspace := b.TempDir()
	ctx := context.Background()
	t, err := s.EnsureTask(ctx, task.Task{ID: NewID(), Goal: "benchmark", Workspace: workspace})
	if err != nil {
		b.Fatal(err)
	}
	l, err := s.Acquire(ctx, t.ID, workspace)
	if err != nil {
		b.Fatal(err)
	}

	// Prepare: insert 1000 events
	for i := 0; i < 1000; i++ {
		s.AppendEvent(ctx, l, task.Event{TaskID: t.ID, Kind: "test"})
	}
	l.Close()

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		cursor := int64(0)
		for {
			events, err := s.Events(ctx, t.ID, cursor, 100)
			if err != nil {
				b.Fatal(err)
			}
			if len(events) == 0 {
				break
			}
			cursor = events[len(events)-1].Sequence
		}
	}
}

// BenchmarkTaskList benchmarks listing tasks in a workspace.
func BenchmarkTaskList(b *testing.B) {
	s, err := Open(filepath.Join(b.TempDir(), "bench"))
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()

	workspace := b.TempDir()
	ctx := context.Background()

	// Prepare: create 50 tasks
	for i := 0; i < 50; i++ {
		s.EnsureTask(ctx, task.Task{
			ID:        NewID(),
			Goal:      fmt.Sprintf("task %d", i),
			Workspace: workspace,
		})
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		tasks, err := s.ListTasks(ctx, workspace, 50)
		if err != nil {
			b.Fatal(err)
		}
		if len(tasks) == 0 {
			b.Fatal("no tasks returned")
		}
	}
}

// BenchmarkToolLookup benchmarks looking up tool calls by ID.
// This tests the performance of the tool call index.
func BenchmarkToolLookup(b *testing.B) {
	s, err := Open(filepath.Join(b.TempDir(), "bench"))
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()

	workspace := b.TempDir()
	ctx := context.Background()
	t, err := s.EnsureTask(ctx, task.Task{ID: NewID(), Goal: "benchmark", Workspace: workspace})
	if err != nil {
		b.Fatal(err)
	}
	l, err := s.Acquire(ctx, t.ID, workspace)
	if err != nil {
		b.Fatal(err)
	}
	defer l.Close()

	run, _ := s.StartRun(ctx, l, task.Run{
		TaskID: t.ID,
		Config: task.Config{Model: "bench", Provider: "test"},
	})

	// Create 100 tool calls
	callIDs := make([]string, 100)
	for i := 0; i < 100; i++ {
		call, _ := s.PlanTool(ctx, l, task.ToolCall{
			TaskID:    t.ID,
			RunID:     run.ID,
			Name:      fmt.Sprintf("tool_%d", i),
			Effect:    task.Read,
			Arguments: json.RawMessage(`{}`),
		})
		s.StartTool(ctx, l, call.ID)
		s.FinishTool(ctx, l, call.ID, task.Result{Status: task.ToolSucceeded})
		callIDs[i] = call.ID
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		// Look up a random call by ID
		callID := callIDs[i%100]
		existing, err := s.GetToolCall(ctx, callID)
		if err != nil {
			b.Fatalf("lookup failed: %v", err)
		}
		if existing.ID != callID {
			b.Fatalf("wrong call returned: got %s, want %s", existing.ID, callID)
		}
	}
}

// BenchmarkApprovalFlow benchmarks the approval record and decision flow.
func BenchmarkApprovalFlow(b *testing.B) {
	s, err := Open(filepath.Join(b.TempDir(), "bench"))
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()

	workspace := b.TempDir()
	ctx := context.Background()
	t, err := s.EnsureTask(ctx, task.Task{ID: NewID(), Goal: "benchmark", Workspace: workspace})
	if err != nil {
		b.Fatal(err)
	}
	l, err := s.Acquire(ctx, t.ID, workspace)
	if err != nil {
		b.Fatal(err)
	}
	defer l.Close()

	run, _ := s.StartRun(ctx, l, task.Run{TaskID: t.ID, Config: task.Config{Model: "bench", Provider: "test"}})

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		call, _ := s.PlanTool(ctx, l, task.ToolCall{
			TaskID:    t.ID,
			RunID:     run.ID,
			Name:      "write_file",
			Effect:    task.Write,
			Arguments: json.RawMessage(`{"path":"test.go"}`),
		})

		approval, _ := s.RecordApproval(ctx, l, task.Approval{
			TaskID:         t.ID,
			RunID:          run.ID,
			ToolCallID:     call.ID,
			Operation:      "write",
			ParameterHash:  call.ParameterHash,
			WorkspaceState: json.RawMessage(`{"before":"abc"}`),
		})

		s.DecideApproval(ctx, l, approval.ID, task.Approved)
		s.StartTool(ctx, l, call.ID)
		s.FinishTool(ctx, l, call.ID, task.Result{Status: task.ToolSucceeded})
	}
}
