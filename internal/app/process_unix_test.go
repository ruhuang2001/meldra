//go:build darwin || linux

package app

import (
	"strings"
	"testing"
	"time"

	"meldra/internal/task"
)

func TestManagedProcessTERMAndKILLEscalation(t *testing.T) {
	for _, mode := range []string{"cooperative-term", "ignore-term"} {
		t.Run(mode, func(t *testing.T) {
			_, registry, executable := managedWorkspace(t)
			view := invokeProcess(t, registry, "start_process", helperInput(executable, mode, 10))
			deadline := time.Now().Add(5 * time.Second)
			for !strings.Contains(view.Output, "ready") && time.Now().Before(deadline) {
				view = invokeProcess(t, registry, "wait_process", map[string]any{"process_id": view.Process.ID, "wait_ms": 50})
			}
			if !strings.Contains(view.Output, "ready") {
				t.Fatal("signal helper did not start")
			}
			started := time.Now()
			view = invokeProcess(t, registry, "stop_process", map[string]any{"process_id": view.Process.ID})
			if time.Since(started) > 4*time.Second || view.Process.State != task.ProcessStopped || view.Process.Effects != "unknown" {
				t.Fatalf("stop did not settle: %+v", view)
			}
			if mode == "cooperative-term" && !strings.Contains(view.Output, "term received") {
				t.Fatal("TERM was not delivered before KILL")
			}
			if mode == "ignore-term" && view.Process.Signal != "killed" {
				t.Fatalf("ignored TERM not escalated: %+v", view.Process)
			}
		})
	}
}
