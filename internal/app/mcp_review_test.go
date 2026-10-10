package app

import (
	"encoding/json"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPArgumentsRejectLossyNumericTokens(t *testing.T) {
	for _, input := range []string{`{"value":9007199254740993}`, `{"items":[0.1234567890123456789]}`, `{"nested":{"value":1e-400}}`, `{} {}`} {
		if _, err := decodeMCPArguments(json.RawMessage(input)); err == nil {
			t.Fatalf("accepted lossy or invalid arguments: %s", input)
		}
	}
	for _, input := range []string{`{"value":0.1}`, `{"value":9007199254740992}`, `{}`} {
		if _, err := decodeMCPArguments(json.RawMessage(input)); err != nil {
			t.Fatalf("rejected exact arguments %s: %v", input, err)
		}
	}
}

func TestMCPFormNumericFailureSurvivesOtherFields(t *testing.T) {
	// Repeated maps exercise both field orders; a later valid field cannot
	// overwrite a precision rejection from the numeric field.
	for range 40 {
		w, _ := testWorkspace(t, t.TempDir(), "{\"number\":0.1234567890123456789,\"text\":\"valid\"}\n", false)
		result, err := w.elicitMCP(t.Context(), "fixture", &mcp.ElicitParams{RequestedSchema: &jsonschema.Schema{Type: "object", Properties: map[string]*jsonschema.Schema{"number": {Type: "number"}, "text": {Type: "string"}}, Required: []string{"number", "text"}}})
		if err == nil && result.Action == "accept" {
			t.Fatal("valid field overwrote numeric precision rejection")
		}
	}
}

func TestMCPConfiguredPATHRequiresExplicitCommand(t *testing.T) {
	w, _ := testWorkspace(t, t.TempDir(), "", false)
	command := newMCPCommand(mcpServerConfig{Command: "fixture", Env: map[string]string{"PATH": t.TempDir()}}, w)
	if command.Err == nil {
		t.Fatal("bare command ignored configured PATH")
	}
}
