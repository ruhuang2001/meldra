package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

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
	var output []responses.ResponseOutputItemUnion
	if err := json.Unmarshal([]byte(`[
		{"type":"function_call","call_id":"call_1","name":"echo","arguments":"{\"value\":\"one\"}"},
		{"type":"function_call","call_id":"call_2","name":"missing","arguments":"{}"}
	]`), &output); err != nil {
		t.Fatal(err)
	}

	agent := Agent{tools: []ToolDefinition{{
		Name: "echo",
		Function: func(input json.RawMessage) (string, error) {
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
	var output []responses.ResponseOutputItemUnion
	if err := json.Unmarshal([]byte(`[
		{"type":"function_call","call_id":"call_1","name":"cancel","arguments":"{}"},
		{"type":"function_call","call_id":"call_2","name":"later","arguments":"{}"}
	]`), &output); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	laterCalls := 0
	agent := Agent{tools: []ToolDefinition{
		{Name: "cancel", Function: func(json.RawMessage) (string, error) {
			cancel()
			return "cancelled", nil
		}},
		{Name: "later", Function: func(json.RawMessage) (string, error) {
			laterCalls++
			return "unexpected", nil
		}},
	}}
	results := agent.executeToolCallsContext(ctx, output)
	if len(results) != 1 || laterCalls != 0 {
		t.Fatalf("tool results = %d, later calls = %d", len(results), laterCalls)
	}
}

func TestToolFollowUpInputIncludesCallsBeforeTheirOutputs(t *testing.T) {
	var output []responses.ResponseOutputItemUnion
	if err := json.Unmarshal([]byte(`[
		{"type":"function_call","call_id":"call_1","name":"echo","arguments":"{\"value\":\"one\"}"}
	]`), &output); err != nil {
		t.Fatal(err)
	}

	input, err := toolFollowUpInput(output, responses.ResponseInputParam{
		responses.ResponseInputItemParamOfFunctionCallOutput("call_1", "one"),
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}

	var got []struct {
		Type   string `json:"type"`
		CallID string `json:"call_id"`
		Name   string `json:"name"`
	}
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("tool follow-up has %d items, want 2", len(got))
	}
	if got[0].Type != "function_call" || got[0].CallID != "call_1" || got[0].Name != "echo" {
		t.Fatalf("first item is %#v, want the original function call", got[0])
	}
	if got[1].Type != "function_call_output" || got[1].CallID != "call_1" {
		t.Fatalf("second item is %#v, want the matching function output", got[1])
	}
}

func TestToolFollowUpInputPreservesReasoningMessagesAndCalls(t *testing.T) {
	var output []responses.ResponseOutputItemUnion
	if err := json.Unmarshal([]byte(`[
		{"type":"reasoning","id":"reason_1","summary":[{"type":"summary_text","text":"checked"}],"encrypted_content":"encrypted","status":"completed"},
		{"type":"message","id":"msg_1","status":"completed","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"working","annotations":[]}]},
		{"type":"function_call","id":"fc_1","call_id":"call_1","name":"echo","arguments":"{}","status":"completed"}
	]`), &output); err != nil {
		t.Fatal(err)
	}
	input, err := toolFollowUpInput(output, responses.ResponseInputParam{
		responses.ResponseInputItemParamOfFunctionCallOutput("call_1", "ok"),
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	var got []map[string]any
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("replayed %d items, want 4: %s", len(got), encoded)
	}
	wantTypes := []string{"reasoning", "message", "function_call", "function_call_output"}
	for index, want := range wantTypes {
		if got[index]["type"] != want {
			t.Errorf("item %d type = %v, want %s", index, got[index]["type"], want)
		}
	}
	if got[0]["encrypted_content"] != "encrypted" || got[1]["phase"] != "commentary" || got[2]["id"] != "fc_1" {
		t.Fatalf("replayed output lost fields: %s", encoded)
	}
	if _, err := toolFollowUpInput([]responses.ResponseOutputItemUnion{{Type: "unknown"}}, nil); err == nil {
		t.Fatal("unsupported output type was silently discarded")
	}
}

func TestValidateResponse(t *testing.T) {
	if err := validateResponse(&responses.Response{Status: responses.ResponseStatusCompleted}); err != nil {
		t.Fatalf("completed response was rejected: %v", err)
	}

	incomplete := &responses.Response{
		Status:            responses.ResponseStatusIncomplete,
		IncompleteDetails: responses.ResponseIncompleteDetails{Reason: "max_output_tokens"},
	}
	if err := validateResponse(incomplete); err == nil || !strings.Contains(err.Error(), "max_output_tokens") {
		t.Fatalf("incomplete response returned unexpected error: %v", err)
	}

	failed := &responses.Response{
		Status: responses.ResponseStatusFailed,
		Error:  responses.ResponseError{Message: "model failed"},
	}
	if err := validateResponse(failed); err == nil || !strings.Contains(err.Error(), "model failed") {
		t.Fatalf("failed response returned unexpected error: %v", err)
	}
}

func TestModelNameCanBeConfigured(t *testing.T) {
	t.Setenv("OPENAI_MODEL", "gpt-test")
	if got := modelName(); got != "gpt-test" {
		t.Fatalf("modelName returned %q, want gpt-test", got)
	}
}

func TestUsesCustomBaseURL(t *testing.T) {
	for _, baseURL := range []string{"", defaultBaseURL, defaultBaseURL + "/"} {
		t.Setenv("OPENAI_BASE_URL", baseURL)
		if usesCustomBaseURL() {
			t.Errorf("usesCustomBaseURL() = true for official URL %q", baseURL)
		}
	}

	t.Setenv("OPENAI_BASE_URL", "https://provider.example/v1")
	if !usesCustomBaseURL() {
		t.Error("usesCustomBaseURL() = false for compatible provider URL")
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
			Function: func(input json.RawMessage) (string, error) {
				var value struct {
					Value string `json:"value"`
				}
				if err := json.Unmarshal(input, &value); err != nil {
					return "", err
				}
				return value.Value, nil
			},
		}},
		createResponse: func(_ context.Context, params responses.ResponseNewParams) (*responses.Response, error) {
			requests = append(requests, params)
			response := responsesToReturn[0]
			responsesToReturn = responsesToReturn[1:]
			return response, nil
		},
	}

	if err := agent.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 {
		t.Fatalf("request count = %d, want 2", len(requests))
	}
	for index, request := range requests {
		if !request.Instructions.Valid() || request.Instructions.Value != agentInstructions {
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

func TestAgentToolCallLimitPausesAndRebuildsContext(t *testing.T) {
	store := NewSessionStore(mustConfigPaths(t))
	session, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first := responseFromJSON(t, `{
		"id":"resp_limit",
		"status":"completed",
		"output":[
			{"type":"function_call","call_id":"call_1","name":"echo","arguments":"{}"},
			{"type":"function_call","call_id":"call_2","name":"echo","arguments":"{}"}
		]
	}`)
	second := responseFromJSON(t, `{
		"id":"resp_done",
		"status":"completed",
		"output":[{"type":"message","id":"msg_done","status":"completed","role":"assistant","content":[{"type":"output_text","text":"continued","annotations":[]}]}]
	}`)
	responsesToReturn := []*responses.Response{first, second}
	var requests []responses.ResponseNewParams
	executed := 0
	agent := Agent{
		getUserMessage: userMessages("start", "continue"),
		output:         io.Discard,
		session:        session,
		store:          store,
		maxToolCalls:   1,
		tools: []ToolDefinition{{Name: "echo", Function: func(json.RawMessage) (string, error) {
			executed++
			return "ok", nil
		}}},
		createResponse: func(_ context.Context, params responses.ResponseNewParams) (*responses.Response, error) {
			requests = append(requests, params)
			response := responsesToReturn[0]
			responsesToReturn = responsesToReturn[1:]
			return response, nil
		},
	}

	if err := agent.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if executed != 0 {
		t.Fatalf("overflowing tool batch executed %d calls", executed)
	}
	if len(requests) != 2 || requests[1].PreviousResponseID.Valid() {
		t.Fatalf("requests after pause = %#v", requests)
	}
	if !strings.Contains(requests[1].Input.OfString.Value, "reached the limit of 1 tool calls") {
		t.Fatalf("rebuilt context = %q", requests[1].Input.OfString.Value)
	}
	loaded, err := store.Load(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.PreviousResponseID != "resp_done" {
		t.Fatalf("saved previous response = %q", loaded.PreviousResponseID)
	}
}

func TestAgentInferenceLimitPausesAfterExecutedTools(t *testing.T) {
	first := responseFromJSON(t, `{
		"id":"resp_step",
		"status":"completed",
		"output":[{"type":"function_call","call_id":"call_1","name":"echo","arguments":"{}"}]
	}`)
	second := responseFromJSON(t, `{
		"id":"resp_done",
		"status":"completed",
		"output":[{"type":"message","id":"msg_done","status":"completed","role":"assistant","content":[{"type":"output_text","text":"continued","annotations":[]}]}]
	}`)
	responsesToReturn := []*responses.Response{first, second}
	executed := 0
	var output bytes.Buffer
	agent := Agent{
		getUserMessage:    userMessages("start", "continue"),
		output:            &output,
		maxInferenceSteps: 1,
		tools: []ToolDefinition{{Name: "echo", Function: func(json.RawMessage) (string, error) {
			executed++
			return "ok", nil
		}}},
		createResponse: func(_ context.Context, _ responses.ResponseNewParams) (*responses.Response, error) {
			response := responsesToReturn[0]
			responsesToReturn = responsesToReturn[1:]
			return response, nil
		},
	}

	if err := agent.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if executed != 1 || !strings.Contains(output.String(), "limit of 1 model steps") {
		t.Fatalf("executed = %d, output = %q", executed, output.String())
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
		store:          store,
		createResponse: func(_ context.Context, _ responses.ResponseNewParams) (*responses.Response, error) {
			cancel()
			return nil, context.Canceled
		},
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
