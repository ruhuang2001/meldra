package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	"meldra/internal/provider"
	"meldra/internal/tool"
)

func TestSteeringContextRetainsGoalCompletedToolAndOriginalReference(t *testing.T) {
	a, _ := recordedAgent(t, nil, nil)
	w := a.execution.workspace
	w.projectContext = NewProjectContext(w)
	a.projectContext = w.projectContext
	writeContextFixture(t, w, "source.txt", "original attached bytes")
	if err := a.execution.begin(t.Context(), "original goal inspect @source.txt"); err != nil {
		t.Fatal(err)
	}
	defer a.execution.finish(t.Context(), nil)
	_, refs, err := a.prepareReferences(t.Context(), "original goal inspect @source.txt")
	if err != nil {
		t.Fatal(err)
	}
	a.session.appendMessage("user", "original goal inspect @source.txt")
	a.session.Messages[0].References = refs
	if _, err := w.projectContext.Instructions(); err != nil {
		t.Fatal(err)
	}
	registry, err := tool.New(w.ToolDefinitions())
	if err != nil {
		t.Fatal(err)
	}
	result, err := a.execution.invoke(t.Context(), registry, "completed-edit", "edit_file", json.RawMessage(`{"path":"created.txt","old_str":"","new_str":"already completed"}`))
	if err != nil || result.Status != tool.Succeeded {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	writeContextFixture(t, w, "source.txt", "changed after attachment")
	_, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	if err := a.beginControl(t.Context(), cancel); err != nil {
		t.Fatal(err)
	}
	defer a.endControl()
	if err := a.SubmitControl("steer", "keep the completed edit and investigate next"); err != nil {
		t.Fatal(err)
	}
	correction, err := a.applySteering(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	input, err := a.steeringContext(t.Context(), correction)
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"Original goal: original goal", "edit_file: succeeded", "already completed", "original attached bytes", "keep the completed edit"} {
		if !strings.Contains(input, part) {
			t.Fatalf("steering context omitted %q: %s", part, input)
		}
	}
	if strings.Contains(input, "changed after attachment") {
		t.Fatal("steering reread historical attachment")
	}
}

func TestSteeringReferencesAppearOnceAndKeepAcceptedSnapshots(t *testing.T) {
	for _, durable := range []bool{false, true} {
		for _, custom := range []bool{false, true} {
			t.Run(fmt.Sprintf("durable=%t/custom=%t", durable, custom), func(t *testing.T) {
				w := contextWorkspace(t)
				a := NewAgent(nil, nil, nil)
				if durable {
					a, _ = recordedAgent(t, nil, nil)
					w = a.execution.workspace
					w.projectContext = NewProjectContext(w)
				}
				a.projectContext, a.customProvider = w.projectContext, custom
				a.output = io.Discard
				contents := map[string]string{
					"original.txt": "ORIGINAL_SNAPSHOT_" + strings.Repeat("o", 32<<10-18),
					"first.txt":    "FIRST_CORRECTION_" + strings.Repeat("f", 64<<10-17),
					"second.txt":   "SECOND_CORRECTION_" + strings.Repeat("s", 32<<10-18),
					"queued.txt":   "QUEUED_SNAPSHOT",
				}
				for path, content := range contents {
					writeContextFixture(t, w, path, content)
				}
				requests := 0
				a.backend = inferenceFunc(func(_ context.Context, request provider.Request, _ provider.Options, _ provider.Observer) (provider.Result, error) {
					requests++
					input := fmt.Sprintf("%+v", request.Input)
					switch requests {
					case 1:
						for _, text := range []string{"Use @first.txt", "Also inspect @second.txt"} {
							if err := a.SubmitControl("steer", text); err != nil {
								t.Fatal(err)
							}
						}
						if err := a.SubmitControl("queue", "Later inspect @queued.txt"); err != nil {
							t.Fatal(err)
						}
						for path := range contents {
							writeContextFixture(t, w, path, "CHANGED_AFTER_ACCEPTANCE")
						}
					case 2:
						for _, marker := range []string{"ORIGINAL_SNAPSHOT_", "FIRST_CORRECTION_", "SECOND_CORRECTION_"} {
							if count := strings.Count(input, marker); count != 1 {
								t.Errorf("corrected input contains %q %d times; want exactly once", marker, count)
							}
						}
						if strings.Contains(input, "QUEUED_SNAPSHOT") || strings.Contains(input, "CHANGED_AFTER_ACCEPTANCE") {
							t.Error("steering attached queued work or reread accepted files")
						}
					case 3:
						if strings.Count(input, "QUEUED_SNAPSHOT") != 1 || strings.Contains(input, "CHANGED_AFTER_ACCEPTANCE") {
							t.Error("queued request lost its original accepted attachment")
						}
					default:
						t.Fatalf("unexpected request %d", requests)
					}
					return provider.Result{Response: finishResponse()}, nil
				})
				provided := false
				a.getUserMessage = func() (string, bool) {
					if provided {
						return "", false
					}
					provided = true
					return "Start with @original.txt", true
				}
				if err := a.Run(t.Context()); err != nil {
					t.Fatal(err)
				}
				if requests != 3 {
					t.Fatalf("requests = %d; want 3", requests)
				}
			})
		}
	}
}
