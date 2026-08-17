package main

import "context"

// UIEvent is a presentation-safe update emitted by the agent while it works.
// It deliberately excludes raw requests, model reasoning, and credentials.
type UIEvent struct {
	Kind    UIEventKind
	Text    string
	Name    string
	Detail  string
	Metrics *UIMetrics
}

// UIMetrics contains safe per-turn progress and provider usage totals.
type UIMetrics struct {
	InferenceSteps int
	InferenceLimit int
	ToolCalls      int
	ToolCallLimit  int
	ContextBytes   int
	InputTokens    int64
	OutputTokens   int64
}

type UIEventKind string

const (
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
	ApprovalChanges ApprovalKind = "changes"
	ApprovalCommand ApprovalKind = "command"
)

// ApprovalRequest contains the complete user-visible operation before it is
// applied. UI implementations must default to rejection when unavailable.
type ApprovalRequest struct {
	Kind   ApprovalKind
	Title  string
	Detail string
	Prompt string
}

type ApprovalFunc func(context.Context, ApprovalRequest) bool

// ApprovalPresenter shows an operation that was approved without requiring a
// confirmation response, such as an operation approved by --yes.
type ApprovalPresenter func(ApprovalRequest)
