package provider

import (
	"encoding/json"
	"github.com/openai/openai-go/v3/responses"
	"testing"
)

func TestMergeKeepsCompleteToolIdentity(t *testing.T) {
	decode := func(raw string) responses.ResponseOutputItemUnion {
		var item responses.ResponseOutputItemUnion
		if err := json.Unmarshal([]byte(raw), &item); err != nil {
			t.Fatal(err)
		}
		return item
	}
	full := decode(`{"type":"function_call","id":"fc","call_id":"call","name":"read_file","arguments":"{\"path\":\"file.txt\"}","status":"completed"}`)
	partial := decode(`{"type":"function_call","id":"fc","name":"read_file","status":"completed"}`)
	for _, pair := range [][2]responses.ResponseOutputItemUnion{{full, partial}, {partial, full}} {
		merged, err := mergeCompletedStreamOutput([]responses.ResponseOutputItemUnion{pair[0]}, []responses.ResponseOutputItemUnion{pair[1]})
		if err != nil || len(merged) != 1 {
			t.Fatalf("merge=%+v %v", merged, err)
		}
		response := domainResponse(&responses.Response{Status: "completed", Output: merged})
		if err := Validate(response); err != nil {
			t.Fatal(err)
		}
		call := response.Output[0]
		if call.CallID != "call" || call.Name != "read_file" || call.Arguments != `{"path":"file.txt"}` {
			t.Fatalf("lost terminal fields: %+v", call)
		}
		followup, err := FollowUp(response.Output, Items{ToolOutput("call", "content")})
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(followup)
		if err != nil {
			t.Fatal(err)
		}
		var items []map[string]any
		if err := json.Unmarshal(encoded, &items); err != nil {
			t.Fatal(err)
		}
		if items[0]["call_id"] != "call" || items[0]["arguments"] != `{"path":"file.txt"}` {
			t.Fatalf("SDK replay fields differ: %s", encoded)
		}
	}
	for _, raw := range []string{
		`{"type":"function_call","id":"fc","call_id":"different","name":"read_file","arguments":"{}"}`,
		`{"type":"function_call","id":"fc","call_id":"call","name":"edit_file","arguments":"{}"}`,
		`{"type":"function_call","id":"fc","call_id":"call","name":"read_file","arguments":"{}"}`,
	} {
		if _, err := mergeCompletedStreamOutput([]responses.ResponseOutputItemUnion{full}, []responses.ResponseOutputItemUnion{decode(raw)}); err == nil {
			t.Fatal("conflicting completed call accepted")
		}
	}
}
