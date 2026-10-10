package task

import (
	"errors"
	"testing"
)

func TestRunTransitions(t *testing.T) {
	states := []RunStatus{RunRunning, RunWaitingApproval, RunSucceeded, RunFailed, RunCancelled, RunInterrupted, "invalid"}
	for _, from := range states {
		for _, to := range states {
			valid := (from == RunRunning || from == RunWaitingApproval) && (to.Terminal() || from == RunRunning && to == RunWaitingApproval || from == RunWaitingApproval && to == RunRunning)
			err := RunTransition(from, to)
			if (err == nil) != valid {
				t.Errorf("transition %s -> %s: %v", from, to, err)
			}
			if err != nil && !errors.Is(err, ErrTransition) {
				t.Errorf("missing typed transition error: %v", err)
			}
		}
	}
}

func TestToolTransitions(t *testing.T) {
	states := []ToolStatus{ToolPlanned, ToolRunning, ToolSucceeded, ToolFailed, ToolDeclined, ToolCancelled, ToolUnknown, "invalid"}
	for _, from := range states {
		for _, to := range states {
			valid := from == ToolPlanned && (to == ToolRunning || to == ToolDeclined || to == ToolCancelled) || from == ToolRunning && (to == ToolSucceeded || to == ToolFailed || to == ToolDeclined || to == ToolCancelled || to == ToolUnknown)
			err := ToolTransition(from, to)
			if (err == nil) != valid {
				t.Errorf("transition %s -> %s: %v", from, to, err)
			}
			if err != nil && !errors.Is(err, ErrTransition) {
				t.Errorf("missing typed transition error: %v", err)
			}
		}
	}
	for run, status := range map[RunStatus]Status{RunRunning: Running, RunWaitingApproval: WaitingApproval, RunSucceeded: Completed, RunFailed: Failed, RunCancelled: Cancelled, RunInterrupted: Interrupted} {
		if got := StatusForRun(run); got != status {
			t.Errorf("%s: %s != %s", run, got, status)
		}
	}
}
