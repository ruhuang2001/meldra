# Model-generated context compaction

Status: **implemented in the working tree, unreleased.** Design of record for
[issue #65](https://github.com/ruhuang2001/meldra/issues/65). This document
replaces the earlier draft's open questions with decided values; implementation
follows the stable JSON stream protocol.

## Problem

Native Responses continuation retains implicit server history, while saved
conversation snapshots have bounded retention but no model-generated summary.
Custom-provider budgeting replaces older tool outputs with omission markers;
that bounds input but does not explain what was discarded. `Session.Summary`
exists today but is only ever written by the `save_summary` tool, and
`resumeContext()` injects it only for resumed or custom-provider turns.

## Decisions

| Question | Decision |
| --- | --- |
| Scope of v1 | `/compact [focus]` plus automatic threshold triggering. Model-initiated `compact_context` stays out of scope. |
| Retained tail | 8 messages by default (user + assistant counted together), configurable. |
| Measurement unit | UTF-8 bytes of the locally rebuilt turn context. No tokenizer, no estimated token counts. |
| Threshold denominator | The per-turn byte budget already in force for that path — see [Measurement](#measurement-and-the-denominator). |
| Default threshold | 80% of that budget; `0` disables automatic compaction. |
| Token usage as driver | Rejected. `input_tokens` is recorded per response, but model capacity is unknown for both native and custom endpoints, so it cannot form an honest denominator. |

## Configuration

Three new keys in `config.toml`, parsed and written by the existing
`parseConfigTOML`/`SaveConfig` pair and surfaced by `config show`:

| Key | Type | Default | Range | Meaning |
| --- | --- | --- | --- | --- |
| `compact_keep_messages` | int | `8` | `2..64` | Messages retained verbatim after compaction. |
| `compact_threshold_percent` | int | `80` | `0..100` | Trigger automatic compaction when the rebuilt turn context reaches this percentage of the budget. `0` disables. |
| `max_turn_context_bytes` | int | `4194304` | `1..67108864` | Byte budget for the rebuilt turn context on the native Responses path. Also caps the denominator when a custom endpoint's own budget is larger. |

The defaults are chosen so that a fresh configuration compacts only on long
sessions: 80% of 4 MiB is roughly 3.2 MiB of rebuilt text context, well beyond
an ordinary turn. `compact_keep_messages = 8` retains about four exchanges,
which keeps the running thread, the current plan, and the latest tool outcomes
in context while the summary carries everything earlier.

`max_turn_context_bytes` deliberately does **not** claim to be a model context
window. It is a local byte budget Meldra enforces and can measure; the real
window may be larger or smaller.

## Measurement and the denominator

Both trigger paths use one measurement: the byte length of the text context
Meldra itself rebuilds for the upcoming turn, as assembled at the top of
`Agent.RunTurn` (`internal/app/agent.go`) — resumed summary, recent
conversation, reference history, plan, project and execution context, and the
current user request. Image and file reference items are bounded separately by
`maxReferenceTotalBytes` and are not re-measured here.

The denominator is explicit and differs per path, because only one of them has
a real budget today:

- **Custom endpoint** (`a.customProvider`): `max_custom_turn_input_bytes`, the
  limit `provider.BoundInput` already enforces, further capped by
  `max_turn_context_bytes` so a large custom budget cannot quietly disable the
  trigger.
- **Native Responses**: `max_turn_context_bytes`. The server also retains
  implicit history that Meldra cannot measure, so on this path the threshold is
  a **floor, not a ceiling**: it fires before local context is large, and never
  implies the server-side chain was within any limit.

When `compact_threshold_percent × denominator ÷ 100` is reached, the same code
path as `/compact` runs before inference. The turn then proceeds without
`previous_response_id`, using the compacted context. A compaction that would
leave the measured context unchanged is skipped and the turn continues
uncompacted; the budget errors that already exist still apply.

## `/compact [focus]`

Available while idle in both foreground interfaces and in both modes. It
changes metadata only — it never grants permission, never changes mode, and
never dispatches a tool. The optional `focus` argument is a short steer such as
`/compact the flaky integration test`.

Handling points:

- `builtinSlashCommands` in `internal/app/commands.go` gains
  `{Name: "compact", Description: "Summarize older context while idle", ArgumentHint: "[focus]"}`.
- `parseControlText` in `internal/app/control.go` gains a `"compact"` kind so
  that submitting it mid-turn fails with an explicit
  `compact is available only while idle` instead of being read as ordinary
  steering text. `SubmitControl` rejects that kind.
- `Agent.Run`'s idle branch dispatches it the same way `/context` is dispatched
  today, then continues the loop.

## Summarization request

The summary is produced by one ordinary inference over history, using the
configured model and no tools (`Tools` empty), so a compaction can never admit
a workspace operation. Guardrails:

- `MaxOutputTokens` is fixed at 1024; a truncated response is an error, not a
  partial summary.
- The response must be non-empty after trimming and at most
  `maxSessionSummaryBytes`; otherwise the operation fails closed and the
  session is left untouched.
- Cancellation, provider failure, and response-validation failure leave both
  `Session.Summary` and `Session.PreviousResponseID` unchanged.
- Raw reasoning and credentials are never placed in the prompt or the artifact.

The instruction asks the model to emit a bounded, structured summary covering:
the original user requests, decisions taken, files touched, verification
performed and its result, open questions, and explicitly which tool calls were
folded. It is delimited as `[Prior context summary — compacted]` when injected,
and is subordinate to the current user request and runtime policy.

## Retained evidence

Compaction folds **older messages** into the summary. Everything Meldra must
never lose is preserved outside the summary:

- **User requests verbatim.** Full original text is stored in the compaction
  artifact, including requests longer than the `maxSessionMessageBytes` display
  snapshot truncation.
- **Recent tail.** The last `compact_keep_messages` messages stay as ordinary
  `Session.Messages`, carrying their `ControlID` and `References`, so
  attachment references survive.
- **Tool call evidence.** For folded turns, the artifact records tool name,
  call ID, status, bounded arguments digest, and outcome artifact references
  read from the task ledger. `task show` resolves the IDs back to the original
  records.
- **Plan.** `Session.Plan` is not summarized away.

Payloads that are never summarized despite being old:

- Any tool call whose status is `unknown`.
- Any process whose effects are `unknown`.
- Any pending approval.

If such a record exists, `/compact` refuses with a message naming the count and
the inspection command, and automatic compaction is skipped with a notice. The
user must resolve them first; compaction never converts an unresolved outcome
into a claim.

## Persistence order

Compaction must not advertise a context change that did not happen, and must
not strand a `previous_response_id` pointing at history it just discarded:

1. `PutArtifact` writes `context-compaction.json` — schema version, focus,
   from/to byte estimates, retained message range, verbatim requests, folded
   call IDs, and the summary text.
2. `SessionStore.Save` commits the new snapshot: `Summary` set,
   `Messages` trimmed to the retained tail, `PreviousResponseID` cleared.
3. `AppendEvent` records `context.compacted` with the artifact reference.

Ordering is deliberate. A failure at step 1 changes nothing. A failure at
step 2 stops the turn with the existing `persistenceError` behavior while the
artifact remains, and the session still holds its original summary, messages,
and response ID — a retry is a fresh operation, never a replay of the old
chain. A failure at step 3 also stops the turn; the session is already
consistent and the missing event is an audit gap, never a false completion
record. Recovery never re-runs inference or tools.

`PreviousResponseID` is only ever cleared after the artifact and the snapshot
are durable, matching the issue's requirement.

## Ledger event

`context.compacted` is an ordinary `task.Event` blob, so unknown kinds remain
forward-compatible for older readers:

```json
{
  "from_bytes": 4194304,
  "to_bytes": 311296,
  "kept_messages": 8,
  "source_turns": {"first_sequence": 12, "last_sequence": 47},
  "artifact": {"id": "...", "name": "context-compaction.json", "sha256": "...", "size": 41231},
  "compacted_calls": ["<call-id>", "..."],
  "trigger": "manual",
  "focus": "flaky integration test"
}
```

`trigger` is `manual` or `threshold`. `compacted_calls` is bounded; when the
count would exceed the event budget the event states the total and the
artifact holds the full list.

## Presentation

- `meldra task show ID` gains a compaction section listing every
  `context.compacted` event: trigger, source turn sequence range, from/to
  bytes, artifact ID, and each folded call ID. `--json` includes the same data.
- Both foreground interfaces emit one notice after compaction, identifying the
  source turn range and the inspection command, for example
  `Compacted turns 12–47 into a summary (4.0 MiB → 304 KiB). Inspect: meldra task show <id>`.
- Line and PTY modes must both render the notice and remain usable afterward;
  the notice never enters stdout JSON or MCP protocol output.

## Files to change

| File | Change |
| --- | --- |
| `internal/app/config.go`, `internal/app/cli.go` | Three new keys in `Config`/`Settings`/`ConfigShow`, template, parse, validate, save, environment passthrough. |
| `internal/app/compaction.go` (new) | Measurement, trigger decision, prompt/request construction, artifact write, durable commit, event append. |
| `internal/app/agent.go` | Auto-trigger after `execution.begin`, before inference, then rebuild input without `previous_response_id`. |
| `internal/app/control.go` | `compact` kind; idle dispatch; busy rejection. |
| `internal/app/commands.go` | Builtin command entry. |
| `internal/app/task_cli.go` | `task show` compaction section and `--json` field. |
| `internal/app/ui_events.go` | No new kind needed; compaction notices use `UIEventNotice`. |
| `internal/app/session.go` | Validation unchanged; document that `Summary` may now exceed a single `save_summary` call's origin. |
| `docs/capabilities.md`, `docs/configuration.md`, `docs/providers.md`, `docs/task-storage.md` | Command table, new keys, distinction from omission-marker trimming, event and artifact documentation. |
| `scripts/` | Loopback E2E scenario. |

## Acceptance and failure verification

1. A real-binary loopback E2E creates multiple tool turns, compacts them, then
   verifies the next request carries the summary, verbatim requests, and
   retained call evidence and sends no prior response ID.
2. `task show` exposes every folded ID; original artifacts remain readable;
   before/after records prove no admitted operation or outcome changed.
3. Compaction in Plan mode performs no write or exec workspace action.
4. Provider failure, cancellation, empty or oversized summary, artifact
   failure, and snapshot failure each leave the session recoverable and
   unchanged where the design requires it.
5. An unresolved approval or unknown outcome blocks compaction and is reported
   by count and inspection command.
6. Repeated compaction and resume do not duplicate requests or drop attachment
   references; a corrupt summary artifact fails closed.
7. Threshold triggering fires on the documented denominator and never claims a
   server-side limit was respected on the native path.
8. `make check` passes with the source tree unchanged during coverage
   collection.

## Out of scope

- Model-initiated `compact_context` tool calls. v1 is user- and
  threshold-initiated only.
- Token-based thresholds; they need a known model capacity.
- Compaction of raw reasoning. Reasoning is never persisted and is never
  fabricated into a summary.
- Cross-provider reasoning preservation, which belongs to
  [issue #66](https://github.com/ruhuang2001/meldra/issues/66).
