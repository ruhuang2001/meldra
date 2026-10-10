package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"meldra/internal/provider"
)

func TestModeConnectionFailureKeepsChatUsable(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(map[bool]string{false: "immediate", true: "pending"}[pending], func(t *testing.T) {
			var a *Agent
			requests := 0
			a, _ = recordedAgent(t, inferenceFunc(func(_ context.Context, r provider.Request, _ provider.Options, _ provider.Observer) (provider.Result, error) {
				requests++
				if !strings.Contains(r.Instructions, "Runtime mode: plan") {
					t.Fatal("failed mode change granted Build")
				}
				if pending && requests == 1 {
					if err := a.SubmitControl("mode", "build"); err != nil {
						t.Fatal(err)
					}
				}
				return provider.Result{Response: finishResponse()}, nil
			}), nil)
			a.policy, _ = newRuntimePolicy(ModePlan, PermissionInteractive)
			a.connectExternal = func() error { return errors.New("invalid MCP config\x1b[31m") }
			var output bytes.Buffer
			a.output = &output
			messages := []string{"/build", "inspect after failed mode change"}
			if pending {
				messages = []string{"start plan", "inspect after pending change"}
			}
			a.getUserMessage = func() (string, bool) {
				if len(messages) == 0 {
					return "", false
				}
				s := messages[0]
				messages = messages[1:]
				return s, true
			}
			if err := a.Run(t.Context()); err != nil {
				t.Fatal(err)
			}
			want := 1
			if pending {
				want = 2
			}
			if requests != want {
				t.Fatalf("requests=%d", requests)
			}
			if !strings.Contains(output.String(), "Mode change failed:") || strings.Contains(output.String(), "\x1b[31m") {
				t.Fatalf("notice=%q", output.String())
			}
			if a.control.selected != nil {
				t.Fatal("failed mode selection retained")
			}
			if pending {
				events, err := openTaskDB(t, a.execution.paths).Events(t.Context(), a.session.ID, 0, 1000)
				if err != nil {
					t.Fatal(err)
				}
				received, applied := "", ""
				for _, event := range events {
					var r ControlRequest
					_ = json.Unmarshal(event.Data, &r)
					if event.Kind == "control.received" {
						received = r.ID
					}
					if event.Kind == "control.applied" {
						applied = r.ID
					}
				}
				if received == "" || applied != received {
					t.Fatalf("failed change not acknowledged %q %q", received, applied)
				}
			}
		})
	}
}

func TestSelectedControlClearedBeforeBeginAndReferenceErrors(t *testing.T) {
	for _, failure := range []string{"pre_cancel", "begin", "references"} {
		t.Run(failure, func(t *testing.T) {
			var last provider.Request
			a, _ := recordedAgent(t, inferenceFunc(func(_ context.Context, r provider.Request, _ provider.Options, _ provider.Observer) (provider.Result, error) {
				last = r
				return provider.Result{Response: finishResponse()}, nil
			}), nil)
			c := a.initControl()
			c.selected = &ControlRequest{ID: "stale-control", Kind: "queue", Text: "stale request"}
			ctx := t.Context()
			var cancel context.CancelFunc
			if failure == "pre_cancel" {
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if failure == "references" {
				c.selected.References = []ReferenceSnapshot{{FileReference: FileReference{Path: "bad"}}}
			}
			if failure == "begin" {
				lease, err := openTaskDB(t, a.execution.paths).Acquire(ctx, a.session.ID, a.session.Workspace)
				if err != nil {
					t.Fatal(err)
				}
				defer lease.Close()
				if err := a.RunTurn(ctx, "stale request"); err == nil {
					t.Fatal("busy begin succeeded")
				}
				if c.selected != nil {
					t.Fatal("begin failure retained selection")
				}
				return
			}
			if err := a.RunTurn(ctx, "stale request"); err == nil {
				t.Fatal("invalid request succeeded")
			}
			if c.selected != nil {
				t.Fatal("early failure retained selection")
			}
			if err := a.RunTurn(t.Context(), "fresh request"); err != nil {
				t.Fatal(err)
			}
			encoded := fmt.Sprintf("%+v", last.Input)
			if !strings.Contains(encoded, "fresh request") {
				t.Fatalf("new input lost: %s", encoded)
			}
			if a.session.Messages[len(a.session.Messages)-2].ControlID != "" {
				t.Fatal("fresh message reused old identity")
			}
		})
	}
}

func TestSelectedCleanupDoesNotClearReplacement(t *testing.T) {
	a := NewAgent(nil, nil, nil)
	c := a.initControl()
	old := &ControlRequest{ID: "old"}
	replacement := &ControlRequest{ID: "replacement"}
	c.selected = replacement
	a.clearSelected(old)
	if c.selected != replacement {
		t.Fatal("new selection erased")
	}
}

func TestInactiveControlPreservesLineAndTUIDrafts(t *testing.T) {
	a := NewAgent(nil, nil, nil)
	if err := a.SubmitControl("steer", "correction"); !errors.Is(err, ErrNoActiveTurn) || strings.Contains(err.Error(), "already has") {
		t.Fatalf("inactive error=%v", err)
	}
	input := &mcpCLIInput{Reader: bufio.NewReader(strings.NewReader("/steer keep this\n/queue keep this too\n"))}
	l := newLineController(t.Context(), input, a, io.Discard)
	defer l.close()
	for _, want := range []string{"/steer keep this", "/queue keep this too"} {
		got, ok := l.next(t.Context())
		if !ok || got != want {
			t.Fatalf("lost late input: %q %v", got, ok)
		}
	}
	m := newTUIModel(newTUIController(nil), tuiInitialState{})
	m.Update(tuiControlResultMsg{kind: "steer", text: "keep this", err: ErrNoActiveTurn})
	if m.input.Value() != "keep this" {
		t.Fatal("failed control draft lost")
	}
	m.input.SetValue("newer draft")
	m.Update(tuiControlResultMsg{kind: "queue", text: "older draft", err: ErrNoActiveTurn})
	if m.input.Value() != "newer draft" || !strings.Contains(m.renderTimeline(), "older draft") {
		t.Fatal("new draft overwritten or failed input hidden")
	}
}

func TestTUILateControlFallsBackToOrdinaryInput(t *testing.T) {
	for _, c := range []struct{ kind, value, original string }{{"mode", "plan", "/plan"}, {"steer", "correct", "/steer correct"}, {"queue", "next", "/queue next"}, {"stop", "", "/stop"}} {
		t.Run(c.kind, func(t *testing.T) {
			controller := newTUIController(nil)
			controller.submitControl = func(string, string) error { return ErrNoActiveTurn }
			result := controller.submitUserControl(c.kind, c.value, c.original)
			if !result.normalMessage || result.err != nil {
				t.Fatalf("late control rejected: %+v", result)
			}
			got, ok := controller.nextMessage()
			if !ok || got != c.original {
				t.Fatalf("late input lost: %q", got)
			}
			model := newTUIModel(controller, tuiInitialState{})
			model.Update(result)
			if strings.Contains(model.renderTimeline(), "Queued") {
				t.Fatal("unpersisted input falsely acknowledged as durable control")
			}
		})
	}
}
