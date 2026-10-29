# Model-generated context compaction

Status: proposed, not implemented. This is the phase-two issue deliverable from
`plan.md`; implementation follows the stable JSON stream protocol.

## Problem

Native Responses continuation retains implicit server history, while saved
conversation snapshots have bounded retention but no model-generated summary.
Custom-provider budgeting replaces older tool outputs with omission markers;
that bounds input but does not explain what was discarded.

## Proposed behavior

- Add `/compact [focus]` to both foreground interfaces, usable while idle in
  Plan and Build. The operation changes metadata only and grants no permissions.
- Ask the configured model for a bounded summary without exposing executable
  tools. Store the summary in `Session.Summary` and an immutable task artifact.
- Preserve original user requests verbatim in durable artifacts, including
  requests exceeding the display snapshot limit. Retain the latest configurable
  number of turns with tool names, bounded arguments, call IDs, and outcome
  artifact references. Never summarize away unresolved approvals or unknown
  outcomes; references must lead back to original records.
- Inject the summary as clearly delimited prior context in the next developer
  instruction, subordinate to current user intent and runtime policy.
- Clear both runtime and persisted `PreviousResponseID` only after the new
  context and artifact references are durably committed. Rebuild native history
  from the compacted context, avoiding the old implicit chain.
- Append `context.compacted` with `from_bytes`, `to_bytes`, `kept_messages`,
  source turn range, summary artifact, and `compacted_calls`. `task show` must
  identify the folded call IDs; a CLI/TUI notice identifies the source turns and
  inspection command. Unknown stream event kinds remain forward-compatible.
- An optional `compact_threshold_ratio` triggers the same path between turns.
  Define its denominator explicitly rather than guessing a model context size.

## Decisions required before implementation

Choose a configured context capacity and token estimator: native input-token
usage can drive a threshold only when the model capacity is known; custom
providers need a documented byte-budget fallback. The original plan omitted a
default ratio and retained-turn count, so neither is an agreed value. Decide
whether model-initiated `compact_context` is in scope separately; v1 can remain
user/threshold initiated. Summary artifacts are proposed for inspectability.

## Acceptance and failure verification

1. A real-binary loopback E2E creates multiple tool turns, compacts them, then
   verifies the next native request has no prior response ID and carries the
   summary, verbatim requests, and retained call evidence.
2. `task show` exposes every folded ID and original artifacts remain readable.
   Before/after records prove no admitted operation or outcome was changed.
3. Plan compaction performs no write/exec workspace action. Both line and PTY
   modes render the notice and remain usable afterward.
4. Provider failure, cancellation, empty/oversized summary, artifact failure,
   snapshot failure, and crash between writes retain recoverable original
   context. Recovery never silently reruns inference or tools.
5. Repeated compaction and resume do not duplicate requests or lose attachment
   references. A corrupt summary artifact fails closed.
6. E2E reports retain requests, transcripts, ledger snapshots, and artifacts;
   `make check` passes with the source tree unchanged during coverage collection.
