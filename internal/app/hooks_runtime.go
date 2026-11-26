package app

import (
	"cmp"
	"context"
	"encoding/json"
	"strings"

	"meldra/internal/task"
	"meldra/internal/tool"
)

type runEndSnapshot struct {
	taskID           string
	runID            string
	taskStatus       task.Status
	runStatus        task.RunStatus
	reason           string
	unknownToolCalls int
}

func (a *Agent) hookBasePayload(eventName string) hookPayload {
	mode, permissions, _ := a.policy.snapshot()
	autoApprove := a.workspace != nil && a.workspace.autoApprove
	cwd := ""
	if a.workspace != nil {
		cwd = a.workspace.root
	}
	sessionID := ""
	if a.session != nil {
		sessionID = a.session.ID
	}
	runID := ""
	if a.execution != nil && a.execution.run.TaskID == sessionID {
		runID = a.execution.run.ID
	}
	return hookPayload{
		HookEventName:  eventName,
		SessionID:      sessionID,
		Cwd:            cwd,
		PermissionMode: hookPermissionMode(mode, permissions, autoApprove),
		Meldra: hookMeldraMeta{
			SchemaVersion: 1,
			Mode:          string(mode),
			Permissions:   string(permissions),
			Surface:       a.hookSurface,
			TaskID:        sessionID,
			RunID:         runID,
		},
	}
}

func (a *Agent) hookDispatchOpts(matchTarget string) hookDispatchOptions {
	return hookDispatchOptions{
		matchTarget: matchTarget,
		onStatus: func(text string) {
			a.emit(UIEvent{Kind: UIEventStatus, Text: text})
		},
		onNotice: func(text string) {
			a.emitNotice(text)
		},
	}
}

func (a *Agent) startSessionHooks(ctx context.Context, source string) {
	a.sessionStarted = true
	if a.hooks == nil {
		return
	}
	payload := a.hookBasePayload(hookEventSessionStart)
	payload.Source = cmp.Or(source, "startup")
	outcome := a.hooks.dispatch(ctx, payload, a.hookDispatchOpts(payload.Source))
	a.sessionHookContext = outcome.additionalContext
}

func (a *Agent) endSessionHooks(reason string) {
	if a.hooks == nil || !a.sessionStarted {
		return
	}
	payload := a.hookBasePayload(hookEventSessionEnd)
	payload.Reason = cmp.Or(reason, "other")
	a.hooks.dispatch(context.Background(), payload, hookDispatchOptions{matchTarget: payload.Reason})
}

func (a *Agent) runUserPromptHook(ctx context.Context, prompt string) (hookOutcome, error) {
	if a.hooks == nil || !a.hooks.has(hookEventUserPromptSubmit) {
		return hookOutcome{}, nil
	}
	payload := a.hookBasePayload(hookEventUserPromptSubmit)
	payload.Prompt = prompt
	payload.TurnID = payload.Meldra.RunID
	outcome := a.hooks.dispatch(ctx, payload, a.hookDispatchOpts(""))
	if outcome.blocked {
		if a.execution != nil {
			if err := a.execution.event(ctx, "hook.prompt_blocked", "failed", map[string]string{"reason": truncateSessionMessage(outcome.reason)}); err != nil {
				return outcome, err
			}
		}
		return outcome, &hookBlockedError{event: hookEventUserPromptSubmit, reason: outcome.reason}
	}
	return outcome, nil
}

func (a *Agent) runStopHook(ctx context.Context, stopHookActive bool, lastAssistantMessage string) hookOutcome {
	if a.hooks == nil || !a.hooks.has(hookEventStop) {
		return hookOutcome{}
	}
	payload := a.hookBasePayload(hookEventStop)
	payload.TurnID = payload.Meldra.RunID
	payload.StopHookActive = new(stopHookActive)
	payload.LastAssistantMessage = capText(lastAssistantMessage)
	return a.hooks.dispatch(ctx, payload, a.hookDispatchOpts(""))
}

func (a *Agent) runPreCompactHook(ctx context.Context, trigger, focus string) hookOutcome {
	if a.hooks == nil || !a.hooks.has(hookEventPreCompact) {
		return hookOutcome{}
	}
	hookTrigger := "auto"
	if trigger == "manual" {
		hookTrigger = "manual"
	}
	payload := a.hookBasePayload(hookEventPreCompact)
	payload.Trigger = hookTrigger
	payload.CustomInstructions = focus
	return a.hooks.dispatch(ctx, payload, a.hookDispatchOpts(hookTrigger))
}

func (a *Agent) runPostCompactHook(ctx context.Context, trigger, summary string, fromBytes, toBytes, keptMessages int) {
	if a.hooks == nil || !a.hooks.has(hookEventPostCompact) {
		return
	}
	hookTrigger := "auto"
	if trigger == "manual" {
		hookTrigger = "manual"
	}
	payload := a.hookBasePayload(hookEventPostCompact)
	payload.Trigger = hookTrigger
	payload.CompactSummary = summary
	payload.Meldra.FromBytes = fromBytes
	payload.Meldra.ToBytes = toBytes
	payload.Meldra.KeptMessages = keptMessages
	a.hooks.dispatch(ctx, payload, a.hookDispatchOpts(hookTrigger))
}

func (a *Agent) hookContextInstructions() string {
	var b strings.Builder
	if a.sessionHookContext != "" {
		b.WriteString(frameHookContext(hookEventSessionStart, a.sessionHookContext))
	}
	if a.turnHookContext != "" {
		b.WriteString(frameHookContext(hookEventUserPromptSubmit, a.turnHookContext))
	}
	return b.String()
}

func (e *taskExecution) hookBasePayload(eventName string) hookPayload {
	mode, permissions, _ := e.workspace.policy.snapshot()
	sessionID := ""
	if e.session != nil {
		sessionID = e.session.ID
	}
	return hookPayload{
		HookEventName:  eventName,
		SessionID:      sessionID,
		Cwd:            e.workspace.root,
		PermissionMode: hookPermissionMode(mode, permissions, e.workspace.autoApprove),
		Meldra: hookMeldraMeta{
			SchemaVersion: 1,
			Mode:          string(mode),
			Permissions:   string(permissions),
			Surface:       e.hookSurface,
			TaskID:        sessionID,
			RunID:         e.run.ID,
		},
	}
}

func (e *taskExecution) hookOpts(matchTarget string) hookDispatchOptions {
	return hookDispatchOptions{
		matchTarget: matchTarget,
		onStatus:    e.hookStatus,
		onNotice:    e.hookNotify,
	}
}

func (e *taskExecution) runPreToolHook(ctx context.Context, call task.ToolCall) hookOutcome {
	if e.hooks == nil || !e.hooks.has(hookEventPreToolUse) {
		return hookOutcome{}
	}
	payload := e.hookBasePayload(hookEventPreToolUse)
	payload.ToolName = call.Name
	payload.ToolInput = call.Arguments
	payload.ToolUseID = call.ID
	payload.Meldra.Effect = string(call.Effect)
	return e.hooks.dispatch(ctx, payload, e.hookOpts(call.Name))
}

func (e *taskExecution) runPostToolHook(ctx context.Context, call task.ToolCall, stored task.Result) hookOutcome {
	if e.hooks == nil {
		return hookOutcome{}
	}
	switch stored.Status {
	case task.ToolSucceeded:
		if !e.hooks.has(hookEventPostToolUse) {
			return hookOutcome{}
		}
		payload := e.hookBasePayload(hookEventPostToolUse)
		payload.ToolName = call.Name
		payload.ToolInput = call.Arguments
		payload.ToolUseID = call.ID
		payload.Meldra.Effect = string(call.Effect)
		payload.ToolResponse = &hookToolResponse{
			Status:     string(stored.Status),
			Output:     capText(stored.Output),
			ExitCode:   stored.ExitCode,
			DurationMS: stored.DurationMS,
			Truncated:  stored.Truncated,
		}
		return e.hooks.dispatch(ctx, payload, e.hookOpts(call.Name))
	case task.ToolFailed:
		if !e.hooks.has(hookEventPostToolUseFailure) {
			return hookOutcome{}
		}
		payload := e.hookBasePayload(hookEventPostToolUseFailure)
		payload.ToolName = call.Name
		payload.ToolInput = call.Arguments
		payload.ToolUseID = call.ID
		payload.Meldra.Effect = string(call.Effect)
		payload.Error = stored.Error
		payload.IsInterrupt = new(false)
		payload.ToolResponse = &hookToolResponse{
			Status:     string(stored.Status),
			Output:     capText(stored.Output),
			Error:      stored.Error,
			ExitCode:   stored.ExitCode,
			DurationMS: stored.DurationMS,
			Truncated:  stored.Truncated,
		}
		return e.hooks.dispatch(ctx, payload, e.hookOpts(call.Name))
	default:
		return hookOutcome{}
	}
}

func (e *taskExecution) readRunEndSnapshot(ctx context.Context) (*runEndSnapshot, error) {
	if e.hooks == nil || !e.hooks.has(hookEventRunEnd) || e.db == nil || e.session == nil || e.run.ID == "" {
		return nil, nil
	}
	taskRecord, err := e.db.GetTask(ctx, e.session.ID)
	if err != nil {
		return nil, err
	}
	runRecord, err := e.db.GetRun(ctx, e.run.ID)
	if err != nil {
		return nil, err
	}
	unknownCount := 0
	after := ""
	for {
		calls, err := e.db.UnknownToolCalls(ctx, e.session.ID, after)
		if err != nil {
			return nil, err
		}
		if len(calls) == 0 {
			break
		}
		for _, call := range calls {
			after = call.ID
			if call.Status == task.ToolUnknown {
				unknownCount++
			}
		}
	}
	return &runEndSnapshot{
		taskID:           taskRecord.ID,
		runID:            runRecord.ID,
		taskStatus:       taskRecord.Status,
		runStatus:        runRecord.Status,
		reason:           runRecord.Reason,
		unknownToolCalls: unknownCount,
	}, nil
}

func (e *taskExecution) emitRunEndHook(snapshot runEndSnapshot) {
	if e.hooks == nil || !e.hooks.has(hookEventRunEnd) {
		return
	}
	payload := e.hookBasePayload(hookEventRunEnd)
	payload.SessionID = snapshot.taskID
	payload.Meldra.TaskID = snapshot.taskID
	payload.Meldra.RunID = snapshot.runID
	payload.Meldra.TaskStatus = string(snapshot.taskStatus)
	payload.Meldra.RunStatus = string(snapshot.runStatus)
	payload.Meldra.Reason = snapshot.reason
	payload.Meldra.UnknownToolCalls = new(snapshot.unknownToolCalls)
	e.hooks.dispatch(context.Background(), payload, hookDispatchOptions{
		matchTarget: string(snapshot.runStatus),
		onNotice:    e.hookNotify,
	})
}

func (w *Workspace) checkPermissionHook(ctx context.Context, request ApprovalRequest) (bool, string) {
	if w.hooks == nil || !w.hooks.has(hookEventPermissionRequest) {
		return false, ""
	}
	mode, permissions, _ := w.policy.snapshot()
	payload := hookPayload{
		HookEventName:  hookEventPermissionRequest,
		Cwd:            w.root,
		PermissionMode: hookPermissionMode(mode, permissions, w.autoApprove),
		Meldra: hookMeldraMeta{
			SchemaVersion: 1,
			Mode:          string(mode),
			Permissions:   string(permissions),
			Surface:       w.hookSurface,
			Operation:     string(request.Kind),
			Title:         sanitizeTerminalText(request.Title),
			Detail:        truncateUTF8(sanitizeTerminalText(request.Detail), 4096, "\n[truncated]"),
			Source:        request.Source,
		},
	}
	exec, _ := ctx.Value(executionContextKey{}).(*taskExecution)
	if exec != nil {
		if exec.session != nil {
			payload.SessionID = exec.session.ID
			payload.Meldra.TaskID = exec.session.ID
		}
		payload.Meldra.RunID = exec.run.ID
		if exec.current != nil {
			payload.ToolName = exec.current.Name
			payload.ToolInput = exec.current.Arguments
			payload.ToolUseID = exec.current.ID
			payload.Meldra.Effect = string(exec.current.Effect)
		}
	}
	outcome := w.hooks.dispatch(ctx, payload, hookDispatchOptions{
		matchTarget: payload.ToolName,
		onNotice:    w.hookNotify,
	})
	if outcome.blocked {
		if exec != nil {
			_ = exec.event(ctx, "hook.permission_denied", string(task.ToolDeclined), map[string]string{
				"name":   payload.ToolName,
				"reason": truncateSessionMessage(outcome.reason),
			})
		}
		return true, outcome.reason
	}
	return false, ""
}

func (a *Agent) invokeToolWithoutExecutionHooks(ctx context.Context, callID, name string, input json.RawMessage) (tool.Result, error) {
	if a.hooks == nil {
		return a.executeToolResult(ctx, name, input)
	}
	var preContext string
	if a.hooks.has(hookEventPreToolUse) {
		payload := a.hookBasePayload(hookEventPreToolUse)
		payload.ToolName = name
		payload.ToolInput = input
		payload.ToolUseID = callID
		payload.Meldra.Effect = string(toolEffect(name))
		outcome := a.hooks.dispatch(ctx, payload, a.hookDispatchOpts(name))
		if ctx.Err() != nil {
			return tool.Result{Status: tool.Cancelled, Error: ctx.Err().Error()}, ctx.Err()
		}
		if outcome.blocked {
			return tool.Result{Status: tool.Declined, Output: "Declined by PreToolUse hook: " + outcome.reason}, nil
		}
		preContext = outcome.additionalContext
	}
	result, callErr := a.executeToolResult(ctx, name, input)
	status := result.Status
	if status == "" {
		if callErr != nil {
			status = tool.Failed
		} else {
			status = tool.Succeeded
		}
	}
	if ctx.Err() == nil {
		switch status {
		case tool.Succeeded:
			if a.hooks.has(hookEventPostToolUse) {
				payload := a.hookBasePayload(hookEventPostToolUse)
				payload.ToolName = name
				payload.ToolInput = input
				payload.ToolUseID = callID
				payload.Meldra.Effect = string(toolEffect(name))
				payload.ToolResponse = &hookToolResponse{
					Status:     status,
					Output:     capText(result.Output),
					ExitCode:   result.ExitCode,
					DurationMS: result.DurationMS,
					Truncated:  result.Truncated,
				}
				post := a.hooks.dispatch(ctx, payload, a.hookDispatchOpts(name))
				appendHookToolContext(&result, hookEventPreToolUse, preContext)
				appendHookToolContext(&result, hookEventPostToolUse, post.additionalContext)
				return result, callErr
			}
			appendHookToolContext(&result, hookEventPreToolUse, preContext)
		case tool.Failed:
			if a.hooks.has(hookEventPostToolUseFailure) {
				payload := a.hookBasePayload(hookEventPostToolUseFailure)
				payload.ToolName = name
				payload.ToolInput = input
				payload.ToolUseID = callID
				payload.Meldra.Effect = string(toolEffect(name))
				payload.Error = cmp.Or(result.Error, errorText(callErr))
				payload.IsInterrupt = new(false)
				payload.ToolResponse = &hookToolResponse{
					Status:     status,
					Output:     capText(result.Output),
					Error:      payload.Error,
					ExitCode:   result.ExitCode,
					DurationMS: result.DurationMS,
					Truncated:  result.Truncated,
				}
				post := a.hooks.dispatch(ctx, payload, a.hookDispatchOpts(name))
				appendHookToolContext(&result, hookEventPreToolUse, preContext)
				appendHookToolContext(&result, hookEventPostToolUseFailure, post.additionalContext)
				return result, callErr
			}
			appendHookToolContext(&result, hookEventPreToolUse, preContext)
		}
	}
	return result, callErr
}
