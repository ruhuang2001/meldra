package app

import (
	"context"
	"encoding/json"
)

type UIEvent struct {
	TaskID, RunID, ToolCallID string
	Status                    string
	DurationMS                int64
	TurnIndex                 int64
	Truncated                 bool
	Error                     string
	Kind                      UIEventKind
	Text                      string
	Name                      string
	Detail                    string
	Metrics                   *UIMetrics
}

type UIMetrics struct {
	ContextBytes       int
	InputTokens        int64
	OutputTokens       int64
	ContextTokens      int
	ContextLimitTokens int
}

type UIEventKind string

const (
	UIEventTurnStart        UIEventKind = "turn_start"
	UIEventTurnEnd          UIEventKind = "turn_end"
	UIEventMode             UIEventKind = "mode"
	UIEventModel            UIEventKind = "model"
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

type UIEventSink interface {
	Emit(UIEvent)
}

type UIEventSinkFunc func(UIEvent)

func (f UIEventSinkFunc) Emit(event UIEvent) {
	f(event)
}

type ApprovalKind string

const (
	ApprovalChanges      ApprovalKind = "changes"
	ApprovalCommand      ApprovalKind = "command"
	ApprovalExternalRead ApprovalKind = "external_read"
)

type ApprovalRequest struct {
	MCPServer      string          `json:"mcp_server,omitempty"`
	MCPBinding     string          `json:"mcp_binding,omitempty"`
	Source         string          `json:"source,omitempty"`
	WorkspaceState json.RawMessage `json:"workspace_state,omitempty"`
	Kind           ApprovalKind
	Title          string
	Detail         string
	Prompt         string
}

type ApprovalFunc func(context.Context, ApprovalRequest) bool

type ApprovalPresenter func(ApprovalRequest)
