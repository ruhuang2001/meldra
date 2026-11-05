package app

import (
	"context"
	"strings"
	"testing"

	"meldra/internal/provider"

	"github.com/openai/openai-go/v3/responses"
)

func TestPlanInstructionsBoundExploration(t *testing.T) {
	planAgent := &Agent{}
	planAgent.policy, _ = newRuntimePolicy(ModePlan, PermissionInteractive)
	buildAgent := &Agent{}
	buildAgent.policy, _ = newRuntimePolicy(ModeBuild, PermissionInteractive)

	for _, test := range []struct {
		name string
		a    *Agent
		want []string
		deny []string
	}{
		{
			name: "plan",
			a:    planAgent,
			want: []string{
				"Plan mode:",
				"Only use read-only tools when the plan actually needs project information.",
				"can be answered without project context",
				"end the turn",
				"stop exploring and wait",
			},
			deny: []string{"Build mode:"},
		},
		{
			name: "build",
			a:    buildAgent,
			want: []string{"Build mode:", "changing mode does not preapprove commands"},
			deny: []string{"Plan mode:", "stop exploring and wait"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			instructions := captureInstructions(t, test.a)
			for _, want := range test.want {
				if !strings.Contains(instructions, want) {
					t.Errorf("instructions missing %q:\n%s", want, instructions)
				}
			}
			for _, deny := range test.deny {
				if strings.Contains(instructions, deny) {
					t.Errorf("instructions unexpectedly contain %q:\n%s", deny, instructions)
				}
			}
		})
	}
}

func captureInstructions(t *testing.T, a *Agent) string {
	t.Helper()
	var instructions string
	a.backend = &provider.Client{CreateResponse: func(_ context.Context, params responses.ResponseNewParams) (*responses.Response, error) {
		instructions = params.Instructions.Value
		return responseFromJSON(t, `{
			"id":"resp_1",
			"status":"completed",
			"output":[{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"done","annotations":[]}]}]
		}`), nil
	}}
	if _, err := a.runInference(context.Background(), provider.UserInput("hello"), ""); err != nil {
		t.Fatal(err)
	}
	if instructions == "" {
		t.Fatal("no instructions captured")
	}
	return instructions
}
