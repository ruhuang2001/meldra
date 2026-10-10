// Package task defines durable foreground execution records. It depends on
// neither model SDKs nor terminal/UI packages and never executes side effects.
package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const SchemaVersion = 1

// DatabaseSchemaVersion advances independently of the stable event envelope.
const DatabaseSchemaVersion = 2

type Status string
type RunStatus string
type ToolStatus string
type Effect string
type Decision string

const (
	Queued          Status = "queued"
	Running         Status = "running"
	WaitingApproval Status = "waiting_approval"
	Completed       Status = "completed"
	Failed          Status = "failed"
	Cancelled       Status = "cancelled"
	Interrupted     Status = "interrupted"

	RunRunning         RunStatus = "running"
	RunWaitingApproval RunStatus = "waiting_approval"
	RunSucceeded       RunStatus = "succeeded"
	RunFailed          RunStatus = "failed"
	RunCancelled       RunStatus = "cancelled"
	RunInterrupted     RunStatus = "interrupted"

	ToolPlanned   ToolStatus = "planned"
	ToolRunning   ToolStatus = "running"
	ToolSucceeded ToolStatus = "succeeded"
	ToolFailed    ToolStatus = "failed"
	ToolDeclined  ToolStatus = "declined"
	ToolCancelled ToolStatus = "cancelled"
	ToolUnknown   ToolStatus = "unknown"

	Read    Effect = "read"
	Write   Effect = "write"
	Command Effect = "command"

	Pending  Decision = "pending"
	Approved Decision = "approved"
	Declined Decision = "declined"
	Expired  Decision = "expired"
)

var (
	ErrTransition    = errors.New("invalid state transition")
	ErrNotFound      = errors.New("task record not found")
	ErrBusy          = errors.New("task or workspace already executing")
	ErrUnresolved    = errors.New("task has unresolved tool outcomes")
	ErrLease         = errors.New("execution ownership is not held")
	ErrLimit         = errors.New("task storage limit exceeded")
	ErrArtifactLimit = fmt.Errorf("%w: artifact quota", ErrLimit)
)

type Task struct {
	ID                   string    `json:"id"`
	Goal                 string    `json:"goal"`
	Workspace            string    `json:"workspace"`
	SessionID            string    `json:"session_id"`
	Status               Status    `json:"status"`
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`
	LegacyHistoryMissing bool      `json:"legacy_history_missing,omitzero"`
}

// Config records reproducible non-secret configuration only. Provider is an
// identifier, never a URL carrying credentials or a serialized SDK client.
type Config struct {
	Model              string `json:"model"`
	Provider           string `json:"provider"`
	Workspace          string `json:"workspace"`
	Mode               string `json:"mode,omitempty"`
	Permissions        string `json:"permissions,omitempty"`
	PolicyGeneration   uint64 `json:"policy_generation,omitzero"`
	ApprovedPlanDigest string `json:"approved_plan_digest,omitempty"`
}

type Run struct {
	ID        string    `json:"id"`
	TaskID    string    `json:"task_id"`
	Status    RunStatus `json:"status"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at,omitzero"`
	Reason    string    `json:"reason,omitempty"`
	Config    Config    `json:"config"`
	Executor  string    `json:"executor"`
}

type ArtifactRef struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type Result struct {
	Status     ToolStatus    `json:"status"`
	Output     string        `json:"output,omitempty"`
	ExitCode   *int          `json:"exit_code,omitempty"`
	DurationMS int64         `json:"duration_ms,omitzero"`
	Truncated  bool          `json:"truncated,omitzero"`
	Retryable  bool          `json:"retryable,omitzero"`
	Error      string        `json:"error,omitempty"`
	Artifacts  []ArtifactRef `json:"artifacts,omitempty"`
}

type ToolCall struct {
	ID             string          `json:"id"`
	TaskID         string          `json:"task_id"`
	RunID          string          `json:"run_id"`
	ProviderCallID string          `json:"provider_call_id,omitempty"`
	ReplayScope    string          `json:"replay_scope,omitempty"`
	Name           string          `json:"name"`
	Arguments      json.RawMessage `json:"arguments"`
	ParameterHash  string          `json:"parameter_hash"`
	Effect         Effect          `json:"effect"`
	Status         ToolStatus      `json:"status"`
	PlannedAt      time.Time       `json:"planned_at"`
	StartedAt      time.Time       `json:"started_at,omitzero"`
	EndedAt        time.Time       `json:"ended_at,omitzero"`
	Result         Result          `json:"result"`
}

type Approval struct {
	ID             string          `json:"id"`
	TaskID         string          `json:"task_id"`
	RunID          string          `json:"run_id"`
	ToolCallID     string          `json:"tool_call_id"`
	Operation      string          `json:"operation"`
	ParameterHash  string          `json:"parameter_hash"`
	WorkspaceState json.RawMessage `json:"workspace_state,omitempty"`
	Detail         json.RawMessage `json:"detail,omitempty"`
	Decision       Decision        `json:"decision"`
	Scope          string          `json:"scope"`
	CreatedAt      time.Time       `json:"created_at"`
	DecidedAt      time.Time       `json:"decided_at,omitzero"`
}

type Event struct {
	SchemaVersion int             `json:"schema_version"`
	ID            string          `json:"id"`
	Sequence      int64           `json:"sequence"`
	TaskID        string          `json:"task_id"`
	RunID         string          `json:"run_id,omitempty"`
	ToolCallID    string          `json:"tool_call_id,omitempty"`
	Kind          string          `json:"kind"`
	Status        string          `json:"status,omitempty"`
	Reason        string          `json:"reason,omitempty"`
	Time          time.Time       `json:"time"`
	Data          json.RawMessage `json:"data,omitempty"`
}

func (s RunStatus) Terminal() bool {
	return s == RunSucceeded || s == RunFailed || s == RunCancelled || s == RunInterrupted
}

func RunTransition(from, to RunStatus) error {
	if (from == RunRunning || from == RunWaitingApproval) && (to.Terminal() || from == RunRunning && to == RunWaitingApproval || from == RunWaitingApproval && to == RunRunning) {
		return nil
	}
	return fmt.Errorf("%w: run %s -> %s", ErrTransition, from, to)
}

func ToolTransition(from, to ToolStatus) error {
	if from == ToolPlanned && (to == ToolRunning || to == ToolDeclined || to == ToolCancelled) || from == ToolRunning && (to == ToolSucceeded || to == ToolFailed || to == ToolDeclined || to == ToolCancelled || to == ToolUnknown) {
		return nil
	}
	return fmt.Errorf("%w: tool %s -> %s", ErrTransition, from, to)
}

func StatusForRun(status RunStatus) Status {
	switch status {
	case RunRunning:
		return Running
	case RunWaitingApproval:
		return WaitingApproval
	case RunSucceeded:
		return Completed
	case RunFailed:
		return Failed
	case RunCancelled:
		return Cancelled
	default:
		return Interrupted
	}
}
