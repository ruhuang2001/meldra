//go:build darwin || linux

package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	taskstore "meldra/internal/store"
	"meldra/internal/task"
	"meldra/internal/tool"
)

func TestManagedProcessRealChildPersistenceWindows(t *testing.T) {
	for _, window := range []string{"after_spawn", "after_exit"} {
		t.Run(window, func(t *testing.T) {
			agent, paths := recordedAgent(t, nil, nil)
			e, w := agent.execution, agent.execution.workspace
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			if err := w.SetCommandExecutables([]string{executable}); err != nil {
				t.Fatal(err)
			}
			if err := e.begin(t.Context(), "fault injection with real child"); err != nil {
				t.Fatal(err)
			}
			if err := w.beginProcesses(t.Context(), e); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = w.closeProcesses(); _ = e.close() })
			m := w.processes
			persist := m.persistProcess
			injected := errors.New("injected process persistence failure: " + window)
			var injectedCount atomic.Int32
			var childPID atomic.Int64
			ready := make(chan struct{}, 1)
			w.SetCommandOutput(func(output CommandOutput) {
				if strings.Contains(output.Text, "counted child ready") {
					select {
					case ready <- struct{}{}:
					default:
					}
				}
			})
			m.persistProcess = func(ctx context.Context, lease *taskstore.Lease, p task.Process) error {
				if p.State == task.ProcessRunning {
					childPID.Store(int64(p.PID))
				}
				if window == "after_spawn" && p.State == task.ProcessRunning {
					// Require execution beyond fork: the child has written its marker
					// and emitted output before losing durable running evidence.
					select {
					case <-ready:
					case <-time.After(5 * time.Second):
						return errors.New("real child did not reach ready state")
					}
					injectedCount.Add(1)
					return injected
				}
				if window == "after_exit" && p.State == task.ProcessExited {
					injectedCount.Add(1)
					return injected
				}
				return persist(ctx, lease, p)
			}
			registry, err := tool.New(w.ProcessToolDefinitions())
			if err != nil {
				t.Fatal(err)
			}
			mode := "counted-wait"
			if window == "after_exit" {
				mode = "counted-exit"
			}
			raw, _ := json.Marshal(helperInput(executable, mode, 20))
			result, startErr := e.invoke(t.Context(), registry, "launch", "start_process", raw)
			if window == "after_spawn" && (!errors.Is(startErr, injected) || result.Status != tool.Unknown) {
				t.Fatalf("post-spawn persistence failure hidden: %+v %v", result, startErr)
			}
			if window == "after_exit" && startErr != nil {
				t.Fatalf("launch should have its own successful result: %+v %v", result, startErr)
			}
			m.mu.Lock()
			var process *managedProcess
			for _, p := range m.processes {
				process = p
			}
			m.mu.Unlock()
			if process == nil {
				t.Fatal("child never started")
			}
			select {
			case <-process.done:
			case <-time.After(7 * time.Second):
				t.Fatal("failed persistence left live child or worker")
			}
			if injectedCount.Load() != 1 || childPID.Load() <= 0 {
				t.Fatalf("fault was not reached once: injections=%d pid=%d", injectedCount.Load(), childPID.Load())
			}
			if err := syscall.Kill(int(childPID.Load()), 0); !errors.Is(err, syscall.ESRCH) {
				t.Fatalf("owned child not reaped: pid=%d err=%v", childPID.Load(), err)
			}
			stored, err := e.db.GetProcess(t.Context(), process.record.ID)
			if err != nil {
				t.Fatal(err)
			}
			if window == "after_spawn" && (stored.State != task.ProcessStopped || stored.Effects != "unknown") {
				t.Fatalf("post-spawn cleanup not durable: %+v", stored)
			}
			if window == "after_exit" && (stored.State != task.ProcessRunning || stored.ExitCode != nil) {
				t.Fatalf("uncommitted completion invented: %+v", stored)
			}
			// Even after process cleanup, ownership belongs to the unfinished Run.
			observer, err := taskstore.Open(taskDirectory(paths))
			if err != nil {
				t.Fatal(err)
			}
			defer observer.Close()
			if lease, err := observer.Acquire(t.Context(), taskstore.NewID(), w.root); !errors.Is(err, task.ErrBusy) {
				if lease != nil {
					_ = lease.Close()
				}
				t.Fatalf("ownership released before outcome handling: %v", err)
			}
			cleanupErr := w.closeProcesses()
			if !errors.Is(cleanupErr, injected) {
				t.Fatalf("persistence error lost during cleanup: %v", cleanupErr)
			}
			if err := e.finish(t.Context(), errors.Join(startErr, cleanupErr)); err != nil {
				t.Fatal(err)
			}
			stored, err = observer.GetProcess(t.Context(), stored.ID)
			if err != nil || stored.Effects != "unknown" {
				t.Fatalf("lost unresolved process evidence: %+v %v", stored, err)
			}
			if window == "after_exit" && (stored.State != task.ProcessUnknown || stored.ExitCode != nil) {
				t.Fatalf("failed final save invented exit: %+v", stored)
			}
			lease, err := observer.Acquire(t.Context(), agent.session.ID, w.root)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Close()
			if err := observer.RecoverInterrupted(t.Context(), lease, agent.session.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := observer.StartRun(t.Context(), lease, task.Run{TaskID: agent.session.ID}); !errors.Is(err, task.ErrUnresolved) {
				t.Fatalf("unknown process auto-resumed: %v", err)
			}
			count, err := os.ReadFile(filepath.Join(w.root, "spawn-count.txt"))
			if err != nil || string(count) != "spawned\n" {
				t.Fatalf("recovery relaunched child: %q %v", count, err)
			}
			calls, err := observer.ToolCalls(t.Context(), agent.session.ID)
			if err != nil || len(calls) != 1 {
				t.Fatalf("calls=%+v %v", calls, err)
			}
			if window == "after_exit" && calls[0].Status != task.ToolSucceeded {
				t.Fatalf("process outcome overwrote successful launch: %+v", calls[0])
			}
		})
	}
}
