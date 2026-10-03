package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/param"
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

func TestBoundCustomTurnInputExactBudget(t *testing.T) {
	escaped := strings.Repeat("\"\\\n\t<>&\u2028\u2029世界\xff", 32)
	input := responses.ResponseInputParam{
		responses.ResponseInputItemParamOfMessage("keep <request>", responses.EasyInputMessageRoleUser),
		responses.ResponseInputItemParamOfFunctionCallOutput("old<>&", escaped),
		responses.ResponseInputItemParamOfMessage("keep intervening message", responses.EasyInputMessageRoleAssistant),
		responses.ResponseInputItemParamOfFunctionCallOutput("new\"", escaped),
	}
	original := mustMarshalConversation(t, input)
	want := slices.Clone(input)
	want[1] = responses.ResponseInputItemParamOfFunctionCallOutput("old<>&", compactedToolOutput)
	oneCompacted := mustMarshalConversation(t, want)
	want[3] = responses.ResponseInputItemParamOfFunctionCallOutput("new\"", compactedToolOutput)
	allCompacted := mustMarshalConversation(t, want)
	for _, test := range []struct {
		name      string
		limit     int
		want      []byte
		compacted bool
		wantError bool
	}{
		{"exact original", len(original), original, false, false},
		{"one byte below original", len(original) - 1, oneCompacted, true, false},
		{"exact first replacement", len(oneCompacted), oneCompacted, true, false},
		{"one byte below first replacement", len(oneCompacted) - 1, allCompacted, true, false},
		{"exact final replacement", len(allCompacted), allCompacted, true, false},
		{"irreducible", len(allCompacted) - 1, allCompacted, true, true},
		{"negative limit", -1, allCompacted, true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, size, compacted, err := boundCustomTurnInput(input, test.limit)
			if (err != nil) != test.wantError || compacted != test.compacted || size != len(test.want) {
				t.Fatalf("bound = size %d, compacted %t, error %v; want size %d, compacted %t, error %t", size, compacted, err, len(test.want), test.compacted, test.wantError)
			}
			if !test.wantError && !bytes.Equal(mustMarshalConversation(t, got), test.want) {
				t.Fatalf("compaction changed order, fields or the wrong output: %s", mustMarshalConversation(t, got))
			}
			if test.wantError && got != nil {
				t.Fatal("irreducible input returned a usable context")
			}
			if !bytes.Equal(mustMarshalConversation(t, input), original) {
				t.Fatal("compaction mutated the caller's input")
			}
		})
	}
}

func TestBoundCustomTurnInputEmptyAndNonshrinkingOutputs(t *testing.T) {
	for _, input := range []responses.ResponseInputParam{nil, {}} {
		encoded := mustMarshalConversation(t, input)
		for _, limit := range []int{len(encoded), len(encoded) - 1} {
			got, size, compacted, err := boundCustomTurnInput(input, limit)
			if size != len(encoded) || compacted || (err != nil) != (limit < len(encoded)) {
				t.Fatalf("empty input %s: size %d, compacted %t, error %v", encoded, size, compacted, err)
			}
			if err == nil && !bytes.Equal(mustMarshalConversation(t, got), encoded) {
				t.Fatal("nil and empty input were conflated")
			}
		}
	}
	input := responses.ResponseInputParam{
		responses.ResponseInputItemParamOfFunctionCallOutput("short", ""),
		responses.ResponseInputItemParamOfFunctionCallOutput("long", strings.Repeat("x", 1024)),
	}
	want := responses.ResponseInputParam{
		responses.ResponseInputItemParamOfFunctionCallOutput("short", compactedToolOutput),
		responses.ResponseInputItemParamOfFunctionCallOutput("long", compactedToolOutput),
	}
	encoded := mustMarshalConversation(t, want)
	got, size, compacted, err := boundCustomTurnInput(input, len(encoded))
	if err != nil || !compacted || size != len(encoded) || !bytes.Equal(mustMarshalConversation(t, got), encoded) {
		t.Fatalf("oldest-first policy changed for a short output: size %d, compacted %t, error %v", size, compacted, err)
	}
	// Rechecking a previously compacted context must be stable.
	again, size, compacted, err := boundCustomTurnInput(got, size)
	if err != nil || compacted || !bytes.Equal(mustMarshalConversation(t, again), encoded) {
		t.Fatalf("compacted context is not stable: %v", err)
	}
}

func TestBoundCustomTurnInputPreservesOutputMetadata(t *testing.T) {
	for _, kind := range []string{"typed", "extra fields", "output override", "union override"} {
		t.Run(kind, func(t *testing.T) {
			item := responses.ResponseInputItemParamOfFunctionCallOutput("call_1", strings.Repeat("large ", 512))
			item.OfFunctionCallOutput.ID = openai.String("output_1")
			item.OfFunctionCallOutput.Status = "completed"
			if err := json.Unmarshal([]byte(`{"type":"program","caller_id":"parent_1"}`), &item.OfFunctionCallOutput.Caller); err != nil {
				t.Fatal(err)
			}
			if kind == "extra fields" {
				item.OfFunctionCallOutput.SetExtraFields(map[string]any{"future_metadata": map[string]any{"count": 3}, "output": strings.Repeat("override ", 512)})
			}
			if kind == "output override" {
				raw := mustMarshalConversation(t, item.OfFunctionCallOutput)
				output := param.Override[responses.ResponseInputItemFunctionCallOutputParam](json.RawMessage(raw))
				item.OfFunctionCallOutput = &output
			}
			if kind == "union override" {
				raw := mustMarshalConversation(t, item)
				output := item.OfFunctionCallOutput
				item = param.Override[responses.ResponseInputItemUnionParam](json.RawMessage(raw))
				item.OfFunctionCallOutput = output
			}
			input := responses.ResponseInputParam{item}
			before := mustMarshalConversation(t, input)
			got, size, compacted, err := boundCustomTurnInput(input, 512)
			if err != nil || !compacted {
				t.Fatalf("compaction = %t, %v", compacted, err)
			}
			encoded := mustMarshalConversation(t, got)
			if size != len(encoded) || size > 512 {
				t.Fatalf("measured %d bytes, actual %d", size, len(encoded))
			}
			var original, bounded []map[string]any
			if err := json.Unmarshal(before, &original); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(encoded, &bounded); err != nil {
				t.Fatal(err)
			}
			original[0]["output"] = compactedToolOutput
			if !reflect.DeepEqual(bounded, original) {
				t.Fatalf("protocol metadata changed: got %s", encoded)
			}
			if !bytes.Equal(before, mustMarshalConversation(t, input)) {
				t.Fatal("original protocol fields were mutated")
			}
		})
	}
}

func TestBoundCustomTurnInputRejectsInvalidJSON(t *testing.T) {
	input := responses.ResponseInputParam{param.Override[responses.ResponseInputItemUnionParam](json.RawMessage(`{"broken":`))}
	if got, size, compacted, err := boundCustomTurnInput(input, 1024); err == nil || got != nil || size != 0 || compacted {
		t.Fatalf("invalid JSON: got %v, size %d, compacted %t, error %v", got, size, compacted, err)
	}
}

func FuzzBoundCustomTurnInput(f *testing.F) {
	f.Add("\"\\\n\t<>&\u2028\u2029世界\xff", uint16(3), uint16(256))
	f.Add(strings.Repeat("x", 256), uint16(8), uint16(512))
	f.Add("", uint16(1), uint16(1))
	f.Fuzz(func(t *testing.T, text string, count, budget uint16) {
		if len(text) > 4096 {
			t.Skip()
		}
		input := responses.ResponseInputParam{responses.ResponseInputItemParamOfMessage(text, responses.EasyInputMessageRoleUser)}
		for i := range int(count%8) + 1 {
			input = append(input, responses.ResponseInputItemParamOfFunctionCallOutput(fmt.Sprint(i), text))
		}
		original := mustMarshalConversation(t, input)
		limit := int(budget) % (len(original) + 1)
		// The intentionally slow oracle encodes the entire context after each
		// replacement and checks the first oldest-first prefix that fits.
		want := slices.Clone(input)
		encoded := original
		wantCompacted := false
		for i := 1; len(encoded) > limit && i < len(want); i++ {
			want[i] = responses.ResponseInputItemParamOfFunctionCallOutput(fmt.Sprint(i-1), compactedToolOutput)
			wantCompacted = true
			encoded = mustMarshalConversation(t, want)
		}
		got, size, compacted, err := boundCustomTurnInput(input, limit)
		if size != len(encoded) || compacted != wantCompacted || (err != nil) != (size > limit) {
			t.Fatalf("bound = size %d, compacted %t, error %v; want size %d, compacted %t", size, compacted, err, len(encoded), wantCompacted)
		}
		if err == nil && !bytes.Equal(mustMarshalConversation(t, got), encoded) {
			t.Fatal("output differs from whole-array encoding oracle")
		}
		if !bytes.Equal(mustMarshalConversation(t, input), original) {
			t.Fatal("original input changed")
		}
	})
}

func mustMarshalConversation(t testing.TB, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
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
