package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPFormExactNumbers(t *testing.T) {
	for _, tc := range []struct {
		name, kind, value string
		accept            bool
	}{
		{"large integer", "integer", "9007199254740993", true},
		{"maximum unsigned integer", "integer", "18446744073709551615", true},
		{"out of range integer", "integer", "18446744073709551616", false},
		{"fraction rounded by float64", "integer", "9007199254740993.5", false},
		{"exact binary decimal", "number", "0.125", true},
		{"ordinary decimal", "number", "0.1", true},
		{"inexact decimal", "number", "0.1234567890123456789", false},
		{"underflow", "number", "1e-400", false},
		{"overflow", "number", "1e400", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			answer := `{"value":` + tc.value + `}`
			w, _ := testWorkspace(t, t.TempDir(), answer+"\n", false)
			result, err := w.elicitMCP(t.Context(), "fixture", &mcp.ElicitParams{
				RequestedSchema: &jsonschema.Schema{Type: "object", Properties: map[string]*jsonschema.Schema{"value": {Type: tc.kind}}, Required: []string{"value"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if !tc.accept {
				if result.Action == "accept" {
					t.Fatalf("accepted fractional integer: %+v", result)
				}
				return
			}
			data, err := json.Marshal(result.Content)
			if err != nil || result.Action != "accept" || string(data) != answer {
				t.Fatalf("form changed the supplied number: action=%s content=%s error=%v", result.Action, data, err)
			}
		})
	}
}

func TestMCPFormRejectsNumbersInStringFields(t *testing.T) {
	for _, tc := range []struct{ kind, answer string }{
		{"string", `{"value":17}`},
		{"array", `{"value":[17]}`},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			w, _ := testWorkspace(t, t.TempDir(), tc.answer+"\n", false)
			field := &jsonschema.Schema{Type: tc.kind}
			if tc.kind == "array" {
				field.Items = &jsonschema.Schema{Type: "string", Enum: []any{"17"}}
			}
			result, err := w.elicitMCP(t.Context(), "fixture", &mcp.ElicitParams{RequestedSchema: &jsonschema.Schema{
				Type: "object", Properties: map[string]*jsonschema.Schema{"value": field}, Required: []string{"value"},
			}})
			if err != nil || result.Action == "accept" {
				t.Fatalf("numeric value accepted as a string: %+v, %v", result, err)
			}
		})
	}
}

func TestMCPFormNumericValidation(t *testing.T) {
	w, _ := testWorkspace(t, t.TempDir(), "{\"value\":\"5\"}\n{\"value\":11}\n{\"value\":5}\n", false)
	result, err := w.elicitMCP(t.Context(), "fixture", &mcp.ElicitParams{RequestedSchema: &jsonschema.Schema{
		Type: "object", Properties: map[string]*jsonschema.Schema{"value": {Type: "number", Minimum: new(0.0), Maximum: new(10.0)}}, Required: []string{"value"},
	}})
	if err != nil || result.Action != "accept" || fmt.Sprint(result.Content["value"]) != "5" {
		t.Fatalf("numeric type or bounds validation failed: %+v, %v", result, err)
	}
}

func TestMCPFormPasswordFormat(t *testing.T) {
	w, output := testWorkspace(t, t.TempDir(), "{\"value\":\"credential\"}\n", false)
	_, err := w.elicitMCP(t.Context(), "fixture", &mcp.ElicitParams{RequestedSchema: &jsonschema.Schema{
		Type: "object", Properties: map[string]*jsonschema.Schema{"value": {Type: "string", Format: "password"}},
	}})
	if err == nil || !strings.Contains(err.Error(), "sensitive elicitation") || output.Len() != 0 {
		t.Fatalf("sensitive format reached the form: error=%v output=%s", err, output)
	}
}

func TestMCPSamplingSharingUsesOuterDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var request struct{ Stream bool }
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if request.Stream {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"message":"stream is not supported","type":"invalid_request_error","code":"unsupported_parameter","param":"stream"}}`)
			return
		}
		fmt.Fprint(w, `{"id":"resp_sampling","object":"response","status":"completed","output":[{"type":"message","id":"msg_sampling","role":"assistant","status":"completed","content":[{"type":"output_text","text":"sampling result","annotations":[]}]}]}`)
	}))
	defer server.Close()
	paths, err := ConfigPathsForHome(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := InitializeConfig(paths); err != nil {
		t.Fatal(err)
	}
	if err := writePrivateFile(paths, paths.ConfigFile, []byte("model=\"fixture\"\nbase_url=\""+server.URL+"/v1\"\nallow_insecure_base_url=true\n")); err != nil {
		t.Fatal(err)
	}
	t.Setenv(MeldraHomeEnv, paths.Home)
	t.Setenv("OPENAI_API_KEY", "fixture-key")
	t.Setenv("OPENAI_BASE_URL", "")
	w, _ := testWorkspace(t, t.TempDir(), "", false)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	want, _ := ctx.Deadline()
	shared := false
	w.SetApprovalFunc(func(ctx context.Context, request ApprovalRequest) bool {
		if request.Title == "Share MCP sampling result" {
			shared = true
			if got, ok := ctx.Deadline(); !ok || !got.Equal(want) {
				t.Errorf("sharing consumed inference deadline: got %s, want %s", got, want)
			}
		}
		return true
	})
	_, err = w.sampleMCP(ctx, "fixture", &mcp.CreateMessageParams{MaxTokens: 17, Messages: []*mcp.SamplingMessage{{Role: "user", Content: &mcp.TextContent{Text: "sample"}}}})
	if err != nil || !shared {
		t.Fatalf("sampling did not request sharing consent: shared=%t error=%v", shared, err)
	}
}
