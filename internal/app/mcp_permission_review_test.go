package app

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"slices"
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

func TestMCPApprovalUnderLeaseContention(t *testing.T) {
	for _, name := range []string{"retry", "invalidate", "decline"} {
		t.Run(name, func(t *testing.T) {
			paths := mustConfigPaths(t)
			t.Setenv(MeldraHomeEnv, paths.Home)
			w := contextWorkspace(t)
			writeContextFixture(t, w, "target", "old")
			c := newPermissionReviewMCP(t, w.root, true, PermissionInteractive)
			args := map[string]any{"path": "target", "old_str": "old", "new_str": "new"}
			challenge := c.call("edit_file", args)
			if challenge["resultType"] != "input_required" {
				t.Fatalf("challenge: %v", challenge)
			}
			redeem := func(approve bool) map[string]any {
				return c.request("tools/call", map[string]any{"name": "edit_file", "arguments": args, "requestState": challenge["requestState"], "inputResponses": map[string]any{"approval": map[string]any{"action": "accept", "content": map[string]any{"approve": approve}}}})
			}
			db := openTaskDB(t, paths)
			lease, err := db.Acquire(t.Context(), "other-owner", w.root)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Close()
			if name == "invalidate" {
				args["new_str"] = "altered"
			}
			if got := redeem(name != "decline"); got["isError"] != true {
				t.Fatalf("redeemed while busy: %v", got)
			}
			if err := lease.Close(); err != nil {
				t.Fatal(err)
			}
			args["new_str"] = "new"
			if got := redeem(true); (got["isError"] == true) != (name != "retry") {
				t.Fatalf("redemption after lease release: %v", got)
			}
			if got := redeem(true); got["isError"] != true {
				t.Fatalf("reused approval: %v", got)
			}
			records, err := db.ListTasks(t.Context(), w.root, 10)
			if err != nil || len(records) != 1 {
				t.Fatalf("records=%v err=%v", records, err)
			}
			calls, err := db.ToolCalls(t.Context(), records[0].ID)
			if err != nil || len(calls) != 1 {
				t.Fatalf("calls=%v err=%v", calls, err)
			}
			wantText, wantTask, wantCall := "new", task.Completed, task.ToolSucceeded
			if name == "invalidate" {
				wantText, wantTask, wantCall = "old", task.Failed, task.ToolCancelled
			} else if name == "decline" {
				wantText, wantCall = "old", task.ToolDeclined
			}
			data, err := os.ReadFile(filepath.Join(w.root, "target"))
			if err != nil || string(data) != wantText || records[0].Status != wantTask || calls[0].Status != wantCall || calls[0].StartedAt.IsZero() != (name != "retry") {
				t.Fatalf("file=%q task=%s call=%+v err=%v", data, records[0].Status, calls[0], err)
			}
		})
	}
}

func TestMCPReviewHooksShareOneRecordedCall(t *testing.T) {
	for _, denyAt := range []string{"PreToolUse", "PermissionRequest", "accept"} {
		t.Run(denyAt, func(t *testing.T) {
			paths := mustConfigPaths(t)
			t.Setenv(MeldraHomeEnv, paths.Home)
			dir := t.TempDir()
			log := filepath.Join(dir, "events.jsonl")
			script := writeHookScript(t, dir, "observe.sh", `cat >> "`+log+`"; echo >> "`+log+`"
if [ "$1" = "`+denyAt+`" ]; then exit 2; fi
if [ "$1" = PreToolUse ]; then echo '{"hookSpecificOutput":{"hookEventName":"PreToolUse","additionalContext":"challenge-context"}}'; fi`)
			groups := map[string]any{}
			for _, name := range []string{"PreToolUse", "PermissionRequest", "PostToolUse", "PostToolUseFailure", "RunEnd"} {
				groups[name] = []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": script, "args": []string{name}}}}}
			}
			cfg, err := json.Marshal(map[string]any{"hooks": groups})
			if err != nil {
				t.Fatal(err)
			}
			writeHooksConfig(t, paths, string(cfg))
			w := contextWorkspace(t)
			writeContextFixture(t, w, "target", "old")
			c := newPermissionReviewMCP(t, w.root, true, PermissionInteractive)
			args := map[string]any{"path": "target", "old_str": "old", "new_str": "new"}
			result := c.call("edit_file", args)
			db := openTaskDB(t, paths)
			records, err := db.ListTasks(t.Context(), w.root, 10)
			if err != nil || len(records) != 1 {
				t.Fatalf("expected a durable task at first response: %+v %v", records, err)
			}
			wantText, wantStatus := "old", task.ToolDeclined
			wantEvents := []string{"PreToolUse"}
			if denyAt != "PreToolUse" {
				wantEvents = append(wantEvents, "PermissionRequest")
			}
			if denyAt == "accept" {
				if result["resultType"] != "input_required" || records[0].Status != task.WaitingApproval {
					t.Fatalf("challenge: %+v %+v", result, records[0])
				}
				result = c.request("tools/call", map[string]any{"name": "edit_file", "arguments": args, "requestState": result["requestState"], "inputResponses": map[string]any{"approval": map[string]any{"action": "accept", "content": map[string]any{"approve": true}}}})
				encoded, err := json.Marshal(result["content"])
				if err != nil || result["isError"] == true || strings.Count(string(encoded), "challenge-context") != 1 {
					t.Fatalf("redemption: %+v %v", result, err)
				}
				wantEvents = append(wantEvents, "RunEnd", "PostToolUse")
				wantText, wantStatus = "new", task.ToolSucceeded
			} else if result["isError"] != true || result["requestState"] != nil {
				t.Fatalf("expected denial without challenge: %+v", result)
			}
			wantEvents = append(wantEvents, "RunEnd")
			data, err := os.ReadFile(filepath.Join(w.root, "target"))
			if err != nil || string(data) != wantText {
				t.Fatalf("file=%q err=%v", data, err)
			}
			calls, err := db.ToolCalls(t.Context(), records[0].ID)
			if err != nil || len(calls) != 1 || calls[0].Status != wantStatus || calls[0].StartedAt.IsZero() != (wantStatus == task.ToolDeclined) {
				t.Fatalf("calls=%+v err=%v", calls, err)
			}
			data, err = os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			var gotEvents []string
			for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
				var p hookPayload
				if err := json.Unmarshal([]byte(line), &p); err != nil {
					t.Fatal(err)
				}
				if p.SessionID != records[0].ID || p.Meldra.TaskID != records[0].ID || p.Meldra.RunID != calls[0].RunID || p.HookEventName != "RunEnd" && p.ToolUseID != calls[0].ID {
					t.Fatalf("uncorrelated hook: %+v", p)
				}
				gotEvents = append(gotEvents, p.HookEventName)
			}
			if !slices.Equal(gotEvents, wantEvents) {
				t.Fatalf("hooks=%v want=%v", gotEvents, wantEvents)
			}
		})
	}
}
