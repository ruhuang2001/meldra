package app

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"meldra/internal/task"
)

type permissionReviewMCP struct {
	t          *testing.T
	connection net.Conn
	decoder    *json.Decoder
	encoder    *json.Encoder
	sequence   int
	latest     bool
}

func newPermissionReviewMCP(t *testing.T, workspace string, latest bool, permissions PermissionProfile) *permissionReviewMCP {
	t.Helper()
	client, server := net.Pipe()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- runMCPServe(ctx, []string{"--workspace", workspace, "--permissions", string(permissions)}, server, server)
	}()
	t.Cleanup(func() {
		cancel()
		_ = client.Close()
		_ = server.Close()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("MCP permission test server did not stop")
		}
	})
	c := &permissionReviewMCP{t: t, connection: client, decoder: json.NewDecoder(client), encoder: json.NewEncoder(client), latest: latest}
	if !latest {
		c.request("initialize", map[string]any{"protocolVersion": "2025-11-25", "clientInfo": map[string]any{"name": "permission-review", "version": "1"}, "capabilities": map[string]any{"elicitation": map[string]any{"form": map[string]any{}}}})
		if err := c.encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

func (c *permissionReviewMCP) request(method string, params map[string]any) map[string]any {
	c.t.Helper()
	c.sequence++
	if c.latest {
		params["_meta"] = map[string]any{"io.modelcontextprotocol/protocolVersion": "2026-07-28", "io.modelcontextprotocol/clientInfo": map[string]any{"name": "permission-review", "version": "1"}, "io.modelcontextprotocol/clientCapabilities": map[string]any{"elicitation": map[string]any{"form": map[string]any{}}}}
	}
	if err := c.connection.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		c.t.Fatal(err)
	}
	if err := c.encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": c.sequence, "method": method, "params": params}); err != nil {
		c.t.Fatal(err)
	}
	var response map[string]any
	if err := c.decoder.Decode(&response); err != nil {
		c.t.Fatal(err)
	}
	if response["method"] != nil || response["error"] != nil {
		c.t.Fatalf("unexpected MCP interaction or protocol error: %+v", response)
	}
	result, ok := response["result"].(map[string]any)
	if !ok {
		c.t.Fatalf("missing MCP result: %+v", response)
	}
	return result
}

func (c *permissionReviewMCP) call(name string, arguments map[string]any) map[string]any {
	c.t.Helper()
	return c.request("tools/call", map[string]any{"name": name, "arguments": arguments})
}

func TestMCPWorkspaceEditHonorsPolicyAcrossProtocols(t *testing.T) {
	for _, latest := range []bool{false, true} {
		protocol := "2025-11-25"
		if latest {
			protocol = "2026-07-28"
		}
		t.Run(protocol, func(t *testing.T) {
			paths, err := ConfigPathsForHome(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv(MeldraHomeEnv, paths.Home)
			w := contextWorkspace(t)
			writeContextFixture(t, w, "AGENTS.md", "Use checked file operations.")
			writeContextFixture(t, w, "hello.txt", "old")
			c := newPermissionReviewMCP(t, w.root, latest, PermissionWorkspaceEdit)
			if result := c.call("read_project_context", map[string]any{"path": nil}); result["isError"] == true {
				t.Fatalf("context failed: %+v", result)
			}
			for _, operation := range []struct {
				name      string
				arguments map[string]any
				want      string
			}{
				{"edit_file", map[string]any{"path": "hello.txt", "old_str": "old", "new_str": "edited"}, "edited"},
				{"apply_patch", map[string]any{"patch": nil, "changes": []any{map[string]any{"path": "hello.txt", "old_str": "edited", "new_str": "patched"}}}, "patched"},
				{"undo_last_change", map[string]any{}, "edited"},
			} {
				result := c.call(operation.name, operation.arguments)
				if result["isError"] == true || result["inputRequests"] != nil || result["requestState"] != nil {
					t.Fatalf("workspace-edit did not authorize %s: %+v", operation.name, result)
				}
				data, err := os.ReadFile(filepath.Join(w.root, "hello.txt"))
				if err != nil || string(data) != operation.want {
					t.Fatalf("%s produced %q: %v", operation.name, data, err)
				}
			}
			db := openTaskDB(t, paths)
			records, err := db.ListTasks(t.Context(), w.root, 10)
			if err != nil {
				t.Fatal(err)
			}
			grants := 0
			for _, record := range records {
				approvals, err := db.Approvals(t.Context(), record.ID)
				if err != nil {
					t.Fatal(err)
				}
				for _, approval := range approvals {
					if approval.Decision == task.Approved && strings.Contains(string(approval.Detail), `"source":"workspace-edit-policy"`) {
						grants++
					}
				}
			}
			if grants != 3 {
				t.Fatalf("expected three recorded workspace-edit grants, got %d", grants)
			}
			writeContextFixture(t, w, "AGENTS.md", "Changed instructions.")
			result := c.call("edit_file", map[string]any{"path": "hello.txt", "old_str": "edited", "new_str": "stale"})
			if result["isError"] != true {
				t.Fatalf("workspace-edit bypassed changed project instructions: %+v", result)
			}
			data, err := os.ReadFile(filepath.Join(w.root, "hello.txt"))
			if err != nil || string(data) != "edited" {
				t.Fatalf("stale context modified file: %q %v", data, err)
			}
			if latest {
				if result := c.call("read_project_context", map[string]any{"path": nil}); result["isError"] == true {
					t.Fatalf("context reload failed: %+v", result)
				}
				result := c.call("run_command", map[string]any{"command": "python3", "args": []any{"hello.py"}, "timeout": nil})
				if result["resultType"] != "input_required" {
					t.Fatalf("workspace-edit authorized command without approval: %+v", result)
				}
			}
		})
	}
}

func TestMCPInteractiveStillRequiresEditApproval(t *testing.T) {
	t.Setenv(MeldraHomeEnv, t.TempDir())
	w := contextWorkspace(t)
	writeContextFixture(t, w, "hello.txt", "old")
	c := newPermissionReviewMCP(t, w.root, true, PermissionInteractive)
	result := c.call("edit_file", map[string]any{"path": "hello.txt", "old_str": "old", "new_str": "new"})
	if result["resultType"] != "input_required" {
		t.Fatalf("interactive edit did not require approval: %+v", result)
	}
	data, err := os.ReadFile(filepath.Join(w.root, "hello.txt"))
	if err != nil || string(data) != "old" {
		t.Fatalf("unapproved interactive edit modified file: %q %v", data, err)
	}
}
