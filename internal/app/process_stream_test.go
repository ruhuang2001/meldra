package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"meldra/internal/task"
)

func TestActualCommandEmitsQuietTrailingOutputBeforeExit(t *testing.T) {
	for _, managed := range []bool{false, true} {
		name := "synchronous"
		if managed {
			name = "managed"
		}
		t.Run(name, func(t *testing.T) {
			w, registry, executable := managedWorkspace(t)
			updates := make(chan CommandOutput, 16)
			w.SetCommandOutput(func(update CommandOutput) { updates <- update })
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			var processID string
			if managed {
				view := invokeProcess(t, registry, "start_process", helperInput(executable, "quiet-tail", 10))
				processID = view.Process.ID
			} else {
				go func() {
					raw, _ := json.Marshal(helperInput(executable, "quiet-tail", 10))
					_, err := registry.Invoke(ctx, "run_command", raw)
					done <- err
				}()
			}
			var output strings.Builder
			deadline := time.NewTimer(3 * time.Second)
			defer deadline.Stop()
			for !strings.Contains(output.String(), "quiet-tail中") {
				select {
				case update := <-updates:
					if update.ProcessID != processID {
						t.Fatalf("wrong output owner: %+v", update)
					}
					output.WriteString(update.Text)
				case <-deadline.C:
					t.Fatalf("quiet live command omitted trailing output: %q", output.String())
				case err := <-done:
					t.Fatalf("command exited before quiet output: %v", err)
				}
			}
			if managed {
				view := invokeProcess(t, registry, "process_status", map[string]any{"process_id": processID})
				if view.Process.State != task.ProcessRunning {
					t.Fatalf("quiet output only emitted after exit: %+v", view)
				}
				_ = invokeProcess(t, registry, "stop_process", map[string]any{"process_id": processID})
			} else {
				cancel()
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("synchronous command did not reap after cancellation")
				}
			}
		})
	}
}
