package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3/responses"
)

func toolInput(t *testing.T, value any) json.RawMessage {
	t.Helper()

	input, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return input
}

func TestReadFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "message.txt")
	if err := os.WriteFile(path, []byte("hello, agent"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := ReadFile(toolInput(t, map[string]string{"path": path}))
	if err != nil {
		t.Fatalf("ReadFile returned an error: %v", err)
	}
	if got != "hello, agent" {
		t.Fatalf("ReadFile returned %q, want %q", got, "hello, agent")
	}

	if _, err := ReadFile(toolInput(t, map[string]string{"path": ""})); err == nil {
		t.Fatal("ReadFile accepted an empty path")
	}
}

func TestListFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "root.txt"), []byte("root"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nested", "child.txt"), []byte("child"), 0o644); err != nil {
		t.Fatal(err)
	}

	output, err := ListFiles(toolInput(t, map[string]string{"path": dir}))
	if err != nil {
		t.Fatalf("ListFiles returned an error: %v", err)
	}

	var got []string
	if err := json.Unmarshal([]byte(output), &got); err != nil {
		t.Fatalf("ListFiles returned invalid JSON: %v", err)
	}
	want := []string{"nested/", filepath.Join("nested", "child.txt"), "root.txt"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ListFiles returned %#v, want %#v", got, want)
	}
}

func TestEditFileCreatesAndEditsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "message.txt")

	result, err := EditFile(toolInput(t, EditFileInput{
		Path:   path,
		OldStr: "",
		NewStr: "hello world",
	}))
	if err != nil {
		t.Fatalf("EditFile could not create a file: %v", err)
	}
	if !strings.Contains(result, path) {
		t.Fatalf("creation result %q does not mention %q", result, path)
	}

	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err = EditFile(toolInput(t, EditFileInput{
		Path:   path,
		OldStr: "world",
		NewStr: "Meldra",
	}))
	if err != nil {
		t.Fatalf("EditFile could not edit a file: %v", err)
	}
	if result != "OK" {
		t.Fatalf("edit result is %q, want OK", result)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "hello Meldra" {
		t.Fatalf("edited content is %q, want %q", content, "hello Meldra")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("edited permissions are %o, want 600", info.Mode().Perm())
	}
}

func TestEditFileRejectsInvalidOrAmbiguousEdits(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "repeated.txt")
	if err := os.WriteFile(path, []byte("same same"), 0o644); err != nil {
		t.Fatal(err)
	}
	emptyPath := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(emptyPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		raw   json.RawMessage
		input EditFileInput
	}{
		{name: "empty path", input: EditFileInput{OldStr: "old", NewStr: "new"}},
		{name: "unchanged strings", input: EditFileInput{Path: path, OldStr: "same", NewStr: "same"}},
		{name: "missing match", input: EditFileInput{Path: path, OldStr: "absent", NewStr: "new"}},
		{name: "duplicate match", input: EditFileInput{Path: path, OldStr: "same", NewStr: "new"}},
		{name: "empty match on existing file", input: EditFileInput{Path: emptyPath, OldStr: "", NewStr: "new"}},
		{name: "missing old_str", raw: toolInput(t, map[string]string{"path": filepath.Join(dir, "new.txt"), "new_str": "new"})},
		{name: "unknown property", raw: toolInput(t, map[string]any{"path": path, "old_str": "same same", "new_str": "new", "extra": true})},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := tt.raw
			if input == nil {
				input = toolInput(t, tt.input)
			}
			if _, err := EditFile(input); err == nil {
				t.Fatal("EditFile accepted invalid or ambiguous input")
			}
		})
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "same same" {
		t.Fatalf("failed edits changed the file to %q", content)
	}
	emptyContent, err := os.ReadFile(emptyPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(emptyContent) != 0 {
		t.Fatalf("failed creation-style edit changed an existing empty file to %q", emptyContent)
	}
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

func TestToolFollowUpInputIncludesCallsBeforeTheirOutputs(t *testing.T) {
	var output []responses.ResponseOutputItemUnion
	if err := json.Unmarshal([]byte(`[
		{"type":"function_call","call_id":"call_1","name":"echo","arguments":"{\"value\":\"one\"}"}
	]`), &output); err != nil {
		t.Fatal(err)
	}

	input := toolFollowUpInput(output, responses.ResponseInputParam{
		responses.ResponseInputItemParamOfFunctionCallOutput("call_1", "one"),
	})
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
