package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"unicode/utf8"

	"meldra/internal/provider"

	"github.com/openai/openai-go/v3/responses"
)

func responseFromJSON(t *testing.T, contents string) *responses.Response {
	t.Helper()
	var response responses.Response
	if err := json.Unmarshal([]byte(contents), &response); err != nil {
		t.Fatal(err)
	}
	return &response
}

func userMessages(values ...string) func() (string, bool) {
	index := 0
	return func() (string, bool) {
		if index >= len(values) {
			return "", false
		}
		value := values[index]
		index++
		return value, true
	}
}

func toolInput(t *testing.T, value any) json.RawMessage {
	t.Helper()

	input, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return input
}

func TestExecuteToolCallsReturnsEveryResult(t *testing.T) {
	var output []provider.OutputItem
	if err := json.Unmarshal([]byte(`[
		{"type":"function_call","call_id":"call_1","name":"echo","arguments":"{\"value\":\"one\"}"},
		{"type":"function_call","call_id":"call_2","name":"missing","arguments":"{}"}
	]`), &output); err != nil {
		t.Fatal(err)
	}

	agent := Agent{tools: []ToolDefinition{{
		Name: "echo",
		Function: func(_ context.Context, input json.RawMessage) (string, error) {
			var params struct {
				Value string `json:"value"`
			}
			if err := json.Unmarshal(input, &params); err != nil {
				return "", err
			}
			return params.Value, nil
		},
	}}}
	results := agent.executeToolCalls(output)
	if len(results) != 2 {
		t.Fatalf("executeToolCalls returned %d results, want 2", len(results))
	}

	encoded, err := json.Marshal(results)
	if err != nil {
		t.Fatal(err)
	}
	var got []struct {
		CallID string `json:"call_id"`
		Output string `json:"output"`
	}
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if got[0].CallID != "call_1" || got[0].Output != "one" {
		t.Fatalf("first tool result is %#v", got[0])
	}
	if got[1].CallID != "call_2" || !strings.Contains(got[1].Output, "not found") {
		t.Fatalf("unknown tool result is %#v", got[1])
	}
}

func TestExecuteToolCallsStopsAfterCancellation(t *testing.T) {
	var output []provider.OutputItem
	if err := json.Unmarshal([]byte(`[
		{"type":"function_call","call_id":"call_1","name":"cancel","arguments":"{}"},
		{"type":"function_call","call_id":"call_2","name":"later","arguments":"{}"}
	]`), &output); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	laterCalls := 0
	agent := Agent{tools: []ToolDefinition{
		{Name: "cancel", Function: func(context.Context, json.RawMessage) (string, error) {
			cancel()
			return "cancelled", nil
		}},
		{Name: "later", Function: func(context.Context, json.RawMessage) (string, error) {
			laterCalls++
			return "unexpected", nil
		}},
	}}
	results := agent.executeToolCallsContext(ctx, output)
	if len(results) != 1 || laterCalls != 0 {
		t.Fatalf("tool results = %d, later calls = %d", len(results), laterCalls)
	}
}

func TestSummarizeToolResultCompactsStructuredOutput(t *testing.T) {
	tests := []struct {
		result string
		want   string
	}{
		{result: `["a","b","c"]`, want: "3 items returned"},
		{result: `{"path":"main.go"}`, want: "1 field returned"},
		{result: "first line\nsecond line", want: "first line"},
	}
	for _, test := range tests {
		if got := summarizeToolResult(test.result); got != test.want {
			t.Errorf("summarizeToolResult(%q) = %q, want %q", test.result, got, test.want)
		}
	}
}

func TestSummarizeToolResultTruncatesUTF8OnRuneBoundary(t *testing.T) {
	result := strings.Repeat("\u754c", 141)
	got := summarizeToolResult(result)
	want := strings.Repeat("\u754c", 137) + "..."
	if got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("summary contains invalid UTF-8: %q", got)
	}
}

func TestModelNameCanBeConfigured(t *testing.T) {
	agent := Agent{model: "gpt-test"}
	if got := agent.modelName(); got != "gpt-test" {
		t.Fatalf("agent model name returned %q, want gpt-test", got)
	}
}

func TestUsesCustomBaseURL(t *testing.T) {
	for _, baseURL := range []string{"", defaultBaseURL, defaultBaseURL + "/"} {
		if isCustomBaseURL(baseURL) {
			t.Errorf("isCustomBaseURL(%q) = true", baseURL)
		}
	}

	if !isCustomBaseURL("https://provider.example/v1") {
		t.Error("isCustomBaseURL() = false for compatible provider URL")
	}
}

func TestAgentRequestUsesInstanceModelInsteadOfEnvironment(t *testing.T) {
	t.Setenv("OPENAI_MODEL", "environment-model")
	var requestedModel string
	agent := Agent{
		model: "instance-model", backend: &provider.Client{CreateResponse: func(_ context.Context, params responses.ResponseNewParams) (*responses.Response, error) {
			requestedModel = string(params.Model)
			response := streamedCompletedResponse(t, "done")
			return &response, nil
		}},
	}
	if _, err := agent.runInference(context.Background(), provider.Input{}, ""); err != nil {
		t.Fatal(err)
	}
	if requestedModel != "instance-model" {
		t.Fatalf("requested model = %q", requestedModel)
	}
}

func TestAgentRunCompletesToolLoopWithInstructions(t *testing.T) {
	first := responseFromJSON(t, `{
		"id":"resp_1",
		"status":"completed",
		"output":[{"type":"function_call","call_id":"call_1","name":"echo","arguments":"{\"value\":\"one\"}"}]
	}`)
	second := responseFromJSON(t, `{
		"id":"resp_2",
		"status":"completed",
		"output":[{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"done","annotations":[]}]}]
	}`)
	responsesToReturn := []*responses.Response{first, second}
	var requests []responses.ResponseNewParams
	var output bytes.Buffer
	agent := Agent{
		getUserMessage: userMessages("echo a value"),
		output:         &output,
		tools: []ToolDefinition{{
			Name:       "echo",
			Parameters: objectSchema(map[string]any{"value": map[string]any{"type": "string"}}, []string{"value"}),
			Function: func(_ context.Context, input json.RawMessage) (string, error) {
				var value struct {
					Value string `json:"value"`
				}
				if err := json.Unmarshal(input, &value); err != nil {
					return "", err
				}
				return value.Value, nil
			},
		}}, backend: &provider.Client{CreateResponse: func(_ context.Context, params responses.ResponseNewParams) (*responses.Response, error) {
			requests = append(requests, params)
			response := responsesToReturn[0]
			responsesToReturn = responsesToReturn[1:]
			return response, nil
		}},
	}

	if err := agent.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 {
		t.Fatalf("request count = %d, want 2", len(requests))
	}
	for index, request := range requests {
		if !request.Instructions.Valid() || (!strings.HasPrefix(request.Instructions.Value, agentInstructions) || !strings.Contains(request.Instructions.Value, "Runtime mode: build. Permission profile: interactive.") || request.Instructions.Value != requests[0].Instructions.Value) {
			t.Fatalf("request %d did not include stable agent instructions", index+1)
		}
	}
	if requests[0].PreviousResponseID.Valid() {
		t.Fatal("first request unexpectedly included previous_response_id")
	}
	if !requests[1].PreviousResponseID.Valid() || requests[1].PreviousResponseID.Value != "resp_1" {
		t.Fatalf("second previous_response_id = %#v", requests[1].PreviousResponseID)
	}
	encodedInput, err := json.Marshal(requests[1].Input)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"function_call_output", "call_1", "one"} {
		if !strings.Contains(string(encodedInput), want) {
			t.Errorf("tool follow-up input missing %q: %s", want, encodedInput)
		}
	}
	if !strings.Contains(output.String(), "tool\u001b[0m: echo") || !strings.Contains(output.String(), "done") {
		t.Fatalf("agent output = %q", output.String())
	}
}

func TestAgentContinuesBeyondFormerTurnLimits(t *testing.T) {
	responsesToReturn := make([]*responses.Response, 0, 22)
	for step := 0; step < 21; step++ {
		responsesToReturn = append(responsesToReturn, responseFromJSON(t, fmt.Sprintf(`{
			"id":"resp_%d",
			"status":"completed",
			"output":[
				{"type":"function_call","call_id":"call_%d_1","name":"echo","arguments":"{}"},
				{"type":"function_call","call_id":"call_%d_2","name":"echo","arguments":"{}"},
				{"type":"function_call","call_id":"call_%d_3","name":"echo","arguments":"{}"}
			]
		}`, step, step, step, step)))
	}
	responsesToReturn = append(responsesToReturn, responseFromJSON(t, `{
		"id":"resp_done",
		"status":"completed",
		"output":[{"type":"message","id":"msg_done","status":"completed","role":"assistant","content":[{"type":"output_text","text":"done","annotations":[]}]}]
	}`))

	requests := 0
	executed := 0
	agent := Agent{
		getUserMessage: userMessages("finish the task"),
		output:         io.Discard,
		tools: []ToolDefinition{{Name: "echo", Function: func(context.Context, json.RawMessage) (string, error) {
			executed++
			return "ok", nil
		}}}, backend: &provider.Client{CreateResponse: func(_ context.Context, _ responses.ResponseNewParams) (*responses.Response, error) {
			response := responsesToReturn[requests]
			requests++
			return response, nil
		}},
	}

	if err := agent.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if requests != 22 || executed != 63 {
		t.Fatalf("requests = %d, executed tools = %d; want 22 and 63", requests, executed)
	}
}

func TestAgentCancellationSavesResumableSession(t *testing.T) {
	store := NewSessionStore(mustConfigPaths(t))
	session, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var output bytes.Buffer
	agent := Agent{
		getUserMessage: userMessages("long task"),
		output:         &output,
		session:        session,
		store:          store, backend: &provider.Client{CreateResponse: func(_ context.Context, _ responses.ResponseNewParams) (*responses.Response, error) {
			cancel()
			return nil, context.Canceled
		}},
	}

	if err := agent.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Session "+session.ID+" was saved") {
		t.Fatalf("cancellation output = %q", output.String())
	}
	loaded, err := store.Load(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.PreviousResponseID != "" || len(loaded.Messages) != 2 || loaded.Messages[0].Role != "user" || loaded.Messages[1].Role != "assistant" {
		t.Fatalf("saved interrupted session = %#v", loaded)
	}
}
