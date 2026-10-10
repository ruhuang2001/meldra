package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"meldra/internal/provider"
	"meldra/internal/tool"
)

func TestPolicyPlanRejectsBareAgentWritesAndAllowsMetadata(t *testing.T) {
	calls := 0
	definitions := []ToolDefinition{
		{Name: "edit_file", Function: func(context.Context, json.RawMessage) (string, error) {
			t.Fatal("Plan invoked write handler")
			return "", nil
		}},
		{Name: "update_plan", Function: func(context.Context, json.RawMessage) (string, error) { calls++; return "saved metadata", nil }},
		{Name: "external_readonly_hint", Function: func(context.Context, json.RawMessage) (string, error) {
			t.Fatal("Plan trusted unclassified external effect")
			return "", nil
		}},
	}
	a := NewAgent(nil, nil, definitions)
	a.output = io.Discard
	if err := a.SetMode(ModePlan); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"edit_file", "external_readonly_hint"} {
		if _, err := a.executeTool(t.Context(), name, json.RawMessage(`{}`)); err == nil {
			t.Fatalf("Plan allowed %s", name)
		}
	}
	if _, err := a.executeTool(t.Context(), "update_plan", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("metadata calls=%d", calls)
	}
	for _, raw := range []string{`{"preset":"test"}`, `{"preset":"format"}`, `{"preset":"build"}`} {
		if _, err := a.policy.invocation(t.Context(), "verify", json.RawMessage(raw)); err == nil {
			t.Fatalf("Plan allowed executable verification %s", raw)
		}
	}
	if _, err := a.policy.invocation(t.Context(), "verify", json.RawMessage(`{"preset":"diff"}`)); err != nil {
		t.Fatal(err)
	}
}

func TestPolicyInvalidatedApprovalCannotWriteOrApproveReplacement(t *testing.T) {
	w := contextWorkspace(t)
	writeContextFixture(t, w, "file.txt", "before")
	if _, err := w.projectContext.Instructions(); err != nil {
		t.Fatal(err)
	}
	w.policy, _ = newRuntimePolicy(ModeBuild, PermissionInteractive)
	w.autoApprove = false
	entered, release := make(chan struct{}), make(chan struct{})
	w.SetApprovalFunc(func(context.Context, ApprovalRequest) bool { close(entered); <-release; return true })
	registry, err := tool.New(w.ToolDefinitions())
	if err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		result tool.Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := registry.Invoke(t.Context(), "edit_file", json.RawMessage(`{"path":"file.txt","old_str":"before","new_str":"after"}`))
		done <- outcome{result, err}
	}()
	<-entered
	w.policy.invalidate()
	close(release)
	got := <-done
	if !errors.Is(got.err, ErrOperationSuperseded) {
		t.Fatalf("result=%+v err=%v", got.result, got.err)
	}
	data, err := os.ReadFile(filepath.Join(w.root, "file.txt"))
	if err != nil || string(data) != "before" {
		t.Fatalf("stale approval wrote file=%s err=%v", data, err)
	}
	w.SetApprovalFunc(func(context.Context, ApprovalRequest) bool { return false })
	result, err := registry.Invoke(t.Context(), "edit_file", json.RawMessage(`{"path":"file.txt","old_str":"before","new_str":"replacement"}`))
	if err != nil || result.Status != tool.Declined {
		t.Fatalf("replacement result=%+v err=%v", result, err)
	}
}

func TestPolicyStopBeforeAdmissionPreventsMutation(t *testing.T) {
	w := contextWorkspace(t)
	writeContextFixture(t, w, "file.txt", "before")
	if _, err := w.projectContext.Instructions(); err != nil {
		t.Fatal(err)
	}
	w.policy, _ = newRuntimePolicy(ModeBuild, PermissionInteractive)
	ctx, err := w.policy.invocation(t.Context(), "edit_file", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	changes, err := w.prepare([]changeInput{{Path: "file.txt", OldStr: "before", NewStr: "after"}})
	if err != nil {
		t.Fatal(err)
	}
	w.SetContext(ctx)
	w.policy.invalidate()
	if err := w.writeChanges(changes, false); !errors.Is(err, ErrOperationSuperseded) {
		t.Fatalf("admission err=%v", err)
	}
	data, err := os.ReadFile(filepath.Join(w.root, "file.txt"))
	if err != nil || string(data) != "before" {
		t.Fatalf("stopped mutation=%s err=%v", data, err)
	}
}

func TestSelectedQueueCrashBeforeSnapshotRestoresIdentityAndReferences(t *testing.T) {
	a, paths := recordedAgent(t, nil, nil)
	w := a.execution.workspace
	w.projectContext = NewProjectContext(w)
	a.projectContext = w.projectContext
	writeContextFixture(t, w, "source.txt", "accepted snapshot")
	if err := a.execution.begin(t.Context(), "first request"); err != nil {
		t.Fatal(err)
	}
	_, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	if err := a.beginControl(t.Context(), cancel); err != nil {
		t.Fatal(err)
	}
	if err := a.SubmitControl("queue", "review @source.txt"); err != nil {
		t.Fatal(err)
	}
	selected, err := a.pendingControl()
	if err != nil || selected == nil {
		t.Fatalf("selected=%v err=%v", selected, err)
	}
	a.endControl()
	if err := a.execution.finish(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	a.execution.requestControlID = selected.ID
	a.execution.requestControlRefs = selected.References
	if err := a.execution.begin(t.Context(), selected.Text); err != nil {
		t.Fatal(err)
	}
	// No message or applied event is saved: this is the crash-start window.
	if err := a.execution.close(); err != nil {
		t.Fatal(err)
	}
	a.execution.resume = true
	if err := a.execution.begin(t.Context(), "explicit resume"); err != nil {
		t.Fatal(err)
	}
	if err := a.execution.finish(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	var matched int
	for _, message := range a.session.Messages {
		if message.ControlID == selected.ID {
			matched++
			if len(message.References) != 1 || message.References[0].Digest != digest([]byte("accepted snapshot")) {
				t.Fatalf("restored refs=%+v", message.References)
			}
		}
	}
	if matched != 1 {
		t.Fatalf("selected identity restored %d times", matched)
	}
	loaded, err := newTaskSessionStore(paths).Load(a.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	recovered := NewAgent(inferenceFunc(func(context.Context, provider.Request, provider.Options, provider.Observer) (provider.Result, error) {
		t.Fatal("recovery inspection executed model")
		return provider.Result{}, nil
	}), nil, nil)
	recovered.session = loaded
	recovered.execution = &taskExecution{paths: paths, session: loaded, workspace: w}
	if err := recovered.loadPendingControls(); err != nil {
		t.Fatal(err)
	}
	if len(recovered.control.recovered) != 0 {
		t.Fatalf("already restored queue replayed: %+v", recovered.control.recovered)
	}
}

func TestQueuedSlashTextIsNotReinterpretedAsModeCommand(t *testing.T) {
	var a *Agent
	calls := 0
	a, _ = recordedAgent(t, inferenceFunc(func(context.Context, provider.Request, provider.Options, provider.Observer) (provider.Result, error) {
		calls++
		if calls == 1 {
			if err := a.SubmitControl("queue", "/plan"); err != nil {
				t.Fatal(err)
			}
		}
		return provider.Result{Response: finishResponse()}, nil
	}), nil)
	provided := false
	a.getUserMessage = func() (string, bool) {
		if provided {
			return "", false
		}
		provided = true
		return "initial", true
	}
	if err := a.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	mode, _, _ := a.policy.snapshot()
	if calls != 2 || mode != ModeBuild {
		t.Fatalf("queued text changed runtime mode: calls=%d mode=%s", calls, mode)
	}
	last := a.session.Messages[len(a.session.Messages)-2]
	if last.Content != "/plan" || last.ControlID == "" {
		t.Fatalf("queued text was not preserved: %+v", last)
	}
}

func TestModeControlsPrecedeQueuedWork(t *testing.T) {
	a := NewAgent(nil, nil, nil)
	a.output = io.Discard
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	if err := a.beginControl(ctx, cancel); err != nil {
		t.Fatal(err)
	}
	defer a.endControl()
	if err := a.SubmitControl("queue", "modify files"); err != nil {
		t.Fatal(err)
	}
	if err := a.SubmitControl("mode", "plan"); err != nil {
		t.Fatal(err)
	}
	first, err := a.pendingControl()
	if err != nil {
		t.Fatal(err)
	}
	if first == nil || first.Kind != "mode" {
		t.Fatalf("queued work ran before mode control: %+v", first)
	}
	if err := a.applyMode(ModePlan); err != nil {
		t.Fatal(err)
	}
	second, err := a.pendingControl()
	if err != nil {
		t.Fatal(err)
	}
	if second == nil || !strings.Contains(second.Text, "modify") {
		t.Fatalf("lost queued work: %+v", second)
	}
}

type signalPromptWriter struct {
	once   sync.Once
	prompt chan struct{}
}

func (w *signalPromptWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.prompt) })
	return len(p), nil
}

func TestLineControlStopsApprovalAndPreservesLiteralMCPForm(t *testing.T) {
	for _, form := range []bool{false, true} {
		t.Run(fmt.Sprint(form), func(t *testing.T) {
			a := NewAgent(nil, nil, nil)
			a.output = io.Discard
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			if err := a.beginControl(ctx, cancel); err != nil {
				t.Fatal(err)
			}
			defer a.endControl()
			reader, writer := io.Pipe()
			defer reader.Close()
			defer writer.Close()
			output := &signalPromptWriter{prompt: make(chan struct{})}
			controller := newLineController(t.Context(), &mcpCLIInput{Reader: bufio.NewReader(reader)}, a, output)
			defer controller.close()
			result := make(chan string, 1)
			go func() {
				if form {
					text, _ := controller.humanInput(ctx, "form")
					result <- text
				} else {
					if controller.approve(ctx, ApprovalRequest{Title: "approval"}) {
						result <- "approved"
					} else {
						result <- "declined"
					}
				}
			}()
			<-output.prompt
			if _, err := io.WriteString(writer, "/stop\n"); err != nil {
				t.Fatal(err)
			}
			got := <-result
			if form {
				if got != "/stop" || ctx.Err() != nil {
					t.Fatalf("form answer became control: got=%s err=%v", got, ctx.Err())
				}
			} else {
				if got != "declined" || !errors.Is(context.Cause(ctx), errTurnStopped) {
					t.Fatalf("stop did not cancel approval: got=%s cause=%v", got, context.Cause(ctx))
				}
			}
		})
	}
}
