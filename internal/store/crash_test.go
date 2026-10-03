package store

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"meldra/internal/task"
)

func TestIndependentTaskWritersSerializeTransactions(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	other, err := Open(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	var wg sync.WaitGroup
	errors := make(chan error, 2)
	for i, db := range []*Store{s, other} {
		workspace := t.TempDir()
		wg.Go(func() {
			id := fmt.Sprintf("task-%d", i)
			if _, err := db.EnsureTask(t.Context(), task.Task{ID: id, Goal: "parallel independent workspace", Workspace: workspace}); err != nil {
				errors <- err
				return
			}
			l, err := db.Acquire(t.Context(), id, workspace)
			if err != nil {
				errors <- err
				return
			}
			defer l.Close()
			for range 25 {
				if err := db.AppendEvent(t.Context(), l, task.Event{TaskID: id, Kind: "progress"}); err != nil {
					errors <- err
					return
				}
			}
		})
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		t.Fatal(err)
	}
	for i := range 2 {
		events, err := s.Events(t.Context(), fmt.Sprintf("task-%d", i), 0, 100)
		if err != nil || len(events) != 26 {
			t.Fatalf("concurrent events %d: %d %v", i, len(events), err)
		}
		for n, event := range events {
			if event.Sequence != int64(n+1) {
				t.Fatal("non-monotonic sequence")
			}
		}
	}
}

func TestUncommittedProcessCrashHelper(t *testing.T) {
	if os.Getenv("MELDRA_TX_HELPER") != "1" {
		return
	}
	s, err := Open(os.Getenv("MELDRA_TX_STORE"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(3)
	}
	_, err = s.EnsureTask(context.Background(), task.Task{ID: "crash-task", Goal: "commit fault", Workspace: os.Getenv("MELDRA_TX_WORKSPACE")})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(4)
	}
	l, err := s.Acquire(context.Background(), "crash-task", os.Getenv("MELDRA_TX_WORKSPACE"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(5)
	}
	s.beforeCommit = func() error {
		if err := os.WriteFile(os.Getenv("MELDRA_TX_READY"), []byte("transaction-written"), 0600); err != nil {
			os.Exit(6)
		}
		for {
			time.Sleep(time.Hour)
		}
	}
	_ = s.AppendEvent(context.Background(), l, task.Event{TaskID: "crash-task", Kind: "must_not_survive"})
	os.Exit(7)
}

func TestKilledTransactionDoesNotPersistPartialEvent(t *testing.T) {
	dir := t.TempDir()
	workspace := t.TempDir()
	ready := filepath.Join(t.TempDir(), "ready")
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestUncommittedProcessCrashHelper$")
	cmd.Env = append(os.Environ(), "MELDRA_TX_HELPER=1", "MELDRA_TX_STORE="+dir, "MELDRA_TX_WORKSPACE="+workspace, "MELDRA_TX_READY="+ready)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child failed to reach commit boundary")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	events, err := s.Events(t.Context(), "crash-task", 0, 100)
	if err != nil || len(events) != 1 || events[0].Kind != "task.created" {
		t.Fatalf("uncommitted event survived: %+v %v", events, err)
	}
	l, err := s.Acquire(t.Context(), "crash-task", workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := s.AppendEvent(t.Context(), l, task.Event{TaskID: "crash-task", Kind: "recovered"}); err != nil {
		t.Fatal(err)
	}
	events, err = s.Events(t.Context(), "crash-task", 0, 100)
	if err != nil || len(events) != 2 || events[1].Sequence != 2 {
		t.Fatal("rollback left broken sequence")
	}
}
