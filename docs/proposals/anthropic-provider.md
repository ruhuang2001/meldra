# Anthropic Messages provider adapter

Status: proposed, not implemented. The current phase documents and verifies
Responses-compatible local endpoints; this is a separate second-phase issue.

## Proposed boundary

Implement a Messages adapter under `internal/provider` behind `Inference`. Keep
SDK/protocol types out of `internal/app`, `internal/tool`, `internal/task`, and
`internal/store`. Preserve the existing tool definitions and serial dispatch.

Before introducing a second protocol, add an explicit provider capability
contract, including `SupportsPreviousResponse()` and `ReplayReasoning()`. Replace
app-layer URL heuristics, including MCP sampling, with those capabilities.
Responses gateways continue to use their conservative history and response
budgets. A general provider registry is deferred until two adapters exist.

Translate function calls/results to `tool_use`/`tool_result` entirely inside the
adapter, preserving call identities, errors, cancellation, and streaming partial
text. Messages has no Responses continuation ID, so every request rebuilds and
budgets complete appropriate history. Rework opaque conversation items without
exporting model SDK types across the boundary.

## Switching and reasoning

Cross-provider reasoning is not guaranteed to survive. Responses reasoning items
must never be fabricated into Anthropic thinking blocks; incompatible reasoning
is omitted and the context rebuilt from user-visible conversation and durable
tool evidence. Never persist raw reasoning or credentials. Clearly document the
effect of switching providers before enabling it.

## Acceptance and failure verification

- Real-binary fake Messages transport proves text streaming, tool calls, multiple
  serial tool rounds, bounded history, cancellation, and resumable partial output.
- Native/gateway/Anthropic capability selection is explicit; no hidden dependency
  on endpoint hostnames remains in app orchestration or MCP sampling.
- Malformed or duplicate call IDs, truncated responses, authentication failure,
  unsupported content, and output/context overflow fail before unsafe dispatch.
- Provider switches preserve requests, attachment references, and call outcomes
  without replaying side effects or leaking reasoning into session/ledger files.
- Plan restrictions, exact approvals, persistence-failure stops, and process/task
  ownership remain identical across CLI, TUI, and MCP.
- Record repeatable E2E transcripts, wire requests, and task evidence; pass the
  full `make check` gate before review.
