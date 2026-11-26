# Lifecycle command hooks

Keep lifecycle hooks opt-in and local to the private `$MELDRA_HOME/hooks.json` configuration (`0600` regular file inside a `0700` non-symlink directory). Never discover or execute hook definitions from a workspace checkout, so opening an untrusted repository cannot run local scripts.

Expose one shared command-hook lifecycle aligned with Codex and Claude Code across TUI, line mode, and `stream-json`, while `mcp serve` shares only tool, approval, and durable `RunEnd` events without synthetic chat events. Capture hook stdout and stderr in bounded buffers instead of inheriting process descriptors so hook output cannot corrupt JSONL or MCP stdio transport. Strip provider API keys from hook environments by default and always pass `MELDRA_HOME` so observers can inspect the same task ledger.

Enforce Meldra's permission and evidence boundaries at execution owners rather than UI events. `PreToolUse` runs after `PlanTool` and before `StartTool`, transitioning denied calls directly from `planned` to `declined` with zero side effects and no `PostToolUseFailure`. `PermissionRequest` runs only when human approval is actually required. Hooks may deny but can never grant permissions, bypass Plan mode, or rewrite reviewed tool arguments; unsupported handlers, fields, decisions, and rewrites fail explicitly instead of silently bypassing checks.

Allocate the run before `UserPromptSubmit` so observers have stable IDs, but record the request and fill the initial task goal only after admission. A rejected queued turn is cancelled in the control journal, not marked as applied. The journal records control receipt separately from conversation history.

For a two-phase MCP approval, plan one call before either hook. Keep the call planned while waiting, release the lease, and bind the single-use challenge to the existing call and its pre-hook context. On redemption, revalidate the fingerprint and resume the same waiting call under a new lease. A hook denial settles the planned call without dispatch; an accepted operation still needs to pass normal workspace admission.

Separate `Stop` (pre-finish validation bounded to one continuation per turn, always subordinate to explicit user cancellation) from `RunEnd` (an observer-only notification emitted only after `FinishRun` or `waiting_approval` persistence succeeds and the task lease closes). Reused idempotent tool replays do not re-trigger tool hooks, and `PostToolUseFailure` fires only for known failures of dispatched tools—never for denials, cancellations, or `unknown` outcomes.

[Lifecycle hooks](../integrations/hooks.md) · [Durable execution](./0007-durable-execution.md) · [Private configuration](./0009-private-configuration.md)
