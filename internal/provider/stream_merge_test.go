package provider

import (
	"encoding/json"
	"testing"

	"github.com/openai/openai-go/v3/responses"
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

func TestMergeToolArgumentsSemantically(t *testing.T) {
	item := func(args string) responses.ResponseOutputItemUnion {
		encoded, err := json.Marshal(map[string]any{"type": "function_call", "id": "fc", "call_id": "call", "name": "edit_file", "arguments": args})
		if err != nil {
			t.Fatal(err)
		}
		var value responses.ResponseOutputItemUnion
		if err := json.Unmarshal(encoded, &value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	for _, test := range []struct {
		terminal, streamed string
		conflict           bool
	}{
		{`{"a":1,"b":[true,null]}`, `{ "b": [true, null], "a":1.0 }`, false},
		{`{"a":1}`, `{"a":`, false},
		{`{"a":`, `{"a":1}`, false},
		{`{"a":9007199254740992}`, `{"a":9007199254740993}`, true},
		{`{"a":1e100000000}`, `{"a":1e100000000}`, false},
		{`{"a":1}`, `{"a":2}`, true},
	} {
		merged, err := mergeFunctionCall(item(test.terminal), item(test.streamed))
		if (err != nil) != test.conflict {
			t.Fatalf("merge %s / %s: %v", test.terminal, test.streamed, err)
		}
		if err == nil && json.Valid([]byte(test.terminal)) && merged.AsFunctionCall().Arguments != test.terminal {
			t.Fatal("valid terminal arguments not authoritative")
		}
	}
}
