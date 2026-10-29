package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

var errApprovalRequired = errors.New("operation requires approval; resume the task with --approve-detail-hash")

type usageError struct{ error }

func cliExitCode(err error) int {
	if err == nil {
		return 0
	}
	if _, ok := errors.AsType[*usageError](err); ok {
		return 2
	}
	if onlyError(err, errApprovalRequired) {
		return 3
	}
	return 1
}

// The wire contract is intentionally independent of UIEvent and ledger schemas.
type streamEnvelope struct {
	SchemaVersion int       `json:"schema_version"`
	Sequence      uint64    `json:"seq"`
	Time          time.Time `json:"time"`
	Kind          string    `json:"kind"`
	TaskID        string    `json:"task_id,omitempty"`
	RunID         string    `json:"run_id,omitempty"`
	ToolCallID    string    `json:"tool_call_id,omitempty"`
	Data          any       `json:"data"`
}

type jsonStream struct {
	mu      sync.Mutex
	encoder *json.Encoder
	seq     uint64
	err     error
	cancel  context.CancelFunc
}

func (s *jsonStream) write(kind, taskID, runID, callID string, data any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return
	}
	s.seq++
	s.err = s.encoder.Encode(streamEnvelope{1, s.seq, time.Now().UTC(), kind, taskID, runID, callID, data})
	if s.err != nil {
		s.cancel()
	}
}

func (s *jsonStream) failure() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *jsonStream) Emit(event UIEvent) {
	var data any
	switch event.Kind {
	case UIEventAssistantDelta, UIEventAssistantMessage, UIEventNotice, UIEventError:
		data = struct {
			Text string `json:"text"`
		}{event.Text}
	case UIEventToolStarted:
		data = struct {
			ToolCallID string `json:"tool_call_id"`
			Name       string `json:"name"`
		}{event.ToolCallID, event.Name}
	case UIEventToolFinished:
		data = struct {
			ToolCallID string `json:"tool_call_id"`
			Name       string `json:"name"`
			Status     string `json:"status"`
			DurationMS int64  `json:"duration_ms"`
			Truncated  bool   `json:"truncated"`
			Error      string `json:"error,omitempty"`
		}{event.ToolCallID, event.Name, event.Status, event.DurationMS, event.Truncated, event.Error}
	case UIEventMetrics:
		if event.Metrics == nil {
			return
		}
		data = struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
			ContextBytes int   `json:"context_bytes"`
		}{event.Metrics.InputTokens, event.Metrics.OutputTokens, event.Metrics.ContextBytes}
	case UIEventTurnStart:
		data = struct {
			TurnID string `json:"turn_id"`
			Index  int64  `json:"index"`
		}{event.RunID, event.TurnIndex}
	case UIEventTurnEnd:
		data = struct {
			Status string `json:"status"`
		}{event.Status}
	default:
		return
	}
	s.write(string(event.Kind), event.TaskID, event.RunID, event.ToolCallID, data)
}

func runJSONChat(ctx context.Context, stdout io.Writer, options ChatOptions) (err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	bounded, cleanup, err := newSynchronizedWriter(context.WithoutCancel(ctx), stdout)
	if err != nil {
		return err
	}
	defer cleanup()
	stream := &jsonStream{encoder: json.NewEncoder(bounded), cancel: cancel}
	var runtime *chatRuntime
	defer func() {
		status, reason := "succeeded", "completed"
		if err != nil {
			status, reason = "failed", "error"
			if cliExitCode(err) == 3 {
				status, reason = "waiting_approval", "approval_required"
			} else if onlyError(err, context.Canceled) {
				status, reason = "cancelled", "cancelled"
			} else {
				stream.Emit(UIEvent{Kind: UIEventError, Text: err.Error()})
			}
		}
		var taskID, runID string
		if runtime != nil {
			taskID, runID = runtime.session.ID, runtime.agent.execution.run.ID
		}
		stream.write("session_end", taskID, runID, "", struct {
			Status     string `json:"status"`
			ExitReason string `json:"exit_reason"`
		}{status, reason})
		err = errors.Join(err, stream.failure(), bounded.Close())
	}()
	paths, err := ResolveConfigPaths()
	if err != nil {
		return err
	}
	settings, err := LoadSettings(paths)
	if err != nil {
		return err
	}
	settings, err = effectiveSettings(settings)
	if err != nil {
		return err
	}
	if settings.APIKey == "" {
		return fmt.Errorf("OPENAI_API_KEY is not configured")
	}
	runtime, err = newChatRuntime(ctx, paths, settings, options, func(root string, auto bool) (*Workspace, error) {
		w, err := NewWorkspace(root, bufio.NewReader(strings.NewReader("")), os.Stderr, auto)
		if err == nil {
			w.mcpInput = func(context.Context, string) (string, bool) { return "", false }
		}
		return w, err
	}, nil, os.Stderr)
	if err != nil {
		return err
	}
	defer runtime.deleteEmptyNewSession()
	defer runtime.mcp.close()
	runtime.agent.events = stream
	runtime.agent.completeMessages = true
	runtime.agent.turnEvents = true
	runtime.agent.execution.noninteractive = true
	runtime.agent.execution.stream = stream
	runtime.workspace.SetApprovalFunc(func(context.Context, ApprovalRequest) bool { return false })
	stream.write("session_start", runtime.session.ID, "", "", struct {
		SessionID   string            `json:"session_id"`
		Workspace   string            `json:"workspace"`
		Mode        ExecutionMode     `json:"mode"`
		Permissions PermissionProfile `json:"permissions"`
	}{runtime.session.ID, runtime.workspace.root, runtime.session.Mode, runtime.session.Permissions})
	for _, warning := range runtime.mcp.warnings {
		runtime.agent.emitNotice(warning)
	}
	if runtime.agent.skills != nil {
		for _, warning := range runtime.agent.skills.Warnings {
			runtime.agent.emitNotice(skillDisplayLine(warning))
		}
	}
	if err := stream.failure(); err != nil {
		return err
	}
	return runtime.agent.RunTurn(ctx, options.Prompt)
}
