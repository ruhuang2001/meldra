package task

import "time"

type ProcessState string

const (
	ProcessStarting ProcessState = "starting"
	ProcessRunning  ProcessState = "running"
	ProcessExited   ProcessState = "exited"
	ProcessStopped  ProcessState = "stopped"
	ProcessTimedOut ProcessState = "timed_out"
	ProcessFailed   ProcessState = "failed"
	ProcessUnknown  ProcessState = "unknown"
)

// Process is independent of its successful start tool call. PID is historical
// evidence only: it must never be used to signal or reconnect after recovery.
type Process struct {
	ID               string        `json:"id"`
	TaskID           string        `json:"task_id"`
	RunID            string        `json:"run_id"`
	ToolCallID       string        `json:"tool_call_id"`
	OwnerEpoch       string        `json:"owner_epoch"`
	LaunchHash       string        `json:"launch_hash"`
	Executable       string        `json:"executable"`
	Arguments        []string      `json:"arguments,omitempty"`
	Directory        string        `json:"directory"`
	TimeoutSeconds   int           `json:"timeout_seconds"`
	State            ProcessState  `json:"state"`
	Effects          string        `json:"effects"` // pending, known, unknown, or resolved
	PID              int           `json:"pid,omitzero"`
	StartedAt        time.Time     `json:"started_at"`
	EndedAt          time.Time     `json:"ended_at,omitzero"`
	ExitCode         *int          `json:"exit_code,omitempty"`
	Signal           string        `json:"signal,omitempty"`
	Error            string        `json:"error,omitempty"`
	OutputStart      int64         `json:"output_start,omitzero"`
	OutputEnd        int64         `json:"output_end,omitzero"`
	Truncated        bool          `json:"truncated,omitzero"`
	Artifacts        []ArtifactRef `json:"artifacts,omitempty"`
	ResolutionReason string        `json:"resolution_reason,omitempty"`
	ResolvedAt       time.Time     `json:"resolved_at,omitzero"`
}

func (s ProcessState) Terminal() bool {
	return s == ProcessExited || s == ProcessStopped || s == ProcessTimedOut || s == ProcessFailed || s == ProcessUnknown
}
