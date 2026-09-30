package provider

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3/responses"
)

func TestToolFollowUpInputPreservesReasoningMessagesAndCalls(t *testing.T) {
	var output []responses.ResponseOutputItemUnion
	if err := json.Unmarshal([]byte(`[
		{"type":"reasoning","id":"reason_1","summary":[{"type":"summary_text","text":"checked"}],"encrypted_content":"encrypted","status":"completed"},
		{"type":"message","id":"msg_1","status":"completed","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"working","annotations":[]}]},
		{"type":"function_call","id":"fc_1","call_id":"call_1","name":"echo","arguments":"{}","status":"completed"}
	]`), &output); err != nil {
		t.Fatal(err)
	}
	input, err := toolFollowUpInput(output, responses.ResponseInputParam{responses.ResponseInputItemParamOfFunctionCallOutput("call_1", "ok")})
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
func TestBoundCustomTurnInputCompactsOldestToolOutputs(t *testing.T) {
	input := responses.ResponseInputParam{responses.ResponseInputItemParamOfMessage("request", responses.EasyInputMessageRoleUser), responses.ResponseInputItemParamOfFunctionCallOutput("old", strings.Repeat("x", 2048)), responses.ResponseInputItemParamOfFunctionCallOutput("new", strings.Repeat("y", 2048))}
	full, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	limit := len(full) - 1024
	bounded, size, compacted, err := boundCustomTurnInput(input, limit)
	if err != nil {
		t.Fatal(err)
	}
	if !compacted || size > limit {
		t.Fatalf("bounded context = %d bytes, compacted %v, limit %d", size, compacted, limit)
	}
	encoded, err := json.Marshal(bounded)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), compactedToolOutput) || strings.Contains(string(encoded), strings.Repeat("x", 128)) {
		t.Fatalf("old tool output was not compacted: %s", encoded)
	}
	if !strings.Contains(string(encoded), strings.Repeat("y", 128)) {
		t.Fatalf("newer tool output was compacted before necessary: %s", encoded)
	}
}
func TestBoundCustomTurnInputRejectsIrreducibleContext(t *testing.T) {
	input := responses.ResponseInputParam{responses.ResponseInputItemParamOfMessage(strings.Repeat("x", 1024), responses.EasyInputMessageRoleUser)}
	if _, _, _, err := boundCustomTurnInput(input, 128); err == nil {
		t.Fatal("irreducible custom-provider context was accepted")
	}
}
func TestValidateResponse(t *testing.T) {
	if err := Validate(domainResponse(&responses.Response{Status: responses.ResponseStatusCompleted})); err != nil {
		t.Fatalf("completed response was rejected: %v", err)
	}
	incomplete := &responses.Response{Status: responses.ResponseStatusIncomplete, IncompleteDetails: responses.ResponseIncompleteDetails{Reason: "max_output_tokens"}}
	if err := Validate(domainResponse(incomplete)); err == nil || !strings.Contains(err.Error(), "max_output_tokens") {
		t.Fatalf("incomplete response returned unexpected error: %v", err)
	}
	failed := &responses.Response{Status: responses.ResponseStatusFailed, Error: responses.ResponseError{Message: "model failed"}}
	if err := Validate(domainResponse(failed)); err == nil || !strings.Contains(err.Error(), "model failed") {
		t.Fatalf("failed response returned unexpected error: %v", err)
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
