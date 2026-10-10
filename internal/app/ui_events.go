package app

import (
	"context"
	"encoding/json"
)

// UIEvent is a presentation-safe update emitted by the agent while it works.
// It deliberately excludes raw requests, model reasoning, and credentials.
type UIEvent struct {
	Kind    UIEventKind
	Text    string
	Name    string
	Detail  string
	Metrics *UIMetrics
}

// UIMetrics contains safe per-turn provider usage totals.
type UIMetrics struct {
	ContextBytes int
	InputTokens  int64
	OutputTokens int64
}

type UIEventKind string

const (
	UIEventMode             UIEventKind = "mode"
	UIEventCommandOutput    UIEventKind = "command_output"
	UIEventStatus           UIEventKind = "status"
	UIEventUserMessage      UIEventKind = "user_message"
	UIEventAssistantDelta   UIEventKind = "assistant_delta"
	UIEventAssistantMessage UIEventKind = "assistant_message"
	UIEventAssistantDone    UIEventKind = "assistant_done"
	UIEventToolStarted      UIEventKind = "tool_started"
	UIEventToolFinished     UIEventKind = "tool_finished"
	UIEventMetrics          UIEventKind = "metrics"
	UIEventNotice           UIEventKind = "notice"
	UIEventError            UIEventKind = "error"
)

// UIEventSink implementations must return promptly; slow consumers should buffer
// or coalesce updates rather than blocking execution and cancellation.
type UIEventSink interface {
	Emit(UIEvent)
}

type UIEventSinkFunc func(UIEvent)

func (f UIEventSinkFunc) Emit(event UIEvent) {
	f(event)
}

type ApprovalKind string

const (
	ApprovalChanges ApprovalKind = "changes"
	ApprovalCommand ApprovalKind = "command"
)

// ApprovalRequest contains the complete user-visible operation before it is
// applied. UI implementations must default to rejection when unavailable.
type ApprovalRequest struct {
	Source         string          `json:"source,omitempty"`
	WorkspaceState json.RawMessage `json:"workspace_state,omitempty"`
	Kind           ApprovalKind
	Title          string
	Detail         string
	Prompt         string
}

type ApprovalFunc func(context.Context, ApprovalRequest) bool

// ApprovalPresenter shows an operation that was approved without requiring a
// confirmation response, such as an operation approved by --auto-approve.
type ApprovalPresenter func(ApprovalRequest)
