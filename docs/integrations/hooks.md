---
title: "Lifecycle hooks"
description: "Run local command hooks at session, prompt, tool, compaction, and run boundaries to connect external memory, policy checks, and observers."
---

<Note>
**Unreleased:** Lifecycle hooks are not in the latest published release.
</Note>

Lifecycle hooks run local commands that you configure in your private
`$MELDRA_HOME/hooks.json` (default `~/.meldra/hooks.json`). Use them to load
context from an external memory service, enforce custom pre-tool checks, run a
final verification pass before a reply ends, or observe recorded run outcomes.

Hooks are opt-in and off by default. Meldra never reads `.meldra/hooks.json`,
`.codex/hooks.json`, `.claude/settings.json`, or any other hook configuration
from a repository checkout.

<Warning>
Trusted local hook commands run with your OS user privileges and are not
sandboxed. Only configure scripts you trust, and keep `$MELDRA_HOME/hooks.json`
private (`0600`).
</Warning>

## Configure hooks

Create `~/.meldra/hooks.json` with `0600` permissions (`0700` on `~/.meldra`),
grouping ordered `command` handlers under each event and optional regex
`matcher`:

```json wrap
{
  "description": "External memory and pre-tool policy hooks",
  "hooks": {
    "SessionStart": [
      {
        "matcher": "startup|resume",
        "hooks": [
          {
            "type": "command",
            "command": "python3 ~/.meldra/hooks/load-memory.py",
            "timeout": 5,
            "statusMessage": "Loading external context",
            "additionalContextLimit": 8192
          }
        ]
      }
    ],
    "PreToolUse": [
      {
        "matcher": "edit_file|apply_patch",
        "hooks": [
          {
            "type": "command",
            "command": "python3 ~/.meldra/hooks/check-edit.py",
            "timeout": 10,
            "onError": "block"
          }
        ]
      }
    ],
    "RunEnd": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "python3 ~/.meldra/hooks/record-run.py",
            "timeout": 3
          }
        ]
      }
    ]
  }
}
```

Validate your configuration without API credentials:

```sh wrap
meldra hooks
meldra hooks --json
```

### Handler fields

| Field | Required | Default | Description |
| --- | --- | --- | --- |
| `type` | yes | — | Must be `"command"`. Other types (`http`, `mcp_tool`, `prompt`, `agent`) are rejected explicitly. |
| `command` | yes | — | Command string executed via the platform shell (`sh -c` on macOS/Linux, `cmd /C` on Windows), or executable path when `args` is set. |
| `args` | no | `[]` | Optional argument list; when non-empty, executes `command` directly without a shell. |
| `timeout` | no | `30` (`3` for `SessionEnd`, `RunEnd`) | Timeout in seconds (`1..600`, or `1..30` for `SessionEnd` and `RunEnd`). |
| `onError` | no | `"ignore"` | `"ignore"` warns and continues; `"block"` fails closed on spawn error, timeout, or non-zero exit (supported only on blocking events). |
| `statusMessage` | no | `""` | Short status text displayed while the hook runs. |
| `additionalContextLimit` | no | `8192` | Maximum bytes of `additionalContext` accepted from this handler (`1..65536`; total per event is capped at `16384`). |
| `env` / `env_vars` | no | empty | Extra environment values or parent environment variable names to pass. Provider keys (`OPENAI_API_KEY`, `DEEPSEEK_API_KEY`) are not inherited by default; `MELDRA_HOME` is always passed. |

`async: true` and `"if"` permission filters are rejected explicitly at load
time rather than silently ignored.

## Event matrix

Every command receives a JSON object on stdin containing `hook_event_name`,
`session_id`, `cwd`, `permission_mode` (`default`, `acceptEdits`, `plan`, or
`bypassPermissions`), and a namespaced `meldra` object (`schema_version`,
`mode`, `permissions`, `surface`, `task_id`, `run_id`, plus event-specific
fields).

| Event | Matcher target | Event-specific input | Blocking / continuation | Context (`additionalContext`) | Exit code `2` | Default timeout |
| --- | --- | --- | --- | --- | --- | --- |
| `SessionStart` | `source` (`startup`, `resume`, `fork`) | `source` | None | Yes (plain text or JSON) | Warns only | `30s` |
| `SessionEnd` | `reason` (`completed`, `cancelled`, `error`, `waiting_approval`, `other`) | `reason` | None (best effort) | No | Warns only | `3s` |
| `UserPromptSubmit` | none (`""` or `"*"`) | `prompt`, `turn_id` | `decision: "block"` or `continue: false` rejects prompt before inference | Yes (plain text or JSON) | Blocks prompt | `30s` |
| `PreToolUse` | `tool_name` | `tool_name`, `tool_input`, `tool_use_id`, `meldra.effect` | `permissionDecision: "deny"`, `decision: "block"`, or `continue: false` denies before dispatch | Yes (JSON) | Denies tool | `30s` |
| `PermissionRequest` | `tool_name` | `tool_name`, `tool_input`, `tool_use_id`, `meldra.operation`, `meldra.title`, `meldra.detail`, `meldra.source` | `hookSpecificOutput.decision.behavior: "deny"`, `permissionDecision: "deny"`, `decision: "block"`, or `continue: false` denies before prompting | No | Denies tool | `30s` |
| `PostToolUse` | `tool_name` | `tool_name`, `tool_input`, `tool_use_id`, `tool_response` | None (tool already succeeded) | Yes (JSON) | Warns only | `30s` |
| `PostToolUseFailure` | `tool_name` | `tool_name`, `tool_input`, `tool_use_id`, `error`, `is_interrupt`, `tool_response` | None (dispatched tool failed) | Yes (JSON) | Warns only | `30s` |
| `Stop` | none (`""` or `"*"`) | `turn_id`, `stop_hook_active`, `last_assistant_message` | `decision: "block"` with non-empty `reason` continues once; `continue: false` stops immediately | No | Continues once (stderr is reason) | `30s` |
| `PreCompact` | `trigger` (`manual`, `auto`) | `trigger`, `custom_instructions` | `decision: "block"` or `continue: false` keeps prior context | No | Blocks compaction | `30s` |
| `PostCompact` | `trigger` (`manual`, `auto`) | `trigger`, `compact_summary`, `meldra.from_bytes`, `meldra.to_bytes`, `meldra.kept_messages` | None | No | Warns only | `30s` |
| `RunEnd` | `run_status` (`succeeded`, `failed`, `cancelled`, `interrupted`, `waiting_approval`) | `meldra.task_id`, `meldra.run_id`, `meldra.task_status`, `meldra.run_status`, `meldra.reason`, `meldra.unknown_tool_calls` | None (observer only) | No | Warns only | `3s` |

## Runtime guarantees

- **Same lifecycle across surfaces.** TUI, line mode, and `--output-format stream-json`
  fire the same session, turn, tool, compaction, and run events. `meldra mcp serve`
  fires `PreToolUse`, `PermissionRequest`, `PostToolUse`, `PostToolUseFailure`,
  and `RunEnd` (`meldra.surface: "mcp"`) without synthetic chat events. Hook
  stdout and stderr are captured in bounded buffers and never written to
  `stream-json` or MCP protocol stdout.
- **Hooks never grant permissions.** A passing hook cannot bypass Plan mode or
  exact-operation approval. Any denial from any matching handler wins.
  `PermissionRequest` runs when asking for human approval, not when redeeming
  a matching `--approve-detail-hash` or answering an MCP approval request.
- **Prompt checks see the full submitted text.** A prompt that `UserPromptSubmit`
  rejects is not used as the task goal, replayed on resume, or included in
  compacted conversation history.
- **No silent bypass of unsupported output.** Returning `updatedInput`,
  `updatedToolOutput`, `permissionDecision: "allow"`, `decision: "approve"`, or
  `hookSpecificOutput.decision.behavior: "allow"` is rejected explicitly (failing
  closed on pre-checks).
- **Accurate execution evidence.** `PreToolUse` denial prevents tool dispatch
  and records `declined` with no start timestamp. `PostToolUseFailure` runs only
  for a dispatched operation's known failure — never for denials, cancellations,
  or `unknown` outcomes. Reused ledger replays do not re-trigger tool hooks.
- **Bounded `Stop` and committed `RunEnd`.** A `Stop` validator can request at
  most one continuation per turn and can never override explicit user
  cancellation. `RunEnd` fires only after Meldra commits the run disposition and
  releases the task lease, so observers can immediately run
  `meldra task show TASK_ID --json` and see the same state.

## Reusing Codex and Claude Code hooks

Simple command hooks written for [Codex](https://developers.openai.com/codex/hooks)
or [Claude Code](https://code.claude.com/docs/en/hooks) can often be reused with
minimal or no changes:

- **Works unchanged:**
  - `SessionStart` and `UserPromptSubmit` scripts that read `hook_event_name`,
    `session_id`, `cwd`, `source`, `prompt`, or `turn_id` and return plain text
    or `{"hookSpecificOutput":{"hookEventName":"...","additionalContext":"..."}}`.
  - `Stop` validators that inspect `stop_hook_active` and `last_assistant_message`
    and return `{"decision":"block","reason":"..."}`, `{"continue":false}`, or
    exit `2` with feedback on stderr.
  - `PreToolUse` and `PermissionRequest` guards that deny via exit `2`,
    `permissionDecision: "deny"`, or
    `hookSpecificOutput.decision.behavior: "deny"` and return `{}` or empty
    stdout when passing.
- **Requires adaptation:**
  - **Tool names and input schemas:** Meldra uses its real tool names
    (`edit_file`, `apply_patch`, `run_command`, `read_file`, `web_fetch`,
    `mcp__<server>__<tool>`) rather than emulating `Bash`, `Edit`, or `Write`.
  - **Config location and unsupported features:** Place config in private
    `$MELDRA_HOME/hooks.json` instead of repository `.codex/` or `.claude/`
    directories. Remove unsupported handler types (`http`, `mcp_tool`, `prompt`,
    `agent`), `async: true`, `if` rules, argument/output rewrites
    (`updatedInput`, `updatedToolOutput`), and approval-granting decisions
    (`permissionDecision: "allow"`, `decision: "approve"`).
  - **Durable run observers:** Subscribe to `RunEnd` instead of `Stop` when
    recording task outcomes; `Stop` fires before the turn ends, while `RunEnd`
    reports the committed `waiting_approval`, `cancelled`, `interrupted`,
    `failed`, or `succeeded` disposition.
