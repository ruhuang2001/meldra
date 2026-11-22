package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"meldra/internal/task"
	"meldra/internal/tool"
)

func TestMCPDuringManagedProcessRequiresConfiguredToolAndApproval(t *testing.T) {
	var dispatched atomic.Int64
	server := mcp.NewServer(&mcp.Implementation{Name: "browser-fixture", Version: "1"}, nil)
	pixels := screenshotFixture(t, "png")
	for _, name := range []string{"screenshot", "other"} {
		server.AddTool(&mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			dispatched.Add(1)
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.ImageContent{MIMEType: "image/png", Data: pixels}}}, nil
		})
	}
	endpoint := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true}))
	defer endpoint.Close()
	for _, scenario := range []string{"default", "approved", "declined", "other-tool", "other-server", "unadvertised", "plan", "changed-mode", "unknown-during-approval", "persistence-during-approval"} {
		t.Run(scenario, func(t *testing.T) {
			w, processes, executable := managedWorkspace(t)
			paths, err := ConfigPathsForHome(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := InitializeConfig(paths); err != nil {
				t.Fatal(err)
			}
			config := mcpServerConfig{URL: endpoint.URL}
			if scenario != "default" {
				config.AllowDuringProcess = []string{"screenshot"}
			}
			if scenario == "unadvertised" {
				config.AllowDuringProcess = []string{"missing"}
			}
			raw, _ := json.Marshal(map[string]any{"mcpServers": map[string]mcpServerConfig{"chrome": config, "untrusted": {URL: endpoint.URL}}})
			if err := os.WriteFile(filepath.Join(paths.Home, "mcp.json"), raw, 0600); err != nil {
				t.Fatal(err)
			}
			connections, err := connectMCP(t.Context(), paths, w, "")
			if err != nil {
				t.Fatal(err)
			}
			defer connections.close()
			if scenario == "unadvertised" {
				if len(connections.warnings) != 1 {
					t.Fatalf("unadvertised grant was accepted: %v", connections.warnings)
				}
				for _, definition := range connections.tools {
					if strings.HasPrefix(definition.Name, "mcp__chrome__") {
						t.Fatal("invalid server exposed callable tools")
					}
				}
				return
			}
			if len(connections.warnings) != 0 {
				t.Fatal(connections.warnings)
			}
			remote, err := tool.New(connections.tools)
			if err != nil {
				t.Fatal(err)
			}
			view := invokeProcess(t, processes, "start_process", helperInput(executable, "wait", 60))
			before := dispatched.Load()
			w.autoApprove = false
			w.policy.permissions = PermissionWorkspaceEdit
			approvals := 0
			persistenceFailure := errors.New("injected process persistence failure")
			w.SetApprovalFunc(func(ctx context.Context, request ApprovalRequest) bool {
				approvals++
				if request.MCPBinding == "" || !strings.Contains(request.Detail, "trust grant") {
					t.Error("MCP approval omitted connection identity or process grant")
				}
				if scenario == "changed-mode" {
					_ = w.policy.setMode(ModePlan)
				}
				if scenario == "unknown-during-approval" {
					p := w.processes.processes[view.Process.ID]
					p.cancel()
					<-p.done
				}
				if scenario == "persistence-during-approval" {
					w.processes.mu.Lock()
					w.processes.err = persistenceFailure
					w.processes.mu.Unlock()
				}
				return scenario != "declined"
			})
			name := "mcp__chrome__screenshot"
			if scenario == "other-tool" {
				name = "mcp__chrome__other"
			}
			if scenario == "other-server" {
				name = "mcp__untrusted__screenshot"
			}
			if scenario == "plan" {
				_ = w.policy.setMode(ModePlan)
			}
			result, err := remote.Invoke(t.Context(), name, json.RawMessage(`{}`))
			switch scenario {
			case "approved":
				if err != nil || result.Status != tool.Succeeded || len(result.Content) == 0 || result.Content[0].Image == nil || dispatched.Load() != before+1 {
					t.Fatalf("configured browser call failed: %+v %v", result, err)
				}
				if _, err := processes.Invoke(t.Context(), "edit_file", json.RawMessage(`{"path":"bad.txt","old_str":"","new_str":"bad"}`)); !errors.Is(err, ErrWorkspaceBusy) {
					t.Fatalf("browser grant widened file permissions: %v", err)
				}
			case "default", "other-tool", "other-server":
				if !errors.Is(err, ErrWorkspaceBusy) || approvals != 0 {
					t.Fatalf("unconfigured tool admitted: %+v %v", result, err)
				}
			case "declined":
				if err != nil || result.Status != tool.Declined || approvals != 1 {
					t.Fatalf("workspace-edit bypassed MCP approval: %+v %v", result, err)
				}
			case "plan":
				if err == nil || !strings.Contains(err.Error(), "Plan mode") || approvals != 0 {
					t.Fatalf("process grant bypassed Plan: %+v %v", result, err)
				}
			case "changed-mode":
				if !errors.Is(err, ErrOperationSuperseded) || approvals != 1 {
					t.Fatalf("stale approval admitted: %+v %v", result, err)
				}
			case "unknown-during-approval":
				if !errors.Is(err, task.ErrUnresolved) || approvals != 1 {
					t.Fatalf("unknown process effects bypassed at admission: %+v %v", result, err)
				}
			case "persistence-during-approval":
				if !errors.Is(err, persistenceFailure) || approvals != 1 {
					t.Fatalf("persistence failure bypassed at admission: %+v %v", result, err)
				}
				w.processes.mu.Lock()
				w.processes.err = nil
				w.processes.mu.Unlock()
			}
			if scenario != "approved" && dispatched.Load() != before {
				t.Fatal("denied request reached external server")
			}
			_ = invokeProcess(t, processes, "stop_process", map[string]any{"process_id": view.Process.ID})
			_ = w.policy.setMode(ModeBuild)
			if _, err := remote.Invoke(t.Context(), name, json.RawMessage(`{}`)); !errors.Is(err, task.ErrUnresolved) {
				t.Fatalf("unknown effects bypassed after cancellation: %v", err)
			}
		})
	}
}

func TestMCPProcessGrantConfigAndConnectionIdentity(t *testing.T) {
	paths, err := ConfigPathsForHome(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := InitializeConfig(paths); err != nil {
		t.Fatal(err)
	}
	w, _ := testWorkspace(t, t.TempDir(), "", true)
	config := mcpServerConfig{URL: "http://127.0.0.1:12345/mcp"}
	before, err := mcpConnectionBinding(t.Context(), paths, "chrome", config, w)
	if err != nil {
		t.Fatal(err)
	}
	config.AllowDuringProcess = []string{"screenshot"}
	after, err := mcpConnectionBinding(t.Context(), paths, "chrome", config, w)
	if err != nil || before == after {
		t.Fatal("changing the grant did not invalidate connection approvals", err)
	}
	for _, names := range [][]string{{}, {"screenshot"}, {"*"}, {""}, {"screenshot \n"}, {strings.Repeat("x", 257)}, make([]string, 129)} {
		config.AllowDuringProcess = names
		raw, _ := json.Marshal(map[string]any{"mcpServers": map[string]mcpServerConfig{"chrome": config}})
		if err := os.WriteFile(filepath.Join(paths.Home, "mcp.json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
		_, err := loadMCPConfig(paths)
		wantValid := len(names) == 0 || len(names) == 1 && names[0] == "screenshot"
		if (err == nil) != wantValid {
			t.Fatalf("grant validation %q: %v", names, err)
		}
	}
}
