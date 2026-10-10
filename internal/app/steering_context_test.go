package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

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
