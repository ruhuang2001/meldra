# Architecture and runtime boundaries

Meldra is currently a local, serial coding agent. Its intended direction is
background tasks and controlled parent/child agent execution. The current
refactor establishes boundaries for that work; it does not introduce a daemon,
durable task scheduler, or concurrent workspace writers.

## Dependency direction

```text
main.go                         process entry and link-time version
  -> internal/app               application composition and user interaction
       -> provider.Inference    request/result boundary
            -> provider.Client  Responses SDK, streaming and gateway compatibility
       -> internal/tool         context-aware definitions and validated dispatch
       -> SessionSaver          persistence boundary (currently JSON SessionStore)
```

Production application code must not import the model SDK directly. A test
checks this boundary. Existing HTTP/SSE compatibility tests exercise the real
provider adapter; runner boundary tests use an in-memory provider without an
SDK or terminal.

### Application

`internal/app` owns configuration, CLI/TUI composition, sessions, workspace
policy, and the serial agent. `Agent.Run` is the interactive adapter;
`Agent.RunTurn(ctx, input)` executes exactly one user request without reading
stdin. Both use the same tool loop. A turn may contain multiple model requests
and tools. Context cancellation is returned by `RunTurn`; the interactive
adapter retains the existing graceful Ctrl-C behavior.

An Agent is the owner of mutable conversation state. Concurrent `RunTurn`
requests on the same Agent are rejected with `ErrAgentBusy`, not silently
queued. This is an in-process invariant, not a cross-process session lease.
Interactive `Run` must not be run concurrently with another runner on the same
Agent. A future scheduler should create separate Agents and workspaces.

`SessionSaver` is intentionally narrow: the runner can save a session without
knowing how the CLI lists or selects sessions. The existing JSON format,
revision checks, file permissions and durable writes remain unchanged.

### Provider

`provider.Inference` accepts Meldra request types and returns Meldra response
and partial-stream information. Application code never reads SDK unions.
The current implementation still uses the Responses protocol. Continuation
items are opaque, provider-owned values: they retain reasoning and message
metadata needed for compatible-provider replay. They are not a versioned,
durable task checkpoint format and should not be persisted as one.

The Client owns stream capability fallback for one serial conversation.
Per-request options supply response limits, idle timeout and compatibility mode.
Synchronous observers report status and text without importing terminal types.
Presentation filtering never changes the raw text retained for session recovery.
Existing fallback restrictions, response limits and stream watchdog behavior
remain in the adapter. Clients must not be shared between concurrent Agents.

### Tools and workspaces

`tool.Definition` contains the schema and a handler taking
`context.Context` and JSON arguments. `tool.Registry` rejects missing handlers
and duplicate names before the first model request, and rejects cancelled
calls before dispatch. Definitions and schemas are immutable after registration.
Tool ordering in model requests is preserved independently of map dispatch.

Registered workspace tools install the current invocation's context and restore
it on return. A workspace rejects overlapping registered calls because edit and
undo state is mutable. This does not provide OS isolation or cross-process
coordination. Workspace configuration must be completed before execution.
Handlers must not recursively invoke another registered tool on that workspace.

Tool results remain text/error in this migration. Typed command outcomes,
retries, persistent approval records and durable execution receipts need a
separate change; no automatic retry of side effects is introduced here.

## Compatibility and verification

The root remains the build target (`go build .`). `-X main.version=...` and the
GoReleaser entry point stay unchanged. Configuration and session paths, command
flags, approval prompts, tool names/schemas and session file format are retained.
Tests moved with their implementation; transport-only tests live in provider.

Run `make check` for formatting, vet, module consistency, race tests, aggregate
coverage and build. The boundary tests also verify headless continuation,
concurrent turn rejection and cancellation reaching an active tool.

## Next architectural increments

1. Add explicit Task/Run/ToolCall records and versioned persistence. Record
   intent and outcome around side effects; treat crash-ambiguous calls as unknown
   until reconciled, rather than automatically replaying them.
2. Add a local service that owns runs. Terminal detach should not cancel a run;
   attach replays sequenced events. Persist approval requests tied to operation
   parameters and the reviewed workspace state.
3. Add single-owner execution leases and isolated worktrees for child tasks.
   Parent tasks integrate child artifacts serially and verify the combined tree.
4. Add shared parent/child budgets, structured command jobs and artifact storage.
   OS sandboxing is independent of worktree isolation.

Do not add parallel tool execution against a shared Workspace or treat a JSON
session revision check as ownership of an executing task. A future SQLite store
can implement the persistence boundary, but storage migration alone does not
make external commands transactional.
