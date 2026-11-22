package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"meldra/internal/provider"
	taskstore "meldra/internal/store"
	"meldra/internal/task"
	"meldra/internal/tool"
)

func screenshotFixture(t *testing.T, format string) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 3, 2))
	img.Set(1, 0, color.RGBA{R: 37, G: 141, B: 239, A: 255})
	var data bytes.Buffer
	var err error
	if format == "jpeg" {
		err = jpeg.Encode(&data, img, nil)
	} else {
		err = png.Encode(&data, img)
	}
	if err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func TestMCPImageResultsThroughAgent(t *testing.T) {
	for _, custom := range []bool{false, true} {
		t.Run(fmt.Sprint(custom), func(t *testing.T) {
			pixels := screenshotFixture(t, "png")
			server := mcp.NewServer(&mcp.Implementation{Name: "screenshots", Version: "1"}, nil)
			calls := 0
			server.AddTool(&mcp.Tool{Name: "shot", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				calls++
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "first"}, &mcp.ImageContent{MIMEType: "image/png", Data: pixels}, &mcp.TextContent{Text: "last"}}}, nil
			})
			ct, st := mcp.NewInMemoryTransports()
			ss, err := server.Connect(t.Context(), st, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer ss.Close()
			client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
			cs, err := client.Connect(t.Context(), ct, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer cs.Close()
			a, paths := recordedAgent(t, nil, nil)
			w := a.execution.workspace
			schema, err := (&jsonschema.Schema{Type: "object"}).Resolve(nil)
			if err != nil {
				t.Fatal(err)
			}
			a.tools = []ToolDefinition{{Name: "mcp__test__shot", Parameters: map[string]any{"type": "object"}, Function: w.bindTool(func(raw json.RawMessage) (string, error) { return w.callMCP(cs, "test", "shot", nil, schema, raw) })}}
			a.customProvider = custom
			wires := make(chan []byte, 1)
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				wire, _ := io.ReadAll(r.Body)
				wires <- wire
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"wire-check\",\"status\":\"completed\",\"output\":[]}}\n\n")
			}))
			defer endpoint.Close()
			adapter := provider.Connect(provider.Connection{APIKey: "test", BaseURL: endpoint.URL})
			requests := 0
			a.backend = inferenceFunc(func(ctx context.Context, request provider.Request, _ provider.Options, _ provider.Observer) (provider.Result, error) {
				requests++
				if requests > 1 {
					if _, err := adapter.Infer(ctx, request, provider.Options{CustomProvider: custom}, provider.Observer{}); err != nil {
						t.Fatal(err)
					}
					input := string(<-wires)
					if !strings.Contains(input, "data:image/png;base64,"+base64.StdEncoding.EncodeToString(pixels)) || !strings.Contains(input, "shot-call") {
						t.Fatal("provider did not receive the image attached to its call")
					}
					stored, err := a.execution.db.ToolCalls(ctx, a.session.ID)
					if err != nil || len(stored) != 1 {
						t.Fatalf("calls: %v, %v", stored, err)
					}
					found := false
					for _, ref := range stored[0].Result.Artifacts {
						if ref.Name == "mcp-image-2.png" {
							data, err := a.execution.db.ReadArtifact(ctx, ref)
							if err != nil || !bytes.Equal(data, pixels) {
								t.Fatal("image not durably saved before inference")
							}
							found = true
						}
					}
					if !found || strings.Contains(stored[0].Result.Output, base64.StdEncoding.EncodeToString(pixels)) {
						t.Fatal("missing binary evidence or base64 in text ledger")
					}
				}
				if requests <= 2 {
					return provider.Result{Response: steeringBatchResponse(t, "same-response", map[string]any{"id": "shot-call", "name": "mcp__test__shot", "arguments": map[string]any{}})}, nil
				}
				return provider.Result{Response: finishResponse()}, nil
			})
			if err := a.RunTurn(t.Context(), "inspect screenshot"); err != nil {
				t.Fatal(err)
			}
			if calls != 1 || requests != 3 {
				t.Fatalf("calls=%d requests=%d; duplicate must reuse original image", calls, requests)
			}
			db := openTaskDB(t, paths)
			approvals, err := db.Approvals(t.Context(), a.session.ID)
			if err != nil || len(approvals) != 1 {
				t.Fatalf("approval evidence: %v %v", approvals, err)
			}
			stored, err := db.ToolCalls(t.Context(), a.session.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, ref := range stored[0].Result.Artifacts {
				if ref.Name == "mcp-image-2.png" {
					if err := os.WriteFile(filepath.Join(taskDirectory(paths), "artifacts", ref.ID), []byte("corrupted"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			restored := &taskExecution{db: db}
			if _, err := restored.restoreMCPContent(t.Context(), stored[0].Result); err == nil {
				t.Fatal("corrupted screenshot evidence was accepted")
			}
		})
	}
}

func TestMCPImageValidationAndTextCompatibility(t *testing.T) {
	pngBytes, jpegBytes := screenshotFixture(t, "png"), screenshotFixture(t, "jpeg")
	for _, tc := range []struct {
		name, mime string
		data       []byte
		accepted   bool
	}{
		{"png", "image/png", pngBytes, true}, {"jpeg", "image/jpeg", jpegBytes, true},
		{"mismatch", "image/jpeg", pngBytes, false}, {"corrupt", "image/png", []byte("not image"), false},
		{"empty", "image/png", nil, false}, {"webp", "image/webp", pngBytes, false},
		{"oversize", "image/png", make([]byte, (1<<20)+1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg, err := tool.New([]ToolDefinition{{Name: "shot", Function: func(ctx context.Context, _ json.RawMessage) (string, error) {
				return mcpToolResult(ctx, &mcp.CallToolResult{Content: []mcp.Content{&mcp.ImageContent{MIMEType: tc.mime, Data: tc.data}}})
			}}})
			if err != nil {
				t.Fatal(err)
			}
			result, err := reg.Invoke(t.Context(), "shot", json.RawMessage(`{}`))
			if err != nil || (len(result.Content) > 0) != tc.accepted || result.Truncated == tc.accepted {
				t.Fatalf("unexpected result: %+v, %v", result, err)
			}
			if !tc.accepted && !strings.Contains(result.Output, "not provided to model") {
				t.Fatal("image silently omitted")
			}
		})
	}
	for _, isError := range []bool{false, true} {
		want := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Saved screenshot to /outside/secret.png"}}, IsError: isError, StructuredContent: map[string]any{"count": 7}}
		encoded, _ := json.Marshal(want)
		got, err := mcpToolResult(t.Context(), want)
		if got != string(encoded) || (err != nil) != isError {
			t.Fatalf("text result changed: %s %v", got, err)
		}
	}
}

func TestMCPImageArtifactQuotaStopsInference(t *testing.T) {
	a, _ := recordedAgent(t, nil, nil)
	pixels := screenshotFixture(t, "png")
	a.tools = []ToolDefinition{{Name: "shot", Parameters: map[string]any{"type": "object"}, Function: func(ctx context.Context, _ json.RawMessage) (string, error) {
		output, err := mcpToolResult(ctx, &mcp.CallToolResult{Content: []mcp.Content{&mcp.ImageContent{MIMEType: "image/png", Data: pixels}}})
		ref, saveErr := a.execution.db.PutArtifact(ctx, a.execution.lease, a.session.ID, "quota-fixture", []byte("quota fixture"))
		if saveErr != nil {
			t.Fatal(saveErr)
		}
		path := filepath.Join(taskDirectory(a.execution.paths), "artifacts", ref.ID)
		if err := os.Truncate(path, taskstore.MaxArtifactTotalBytes); err != nil {
			t.Fatal(err)
		}
		return output, err
	}}}
	requests := 0
	a.backend = inferenceFunc(func(context.Context, provider.Request, provider.Options, provider.Observer) (provider.Result, error) {
		requests++
		return provider.Result{Response: callResponse("quota-shot", "shot", `{}`)}, nil
	})
	if err := a.RunTurn(t.Context(), "inspect image"); err == nil || requests != 1 {
		t.Fatalf("unsaved image reached inference: requests=%d error=%v", requests, err)
	}
	db := openTaskDB(t, a.execution.paths)
	calls, err := db.ToolCalls(t.Context(), a.session.ID)
	if err != nil || len(calls) != 1 || calls[0].Status != task.ToolSucceeded || !calls[0].Result.Truncated {
		t.Fatalf("lost known tool outcome: %v %v", calls, err)
	}
	restored := &taskExecution{db: db}
	if _, err := restored.restoreMCPContent(t.Context(), calls[0].Result); err == nil {
		t.Fatal("incomplete screenshot evidence was silently reused as text")
	}
}

func TestMCPImageAggregateLimits(t *testing.T) {
	pixels := screenshotFixture(t, "png")
	for _, tc := range []struct {
		name               string
		bytes, count, want int
	}{
		{"exact bytes", 1 << 20, 2, 2},
		{"aggregate bytes", 1 << 20, 3, 2},
		{"exact count", len(pixels), 32, 32},
		{"aggregate count", len(pixels), 33, 32},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := make([]byte, tc.bytes)
			copy(data, pixels)
			contents := []mcp.Content{&mcp.TextContent{Text: strings.Repeat("x", maxToolOutput+1)}}
			for range tc.count {
				contents = append(contents, &mcp.ImageContent{MIMEType: "image/png", Data: data})
			}
			reg, err := tool.New([]ToolDefinition{{Name: "shot", Function: func(ctx context.Context, _ json.RawMessage) (string, error) {
				return mcpToolResult(ctx, &mcp.CallToolResult{Content: contents, IsError: true})
			}}})
			if err != nil {
				t.Fatal(err)
			}
			result, err := reg.Invoke(t.Context(), "shot", json.RawMessage(`{}`))
			count := 0
			for _, part := range result.Content {
				if part.Image != nil {
					count++
				}
			}
			if err == nil || result.Status != tool.Failed || !result.Truncated || count != tc.want {
				t.Fatalf("count=%d want=%d status=%s truncated=%v err=%v", count, tc.want, result.Status, result.Truncated, err)
			}
		})
	}
}
