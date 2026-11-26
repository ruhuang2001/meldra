package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"meldra/internal/provider"
	taskstore "meldra/internal/store"
	"meldra/internal/task"
	"meldra/internal/tool"
)

func writeHooksConfig(t *testing.T, paths ConfigPaths, raw string) {
	t.Helper()
	if err := ensureConfigHome(paths); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(paths.Home, hooksFileName)
	if err := os.WriteFile(target, []byte(raw), privateFilePerm); err != nil {
		t.Fatal(err)
	}
}

func mustLoadHooks(t *testing.T, paths ConfigPaths, raw string) *hookRunner {
	t.Helper()
	writeHooksConfig(t, paths, raw)
	runner, err := loadHooks(paths)
	if err != nil {
		t.Fatalf("loadHooks: %v", err)
	}
	return runner
}

func writeHookScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func hookTestResult(id, text string) provider.Result {
	return provider.Result{
		Response:     &provider.Response{ID: id, Status: "completed"},
		StreamedText: text,
	}
}

func TestRejectedPromptNeverBecomesRecoveredOrCompactedInput(t *testing.T) {
	const marker = "DENIED_PROMPT_TAIL"
	var requests int
	a, paths := recordedAgent(t, inferenceFunc(func(_ context.Context, r provider.Request, _ provider.Options, _ provider.Observer) (provider.Result, error) {
		requests++
		data, err := json.Marshal(r)
		if err != nil || bytes.Contains(data, []byte(marker)) || bytes.Contains(data, []byte("denied-prefix")) {
			t.Fatalf("provider received rejected input: %v", err)
		}
		return hookTestResult("accepted", "done"), nil
	}), nil)
	a.workspace = a.execution.workspace
	script := writeHookScript(t, t.TempDir(), "guard.sh", `if grep -q 'DENIED_PROMPT_TAIL'; then echo 'policy rejected' >&2; exit 2; fi`)
	a.hooks = mustLoadHooks(t, paths, `{"hooks":{"UserPromptSubmit":[{"hooks":[{"type":"command","command":"`+script+`"}]}]}}`)
	long := "denied-prefix" + strings.Repeat("界", maxSessionMessageBytes) + marker
	deny := func(prompt string) {
		t.Helper()
		if _, ok := errors.AsType[*hookBlockedError](a.RunTurn(t.Context(), prompt)); !ok {
			t.Fatal("expected hook denial")
		}
	}
	deny(long)
	db := openTaskDB(t, paths)
	record, err := db.GetTask(t.Context(), a.session.ID)
	if err != nil || strings.Contains(record.Goal, "denied-prefix") || requests != 0 {
		t.Fatalf("first denial: goal=%q requests=%d err=%v", record.Goal, requests, err)
	}
	const goal = "admitted task goal"
	if err := a.RunTurn(t.Context(), goal); err != nil {
		t.Fatal(err)
	}
	ctrl := a.initControl()
	ctrl.selected = &ControlRequest{ID: taskstore.NewID(), Kind: "steer", Text: marker}
	deny(marker)
	session, err := loadSessionForResume(paths, a.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	a.session, a.execution.session, a.execution.resume = session, session, true
	if err := a.RunTurn(t.Context(), "continue accepted work"); err != nil {
		t.Fatal(err)
	}
	record, err = db.GetTask(t.Context(), a.session.ID)
	if err != nil || record.Goal != goal || requests != 2 {
		t.Fatalf("goal=%q requests=%d err=%v", record.Goal, requests, err)
	}
	if err := a.execution.startRun(t.Context(), ""); err != nil {
		t.Fatal(err)
	}
	defer a.execution.finish(t.Context(), nil)
	history, err := a.compactionHistory(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(history.Requests) != 2 || history.Requests[0].Text != goal || history.Requests[1].Text != "continue accepted work" {
		t.Fatalf("unexpected compactable history: %+v", history.Requests)
	}
}

func TestHooksConfigValidationAndCLI(t *testing.T) {
	paths := mustConfigPaths(t)
	t.Setenv(MeldraHomeEnv, paths.Home)

	if runner, err := loadHooks(paths); err != nil || runner != nil {
		t.Fatalf("missing hooks.json: got (%v, %v), want (nil, nil)", runner, err)
	}

	var out bytes.Buffer
	if err := runHooksCommand(t.Context(), nil, &out); err != nil {
		t.Fatalf("runHooksCommand missing: %v", err)
	}
	if !strings.Contains(out.String(), "No hooks configured") {
		t.Fatalf("unexpected empty listing: %q", out.String())
	}

	if err := ensureConfigHome(paths); err != nil {
		t.Fatal(err)
	}
	loosePath := filepath.Join(paths.Home, hooksFileName)
	if err := os.WriteFile(loosePath, []byte(`{"hooks":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadHooks(paths); err == nil {
		t.Fatal("expected loose permissions on hooks.json to fail")
	}
	_ = os.Remove(loosePath)

	target := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(target, []byte(`{"hooks":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, loosePath); err != nil {
		t.Fatal(err)
	}
	if _, err := loadHooks(paths); err == nil {
		t.Fatal("expected symlink hooks.json to fail")
	}
	_ = os.Remove(loosePath)

	invalidCases := []struct {
		name string
		raw  string
		want string
	}{
		{"unknown-event", `{"hooks":{"PreModelRequest":[{"hooks":[{"type":"command","command":"true"}]}]}}`, "unsupported hook event"},
		{"unsupported-http", `{"hooks":{"PreToolUse":[{"hooks":[{"type":"http","command":"http://127.0.0.1"}]}]}}`, "unsupported hook handler type"},
		{"unsupported-mcp", `{"hooks":{"SessionStart":[{"hooks":[{"type":"mcp_tool","command":"srv/tool"}]}]}}`, "unsupported hook handler type"},
		{"unsupported-async", `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"true","async":true}]}]}}`, "async hooks are not supported"},
		{"unsupported-if", `{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"true","if":"Bash(git *)"}]}]}}`, `hook "if" filter is not supported`},
		{"matcher-on-user-prompt", `{"hooks":{"UserPromptSubmit":[{"matcher":"foo","hooks":[{"type":"command","command":"true"}]}]}}`, "does not support matcher"},
		{"matcher-on-stop", `{"hooks":{"Stop":[{"matcher":"foo","hooks":[{"type":"command","command":"true"}]}]}}`, "does not support matcher"},
		{"invalid-regex", `{"hooks":{"PreToolUse":[{"matcher":"[unclosed","hooks":[{"type":"command","command":"true"}]}]}}`, "matcher"},
		{"block-on-runend", `{"hooks":{"RunEnd":[{"hooks":[{"type":"command","command":"true","onError":"block"}]}]}}`, `onError "block"`},
		{"block-on-stop", `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"true","onError":"block"}]}]}}`, `onError "block"`},
		{"timeout-too-high-runend", `{"hooks":{"RunEnd":[{"hooks":[{"type":"command","command":"true","timeout":31}]}]}}`, "timeout must be between"},
		{"unknown-top-field", `{"extra":1,"hooks":{}}`, "invalid hooks.json"},
	}
	for _, tc := range invalidCases {
		t.Run(tc.name, func(t *testing.T) {
			writeHooksConfig(t, paths, tc.raw)
			_, err := loadHooks(paths)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("loadHooks(%s) = %v, want substring %q", tc.name, err, tc.want)
			}
		})
	}

	valid := `{
		"description": "test hooks",
		"hooks": {
			"SessionStart": [{"matcher": "startup|resume", "hooks": [{"type": "command", "command": "true", "timeout": 5}]}],
			"UserPromptSubmit": [{"matcher": "*", "hooks": [{"type": "command", "command": "true", "onError": "block"}]}],
			"RunEnd": [{"hooks": [{"type": "command", "command": "true"}]}]
		}
	}`
	writeHooksConfig(t, paths, valid)
	out.Reset()
	if err := runHooksCommand(t.Context(), []string{"--json"}, &out); err != nil {
		t.Fatalf("runHooksCommand --json: %v", err)
	}
	var summary struct {
		Enabled  bool `json:"enabled"`
		Handlers []struct {
			Event   string `json:"event"`
			OnError string `json:"on_error"`
		} `json:"handlers"`
	}
	if err := json.Unmarshal(out.Bytes(), &summary); err != nil {
		t.Fatalf("decode hooks json: %v (%s)", err, out.String())
	}
	if !summary.Enabled || len(summary.Handlers) != 3 {
		t.Fatalf("unexpected summary: %+v", summary)
	}
	out.Reset()
	if err := runHooksCommand(t.Context(), nil, &out); err != nil {
		t.Fatalf("runHooksCommand text: %v", err)
	}
	if !strings.Contains(out.String(), "SessionStart") || !strings.Contains(out.String(), "RunEnd") {
		t.Fatalf("unexpected text output: %q", out.String())
	}
}

func TestHooksOutputRejectionAndEnvironmentIsolation(t *testing.T) {
	paths := mustConfigPaths(t)
	scriptDir := t.TempDir()
	wsDir := t.TempDir()
	t.Setenv("OPENAI_API_KEY", "sk-secret-do-not-leak")
	t.Setenv("DEEPSEEK_API_KEY", "ds-secret-do-not-leak")
	t.Setenv("ALLOWED_HOOK_VAR", "visible-value")

	envDump := filepath.Join(scriptDir, "env.txt")
	envScript := writeHookScript(t, scriptDir, "check-env.sh", `env > "`+envDump+`"`)
	runner := mustLoadHooks(t, paths, `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"`+envScript+`","env_vars":["ALLOWED_HOOK_VAR"],"env":{"CUSTOM_FLAG":"1"}}]}]}}`)
	runner.dispatch(t.Context(), hookPayload{HookEventName: hookEventSessionStart, Cwd: wsDir, PermissionMode: "default", Source: "startup"}, hookDispatchOptions{matchTarget: "startup"})
	envData, err := os.ReadFile(envDump)
	if err != nil {
		t.Fatalf("read env dump: %v", err)
	}
	envText := string(envData)
	if strings.Contains(envText, "sk-secret-do-not-leak") || strings.Contains(envText, "ds-secret-do-not-leak") {
		t.Fatalf("provider secrets leaked into hook environment: %s", envText)
	}
	if !strings.Contains(envText, "MELDRA_HOME="+paths.Home) || !strings.Contains(envText, "ALLOWED_HOOK_VAR=visible-value") || !strings.Contains(envText, "CUSTOM_FLAG=1") {
		t.Fatalf("expected MELDRA_HOME and explicit env vars, got: %s", envText)
	}

	rejectionCases := []struct {
		name     string
		event    string
		stdout   string
		wantBlk  bool
		wantText string
	}{
		{"pretool-updated-input", hookEventPreToolUse, `{"hookSpecificOutput":{"hookEventName":"PreToolUse","updatedInput":{"path":"other.txt"}}}`, true, "updatedInput"},
		{"pretool-allow-decision", hookEventPreToolUse, `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow"}}`, true, "permissionDecision"},
		{"pretool-approve-decision", hookEventPreToolUse, `{"decision":"approve"}`, true, "unsupported decision"},
		{"pretool-plain-text", hookEventPreToolUse, `plain text not allowed`, true, "plain text stdout is not supported"},
		{"perm-allow-decision", hookEventPermissionRequest, `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow"}}}`, true, "cannot grant approval"},
		{"posttool-updated-output", hookEventPostToolUse, `{"hookSpecificOutput":{"hookEventName":"PostToolUse","updatedToolOutput":"hacked"}}`, false, "updatedToolOutput"},
		{"runend-block-decision", hookEventRunEnd, `{"decision":"block","reason":"keep going"}`, false, "not supported on RunEnd"},
		{"stop-empty-reason", hookEventStop, `{"decision":"block"}`, false, "requires a non-empty reason"},
		{"event-name-mismatch", hookEventPreToolUse, `{"hookSpecificOutput":{"hookEventName":"PostToolUse","additionalContext":"x"}}`, true, "does not match event"},
	}
	for _, tc := range rejectionCases {
		t.Run(tc.name, func(t *testing.T) {
			script := writeHookScript(t, scriptDir, tc.name+".sh", `printf '%s' '`+tc.stdout+`'`)
			r := mustLoadHooks(t, paths, `{"hooks":{"`+tc.event+`":[{"hooks":[{"type":"command","command":"`+script+`"}]}]}}`)
			var notices []string
			outcome := r.dispatch(t.Context(), hookPayload{HookEventName: tc.event, Cwd: wsDir, PermissionMode: "default", ToolName: "edit_file"}, hookDispatchOptions{
				matchTarget: "edit_file",
				onNotice:    func(s string) { notices = append(notices, s) },
			})
			if outcome.blocked != tc.wantBlk {
				t.Fatalf("blocked = %v, want %v (notices=%v)", outcome.blocked, tc.wantBlk, notices)
			}
			if !strings.Contains(strings.Join(notices, "\n"), tc.wantText) {
				t.Fatalf("notices = %v, want substring %q", notices, tc.wantText)
			}
		})
	}
}

func TestHooksSessionUserPromptAndOptionalTimeout(t *testing.T) {
	paths := mustConfigPaths(t)
	scriptDir := t.TempDir()
	ws, err := NewWorkspace(t.TempDir(), bufio.NewReader(strings.NewReader("")), io.Discard, false)
	if err != nil {
		t.Fatal(err)
	}

	logFile := filepath.Join(scriptDir, "events.jsonl")
	startScript := writeHookScript(t, scriptDir, "start.sh", `cat >> "`+logFile+`"; printf '\n' >> "`+logFile+`"; printf '{"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":"MEMORY_FROM_SESSION_START"}}'`)
	slowOptional := writeHookScript(t, scriptDir, "slow.sh", `sleep 5`)
	promptScript := writeHookScript(t, scriptDir, "prompt.sh", `input=$(cat); printf '%s\n' "$input" >> "`+logFile+`"; if printf '%s' "$input" | grep -q "FORBIDDEN_SECRET"; then printf '{"decision":"block","reason":"secret detected"}'; else printf '{"hookSpecificOutput":{"hookEventName":"UserPromptSubmit","additionalContext":"CONTEXT_FOR_TURN"}}'; fi`)
	endScript := writeHookScript(t, scriptDir, "end.sh", `cat >> "`+logFile+`"; printf '\n' >> "`+logFile+`"`)

	runner := mustLoadHooks(t, paths, `{
		"hooks": {
			"SessionStart": [
				{"matcher": "resume", "hooks": [{"type": "command", "command": "`+startScript+`"}, {"type": "command", "command": "`+slowOptional+`", "timeout": 1, "onError": "ignore"}]}
			],
			"UserPromptSubmit": [
				{"hooks": [{"type": "command", "command": "`+promptScript+`"}]}
			],
			"SessionEnd": [
				{"hooks": [{"type": "command", "command": "`+endScript+`"}]}
			]
		}
	}`)

	var seenInstructions []string
	var prompts = []string{"FORBIDDEN_SECRET leak", "allowed task"}
	idx := 0
	backend := inferenceFunc(func(_ context.Context, req provider.Request, _ provider.Options, _ provider.Observer) (provider.Result, error) {
		seenInstructions = append(seenInstructions, req.Instructions)
		return hookTestResult("resp_1", "done"), nil
	})
	a := NewAgent(backend, func() (string, bool) {
		if idx >= len(prompts) {
			return "", false
		}
		p := prompts[idx]
		idx++
		return p, true
	}, nil)
	a.output = io.Discard
	a.workspace = ws
	a.hooks = runner
	a.sessionStartSource = "resume"
	var notices []string
	a.events = UIEventSinkFunc(func(ev UIEvent) {
		if ev.Kind == UIEventNotice {
			notices = append(notices, ev.Text)
		}
	})

	if err := a.Run(t.Context()); err != nil {
		t.Fatalf("agent.Run: %v", err)
	}
	if len(seenInstructions) != 1 {
		t.Fatalf("expected 1 model request (first prompt blocked, second allowed), got %d", len(seenInstructions))
	}
	if !strings.Contains(seenInstructions[0], "MEMORY_FROM_SESSION_START") || !strings.Contains(seenInstructions[0], "CONTEXT_FOR_TURN") {
		t.Fatalf("hook context missing from first model request: %s", seenInstructions[0])
	}
	joinedNotices := strings.Join(notices, "\n")
	if !strings.Contains(joinedNotices, "timed out") || !strings.Contains(joinedNotices, "secret detected") {
		t.Fatalf("expected timeout notice and block notice, got: %v", notices)
	}
	rawEvents, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rawEvents), `"hook_event_name":"SessionStart"`) || !strings.Contains(string(rawEvents), `"hook_event_name":"SessionEnd"`) {
		t.Fatalf("missing SessionStart/SessionEnd in log: %s", string(rawEvents))
	}
}

func TestHooksPreToolUsePermissionRequestAndPlanBoundary(t *testing.T) {
	paths := mustConfigPaths(t)
	scriptDir := t.TempDir()
	wsDir := t.TempDir()
	targetFile := filepath.Join(wsDir, "hello.txt")
	if err := os.WriteFile(targetFile, []byte("original\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ws, err := NewWorkspace(wsDir, bufio.NewReader(strings.NewReader("")), io.Discard, false)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := newRuntimePolicy(ModeBuild, PermissionInteractive)
	if err != nil {
		t.Fatal(err)
	}
	ws.policy = policy
	approvalCalled := 0
	ws.SetApprovalFunc(func(context.Context, ApprovalRequest) bool {
		approvalCalled++
		return true
	})

	postLog := filepath.Join(scriptDir, "post.jsonl")
	denyPre := writeHookScript(t, scriptDir, "deny-pre.sh", `printf '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"blocked edit"}}'`)
	timeoutPre := writeHookScript(t, scriptDir, "timeout-pre.sh", `sleep 5`)
	postScript := writeHookScript(t, scriptDir, "post.sh", `cat >> "`+postLog+`"; printf '\n' >> "`+postLog+`"`)

	store := newTaskSessionStore(paths)
	session, err := store.New(ws.root)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tool.New(ws.ToolDefinitions())
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name   string
		config string
	}{
		{"denied", `{"hooks":{"PreToolUse":[{"matcher":"edit_file","hooks":[{"type":"command","command":"` + denyPre + `"}]}],"PostToolUse":[{"hooks":[{"type":"command","command":"` + postScript + `"}]}],"PostToolUseFailure":[{"hooks":[{"type":"command","command":"` + postScript + `"}]}]}}`},
		{"mandatory-timeout", `{"hooks":{"PreToolUse":[{"matcher":"edit_file","hooks":[{"type":"command","command":"` + timeoutPre + `","timeout":1,"onError":"block"}]}],"PostToolUse":[{"hooks":[{"type":"command","command":"` + postScript + `"}]}],"PostToolUseFailure":[{"hooks":[{"type":"command","command":"` + postScript + `"}]}]}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := mustLoadHooks(t, paths, tc.config)
			exec := &taskExecution{paths: paths, workspace: ws, session: session, config: task.Config{Provider: "test", Workspace: ws.root}, hooks: runner}
			ws.hooks = runner
			ws.approvalRecord = exec.approval
			ws.approvalPending = exec.pendingApproval
			if err := exec.begin(t.Context(), "edit test"); err != nil {
				t.Fatal(err)
			}
			callID := "call_" + tc.name
			res, callErr := exec.invoke(t.Context(), registry, callID, "edit_file", json.RawMessage(`{"path":"hello.txt","old_str":"original","new_str":"mutated"}`))
			if finishErr := exec.finish(t.Context(), callErr); finishErr != nil {
				t.Fatal(finishErr)
			}
			if res.Status != tool.Declined {
				t.Fatalf("status = %q, want declined (output=%q, err=%v)", res.Status, res.Output, callErr)
			}
			data, _ := os.ReadFile(targetFile)
			if string(data) != "original\n" {
				t.Fatalf("file was modified despite PreToolUse block: %q", string(data))
			}
			if approvalCalled != 0 {
				t.Fatalf("approval prompt should not run when PreToolUse blocks")
			}
			db, err := taskStoreForRead(paths)
			if err != nil {
				t.Fatal(err)
			}
			calls, err := db.ToolCalls(t.Context(), session.ID)
			db.Close()
			if err != nil {
				t.Fatal(err)
			}
			last := calls[len(calls)-1]
			if last.Status != task.ToolDeclined || !last.StartedAt.IsZero() {
				t.Fatalf("expected never-dispatched declined tool call, got status=%s started=%v", last.Status, last.StartedAt)
			}
		})
	}
	if _, err := os.Stat(postLog); !os.IsNotExist(err) {
		t.Fatalf("PostToolUse/PostToolUseFailure should not fire on PreToolUse denial")
	}

	denyPerm := writeHookScript(t, scriptDir, "deny-perm.sh", `printf '{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"deny","message":"no writes allowed"}}}'`)
	permRunner := mustLoadHooks(t, paths, `{"hooks":{"PermissionRequest":[{"matcher":"edit_file","hooks":[{"type":"command","command":"`+denyPerm+`"}]}],"PostToolUse":[{"hooks":[{"type":"command","command":"`+postScript+`"}]}],"PostToolUseFailure":[{"hooks":[{"type":"command","command":"`+postScript+`"}]}]}}`)
	exec := &taskExecution{paths: paths, workspace: ws, session: session, config: task.Config{Provider: "test", Workspace: ws.root}, hooks: permRunner}
	ws.hooks = permRunner
	ws.approvalRecord = exec.approval
	ws.approvalPending = exec.pendingApproval
	if err := exec.begin(t.Context(), "perm deny test"); err != nil {
		t.Fatal(err)
	}
	res, callErr := exec.invoke(t.Context(), registry, "call_perm_deny", "edit_file", json.RawMessage(`{"path":"hello.txt","old_str":"original","new_str":"mutated"}`))
	if finishErr := exec.finish(t.Context(), nil); finishErr != nil {
		t.Fatal(finishErr)
	}
	if res.Status != tool.Declined || approvalCalled != 0 {
		t.Fatalf("PermissionRequest denial failed: status=%q err=%v approvals=%d", res.Status, callErr, approvalCalled)
	}
	data, _ := os.ReadFile(targetFile)
	if string(data) != "original\n" {
		t.Fatalf("file modified despite PermissionRequest denial: %q", string(data))
	}
	if _, err := os.Stat(postLog); !os.IsNotExist(err) {
		t.Fatalf("PostToolUse/PostToolUseFailure should not fire on PermissionRequest denial")
	}

	passPre := writeHookScript(t, scriptDir, "pass-pre.sh", `printf '{}'`)
	passRunner := mustLoadHooks(t, paths, `{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"`+passPre+`"}]}]}}`)
	planPolicy, err := newRuntimePolicy(ModePlan, PermissionInteractive)
	if err != nil {
		t.Fatal(err)
	}
	ws.policy = planPolicy
	ws.hooks = passRunner
	backend := inferenceFunc(func(_ context.Context, _ provider.Request, _ provider.Options, _ provider.Observer) (provider.Result, error) {
		return hookTestResult("resp_plan", "done"), nil
	})
	a := NewAgent(backend, nil, ws.ToolDefinitions())
	a.policy = planPolicy
	a.workspace = ws
	a.hooks = passRunner
	a.output = io.Discard
	if _, policyErr := a.policy.invocation(t.Context(), "edit_file", json.RawMessage(`{"path":"hello.txt","old_str":"original","new_str":"mutated"}`)); policyErr == nil {
		t.Fatal("Plan mode must reject edit_file even with a passing PreToolUse hook")
	}
}

func TestHooksStopContinuationBoundAndCancellation(t *testing.T) {
	paths := mustConfigPaths(t)
	scriptDir := t.TempDir()
	ws, err := NewWorkspace(t.TempDir(), bufio.NewReader(strings.NewReader("")), io.Discard, false)
	if err != nil {
		t.Fatal(err)
	}

	seenActive := filepath.Join(scriptDir, "stop.jsonl")
	alwaysBlock := writeHookScript(t, scriptDir, "stop-block.sh", `cat >> "`+seenActive+`"; printf '\n' >> "`+seenActive+`"; printf '{"decision":"block","reason":"run verification"}'`)
	runner := mustLoadHooks(t, paths, `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"`+alwaysBlock+`"}]}]}}`)

	calls := 0
	backend := inferenceFunc(func(_ context.Context, _ provider.Request, _ provider.Options, _ provider.Observer) (provider.Result, error) {
		calls++
		return hookTestResult("resp_"+string(rune('0'+calls)), "reply"), nil
	})
	a := NewAgent(backend, nil, nil)
	a.output = io.Discard
	a.workspace = ws
	a.hooks = runner

	if err := a.RunTurn(t.Context(), "check this"); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected exactly 2 model calls (initial + 1 bounded Stop continuation), got %d", calls)
	}
	raw, err := os.ReadFile(seenActive)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"stop_hook_active":false`) || !strings.Contains(lines[1], `"stop_hook_active":true`) {
		t.Fatalf("unexpected Stop payloads: %q", string(raw))
	}

	stopPrecedence := writeHookScript(t, scriptDir, "stop-false.sh", `printf '{"continue":false,"stopReason":"done anyway"}'`)
	runnerPrecedence := mustLoadHooks(t, paths, `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"`+alwaysBlock+`"},{"type":"command","command":"`+stopPrecedence+`"}]}]}}`)
	calls = 0
	a.hooks = runnerPrecedence
	if err := a.RunTurn(t.Context(), "check precedence"); err != nil {
		t.Fatalf("RunTurn precedence: %v", err)
	}
	if calls != 1 {
		t.Fatalf("continue:false must override decision:block, got %d calls", calls)
	}

	slowStop := writeHookScript(t, scriptDir, "slow-stop.sh", `sleep 5; printf '{"decision":"block","reason":"too late"}'`)
	runnerCancel := mustLoadHooks(t, paths, `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"`+slowStop+`"}]}]}}`)
	calls = 0
	a.hooks = runnerCancel
	turnCtx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	err = a.RunTurn(turnCtx, "cancel during stop")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled when user cancels during Stop hook, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("cancelled Stop hook must not trigger continuation, got %d calls", calls)
	}
}

func TestHooksRunEndObserverReadsCommittedState(t *testing.T) {
	paths := mustConfigPaths(t)
	scriptDir := t.TempDir()
	ws, err := NewWorkspace(t.TempDir(), bufio.NewReader(strings.NewReader("")), io.Discard, false)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := newRuntimePolicy(ModeBuild, PermissionInteractive)
	if err != nil {
		t.Fatal(err)
	}
	ws.policy = policy

	runEndFile := filepath.Join(scriptDir, "runend.jsonl")
	observer := writeHookScript(t, scriptDir, "runend.sh", `cat >> "`+runEndFile+`"; printf '\n' >> "`+runEndFile+`"; printf '{"decision":"block","reason":"cannot override"}'`)
	runner := mustLoadHooks(t, paths, `{"hooks":{"RunEnd":[{"hooks":[{"type":"command","command":"`+observer+`"}]}]}}`)

	store := newTaskSessionStore(paths)
	session, err := store.New(ws.root)
	if err != nil {
		t.Fatal(err)
	}

	exec := &taskExecution{paths: paths, workspace: ws, session: session, config: task.Config{Provider: "test", Workspace: ws.root}, hooks: runner, noninteractive: true}
	ws.approvalRecord = exec.approval
	ws.approvalPending = exec.pendingApproval
	if err := exec.begin(t.Context(), "waiting approval test"); err != nil {
		t.Fatal(err)
	}
	registry, err := tool.New(ws.ToolDefinitions())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws.root, "f.txt"), []byte("a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, invokeErr := exec.invoke(t.Context(), registry, "call_wait", "edit_file", json.RawMessage(`{"path":"f.txt","old_str":"a","new_str":"b"}`))
	if !errors.Is(invokeErr, errApprovalRequired) {
		t.Fatalf("expected errApprovalRequired, got %v", invokeErr)
	}
	if err := exec.finish(t.Context(), invokeErr); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(runEndFile)
	if err != nil {
		t.Fatal(err)
	}
	var payload hookPayload
	if err := json.Unmarshal(bytes.TrimSpace(raw), &payload); err != nil {
		t.Fatalf("unmarshal RunEnd payload: %v (%s)", err, string(raw))
	}
	if payload.Meldra.RunStatus != string(task.RunWaitingApproval) || payload.Meldra.TaskStatus != string(task.WaitingApproval) {
		t.Fatalf("expected waiting_approval RunEnd, got run=%q task=%q", payload.Meldra.RunStatus, payload.Meldra.TaskStatus)
	}

	db, err := taskStoreForRead(paths)
	if err != nil {
		t.Fatal(err)
	}
	recordedRun, err := db.GetRun(t.Context(), payload.Meldra.RunID)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	if recordedRun.Status != task.RunWaitingApproval {
		t.Fatalf("RunEnd observer mutated recorded status: %s", recordedRun.Status)
	}

	unknownSession, err := store.New(ws.root)
	if err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(runEndFile)
	unknownExec := &taskExecution{paths: paths, workspace: ws, session: unknownSession, config: task.Config{Provider: "test", Workspace: ws.root}, hooks: runner}
	if err := unknownExec.begin(t.Context(), "unknown outcome test"); err != nil {
		t.Fatal(err)
	}
	var unknownOnce sync.Once
	unknownReg, err := tool.New([]ToolDefinition{{
		Name: "flaky_tool",
		Function: func(ctx context.Context, _ json.RawMessage) (string, error) {
			unknownOnce.Do(func() {
				tool.Observe(ctx, func(o *tool.Observation) { o.Started = true })
			})
			return "", errors.New("connection lost after side effect")
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	_, invokeErr = unknownExec.invoke(t.Context(), unknownReg, "call_unknown", "flaky_tool", json.RawMessage(`{}`))
	if finishErr := unknownExec.finish(t.Context(), invokeErr); finishErr != nil {
		t.Fatal(finishErr)
	}
	raw, err = os.ReadFile(runEndFile)
	if err != nil {
		t.Fatal(err)
	}
	var unknownPayload hookPayload
	if err := json.Unmarshal(bytes.TrimSpace(raw), &unknownPayload); err != nil {
		t.Fatalf("unmarshal unknown RunEnd payload: %v", err)
	}
	if unknownPayload.Meldra.RunStatus != string(task.RunInterrupted) || unknownPayload.Meldra.UnknownToolCalls == nil || *unknownPayload.Meldra.UnknownToolCalls != 1 {
		t.Fatalf("expected interrupted with unknown_tool_calls=1, got %+v", unknownPayload.Meldra)
	}
}

func TestHooksCompactionBlockAndAllow(t *testing.T) {
	paths := mustConfigPaths(t)
	scriptDir := t.TempDir()
	ws, err := NewWorkspace(t.TempDir(), bufio.NewReader(strings.NewReader("")), io.Discard, false)
	if err != nil {
		t.Fatal(err)
	}
	store := newTaskSessionStore(paths)
	session, err := store.New(ws.root)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 6 {
		session.appendMessage("user", strings.Repeat("u", 60)+string(rune('0'+i)))
		session.appendMessage("assistant", strings.Repeat("a", 60)+string(rune('0'+i)))
	}
	if err := store.Save(session); err != nil {
		t.Fatal(err)
	}
	db, err := taskstore.Open(taskDirectory(paths))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.EnsureTask(t.Context(), task.Task{ID: session.ID, SessionID: session.ID, Goal: "compact test", Workspace: ws.root}); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	postLog := filepath.Join(scriptDir, "compact.jsonl")
	blockPre := writeHookScript(t, scriptDir, "block-compact.sh", `printf '{"decision":"block","reason":"keep full transcript"}'`)
	allowPre := writeHookScript(t, scriptDir, "allow-compact.sh", `cat >> "`+postLog+`"; printf '\n' >> "`+postLog+`"`)
	postCompact := writeHookScript(t, scriptDir, "post-compact.sh", `cat >> "`+postLog+`"; printf '\n' >> "`+postLog+`"`)

	inferCalls := 0
	backend := inferenceFunc(func(_ context.Context, _ provider.Request, _ provider.Options, _ provider.Observer) (provider.Result, error) {
		inferCalls++
		return hookTestResult("resp_compact", "Concise summary."), nil
	})
	a := NewAgent(backend, nil, nil)
	a.output = io.Discard
	a.workspace = ws
	a.session = session
	a.store = store
	a.compactKeepMessages = 2
	a.execution = &taskExecution{paths: paths, workspace: ws, session: session, config: task.Config{Provider: "test", Workspace: ws.root}}
	a.hooks = mustLoadHooks(t, paths, `{"hooks":{"PreCompact":[{"matcher":"manual","hooks":[{"type":"command","command":"`+blockPre+`"}]}],"PostCompact":[{"hooks":[{"type":"command","command":"`+postCompact+`"}]}]}}`)

	if err := a.handleCompactCommand(t.Context(), "keep details"); err != nil {
		t.Fatalf("blocked handleCompactCommand: %v", err)
	}
	if inferCalls != 0 || a.session.ContextCompaction != nil {
		t.Fatalf("PreCompact block did not prevent compaction: inferCalls=%d compaction=%v", inferCalls, a.session.ContextCompaction)
	}

	a.hooks = mustLoadHooks(t, paths, `{"hooks":{"PreCompact":[{"matcher":"manual","hooks":[{"type":"command","command":"`+allowPre+`"}]}],"PostCompact":[{"matcher":"manual","hooks":[{"type":"command","command":"`+postCompact+`"}]}]}}`)
	if err := a.handleCompactCommand(t.Context(), "focus tests"); err != nil {
		t.Fatalf("allowed handleCompactCommand: %v", err)
	}
	if inferCalls != 1 || a.session.ContextCompaction == nil {
		t.Fatalf("expected compaction to succeed: inferCalls=%d compaction=%v", inferCalls, a.session.ContextCompaction)
	}
	raw, err := os.ReadFile(postLog)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, `"hook_event_name":"PreCompact"`) || !strings.Contains(text, `"custom_instructions":"focus tests"`) || !strings.Contains(text, `"hook_event_name":"PostCompact"`) || !strings.Contains(text, `"compact_summary":"Concise summary."`) {
		t.Fatalf("unexpected compaction hook payloads: %s", text)
	}
}

func TestHooksPostToolSuccessFailureReplayAndMCPServe(t *testing.T) {
	paths := mustConfigPaths(t)
	t.Setenv(MeldraHomeEnv, paths.Home)
	scriptDir := t.TempDir()
	wsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsDir, "note.txt"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ws, err := NewWorkspace(wsDir, bufio.NewReader(strings.NewReader("")), io.Discard, true)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := newRuntimePolicy(ModeBuild, PermissionInteractive)
	if err != nil {
		t.Fatal(err)
	}
	ws.policy = policy

	logPath := filepath.Join(scriptDir, "tool-hooks.jsonl")
	preScript := writeHookScript(t, scriptDir, "pre.sh", `cat >> "`+logPath+`"; printf '\n' >> "`+logPath+`"; printf '{"hookSpecificOutput":{"hookEventName":"PreToolUse","additionalContext":"PRE_CTX"}}'`)
	postOK := writeHookScript(t, scriptDir, "post-ok.sh", `cat >> "`+logPath+`"; printf '\n' >> "`+logPath+`"; printf '{"hookSpecificOutput":{"hookEventName":"PostToolUse","additionalContext":"POST_OK_CTX"}}'`)
	postFail := writeHookScript(t, scriptDir, "post-fail.sh", `cat >> "`+logPath+`"; printf '\n' >> "`+logPath+`"; printf '{"hookSpecificOutput":{"hookEventName":"PostToolUseFailure","additionalContext":"POST_FAIL_CTX"}}'`)

	runner := mustLoadHooks(t, paths, `{
		"hooks": {
			"PreToolUse": [{"hooks": [{"type": "command", "command": "`+preScript+`"}]}],
			"PostToolUse": [{"hooks": [{"type": "command", "command": "`+postOK+`"}]}],
			"PostToolUseFailure": [{"hooks": [{"type": "command", "command": "`+postFail+`"}]}]
		}
	}`)

	store := newTaskSessionStore(paths)
	session, err := store.New(ws.root)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tool.New(ws.ToolDefinitions())
	if err != nil {
		t.Fatal(err)
	}
	exec := &taskExecution{paths: paths, workspace: ws, session: session, config: task.Config{Provider: "test", Workspace: ws.root}, hooks: runner, replayScope: "scope-1"}
	if err := exec.begin(t.Context(), "post tool test"); err != nil {
		t.Fatal(err)
	}
	res, err := exec.invoke(t.Context(), registry, "same_call", "read_file", json.RawMessage(`{"path":"note.txt","offset":null,"limit":null}`))
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}
	if !strings.Contains(res.Output, "PRE_CTX") || !strings.Contains(res.Output, "POST_OK_CTX") {
		t.Fatalf("expected hook context appended to model-visible tool output: %q", res.Output)
	}

	replayed, err := exec.invoke(t.Context(), registry, "same_call", "read_file", json.RawMessage(`{"path":"note.txt","offset":null,"limit":null}`))
	if err != nil {
		t.Fatalf("replayed read_file: %v", err)
	}
	if strings.Contains(replayed.Output, "PRE_CTX") || strings.Contains(replayed.Output, "POST_OK_CTX") {
		t.Fatalf("durable stored output must not be mutated by hook context: %q", replayed.Output)
	}

	failRes, _ := exec.invoke(t.Context(), registry, "fail_call", "read_file", json.RawMessage(`{"path":"missing.txt","offset":null,"limit":null}`))
	if failRes.Status != tool.Failed || !strings.Contains(failRes.Output, "POST_FAIL_CTX") {
		t.Fatalf("expected PostToolUseFailure context on known failure, got status=%q output=%q", failRes.Status, failRes.Output)
	}
	if err := exec.finish(t.Context(), nil); err != nil {
		t.Fatal(err)
	}

	rawLog, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(rawLog)), "\n")
	if len(lines) != 4 {
		t.Fatalf("expected 4 hook events (Pre+Post for read_file, none for replay, Pre+PostFailure for missing.txt), got %d:\n%s", len(lines), string(rawLog))
	}

	mcpLog := filepath.Join(scriptDir, "mcp-hooks.jsonl")
	mcpRecord := writeHookScript(t, scriptDir, "mcp-rec.sh", `cat >> "`+mcpLog+`"; printf '\n' >> "`+mcpLog+`"; printf 'noisy stderr\n' >&2`)
	mcpDenyEdit := writeHookScript(t, scriptDir, "mcp-deny.sh", `printf '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"mcp blocked"}}'`)
	writeHooksConfig(t, paths, `{
		"hooks": {
			"SessionStart": [{"hooks": [{"type": "command", "command": "`+mcpRecord+`"}]}],
			"SessionEnd": [{"hooks": [{"type": "command", "command": "`+mcpRecord+`"}]}],
			"UserPromptSubmit": [{"hooks": [{"type": "command", "command": "`+mcpRecord+`"}]}],
			"Stop": [{"hooks": [{"type": "command", "command": "`+mcpRecord+`"}]}],
			"PreToolUse": [
				{"matcher": "edit_file", "hooks": [{"type": "command", "command": "`+mcpDenyEdit+`"}]},
				{"matcher": "read_file", "hooks": [{"type": "command", "command": "`+mcpRecord+`"}]}
			],
			"PostToolUse": [{"hooks": [{"type": "command", "command": "`+mcpRecord+`"}]}],
			"RunEnd": [{"hooks": [{"type": "command", "command": "`+mcpRecord+`"}]}]
		}
	}`)
	writeContextFixture(t, ws, "AGENTS.md", "MCP hook test.")
	client := newPermissionReviewMCP(t, ws.root, true, PermissionInteractive)
	if res := client.call("read_project_context", map[string]any{"path": nil}); res["isError"] == true {
		t.Fatalf("mcp read_project_context: %+v", res)
	}
	if res := client.call("read_file", map[string]any{"path": "note.txt", "offset": nil, "limit": nil}); res["isError"] == true {
		t.Fatalf("mcp read_file failed: %+v", res)
	}
	editRes := client.call("edit_file", map[string]any{"path": "note.txt", "old_str": "hello", "new_str": "changed"})
	if editRes["isError"] != true {
		t.Fatalf("expected MCP PreToolUse denial to fail tool call: %+v", editRes)
	}
	noteBytes, _ := os.ReadFile(filepath.Join(ws.root, "note.txt"))
	if string(noteBytes) != "hello\n" {
		t.Fatalf("MCP edit modified file despite PreToolUse denial: %q", string(noteBytes))
	}
	mcpEvents, err := os.ReadFile(mcpLog)
	if err != nil {
		t.Fatal(err)
	}
	mcpText := string(mcpEvents)
	for _, forbidden := range []string{`"SessionStart"`, `"SessionEnd"`, `"UserPromptSubmit"`, `"Stop"`} {
		if strings.Contains(mcpText, forbidden) {
			t.Fatalf("MCP serve must not emit synthetic chat event %s:\n%s", forbidden, mcpText)
		}
	}
	if !strings.Contains(mcpText, `"surface":"mcp"`) || !strings.Contains(mcpText, `"PreToolUse"`) || !strings.Contains(mcpText, `"PostToolUse"`) || !strings.Contains(mcpText, `"RunEnd"`) {
		t.Fatalf("expected MCP PreToolUse, PostToolUse, RunEnd with surface=mcp:\n%s", mcpText)
	}
}
