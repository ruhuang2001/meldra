# Architecture and runtime boundaries

Meldra is a local, serial foreground coding agent. The 0.2.0 implementation adds
versioned execution history, task/workspace ownership, structured tool outcomes,
and explicit recovery. It is under integration and release validation; this
branch is not a declaration that 0.2.0 has been released.

Closing the terminal ends the current execution. There is no daemon, scheduler,
automatic restart, detach/attach protocol, or multi-agent execution in this
version. Persistence lets users inspect and explicitly continue work.

## Dependency direction

```text
main.go                          process entry and link-time version
  -> internal/app                signals, CLI/TUI and Agent
       contains Workspace, SessionTools, SessionSaver and taskExecution
       -> internal/provider      Inference interface; Responses adapter and SDK
       -> internal/tool          contracts, observation and validated dispatch
       -> internal/task          record types and transition rules
       -> internal/store         SQLite records, artifacts and execution leases
            -> internal/task
```

`taskExecution` is currently in `internal/app`. It composes the agent, workspace
and store and surrounds real execution with durable records. `internal/task`
contains data contracts and transition rules; it does **not** invoke a Provider,
run tools or schedule work. `internal/store` depends on those contracts, not on
the UI or model SDK. Workspace/SessionTools register handler callbacks with the
tool registry; the tool package does not import the application to invoke them.
A future extraction of orchestration must preserve these
directions rather than pretending a package rename already achieved it.

Production application code does not import the model SDK directly. Boundary
checks and fake-provider tests protect this separation; HTTP/SSE integration
tests exercise the actual provider adapter.

## Agent and application

`Agent.Run` reads interactive requests. `Agent.RunTurn(ctx, input)` executes one
request without reading terminal input; a turn can make multiple model requests
and tool calls. The CLI and TUI construct the same task-recording adapter. A bare
Agent constructed by an embedding caller or unit test does not automatically
create a durable task store.

An Agent owns mutable conversation state and rejects concurrent `RunTurn` calls
with `ErrAgentBusy`. Workspace tool dispatch separately rejects overlap on its
mutable edit/undo state. Cross-process execution ownership is provided by the
store lease, not by either of these in-process guards. A future parallel runner
must create separate agents and workspaces.

`SessionSaver` remains a narrow interface. Conversation messages, plan and
summary use the existing bounded JSON format with private permissions, atomic
replacement, directory synchronization and stale-save rejection. The ledger is
the source of evidence about executed tools; a conversation summary is not a
replacement for it.

## Provider and context

`provider.Inference` uses Meldra request/result types. The concrete Client owns
Responses SDK conversion, streaming fallback, response-size limits, idle
watchdogs and custom-gateway compatibility. Clients are serial conversation
objects and must not be shared between concurrent agents.

Provider-owned continuation items preserve protocol metadata, including opaque
fields required for replay. They are in-memory conversation inputs, not a
versioned durable checkpoint format. The task ledger stores model request and
completion events and available token counts, not raw provider reasoning or API
keys. Configuration records retain the model, sanitized provider identity and
workspace.

Custom-provider replay compaction preserves ordering and protected protocol
fields. Incremental size accounting removes repeated full-input JSON encoding
from the older compaction loop. Runtime benchmark reports measure this cost;
they are independent of model task-success evaluations.

## Tools, approvals and execution records

`tool.Definition` contains a JSON schema and a context-aware handler. The registry
rejects duplicate names/missing handlers and cancelled calls before dispatch.
Existing handlers can retain text/error implementations while execution-point
observations supply structured status, exit code, elapsed time, truncation and
retry information. The model still receives compatible human-readable output.
The task adapter adds bounded artifact references to the persisted result.

The real execution sequence is:

1. Acquire task and canonical workspace ownership, then create a Run.
2. Persist each tool's arguments, parameter hash and planned state before
   execution; transition it to running before calling the handler.
3. For an operation requiring approval, save the pending request before asking
   the user and persist its decision before the side effect. File approvals
   include the reviewed before/after fingerprints. Unused pending approvals
   expire when their run ends.
4. Execute using the current request context, then persist the result and
   bounded artifacts before scheduling more work.
5. Record the terminal Run status, preserving prior attempts, and release the
   ownership lease. A persistence failure stops subsequent side effects.

The workspace still owns path restrictions, command allowlists, diff previews,
atomic single-file replacement, in-process rollback and undo. A persisted
approval is tied to one operation; it is not transferable permission for a later
changed command or patch. This is not an OS sandbox: an approved program can
access the user's filesystem, network and other external systems.

## Record model and ownership

| Record | Meaning |
| --- | --- |
| Task | Goal, canonical workspace, session association and latest overall state |
| Run | One foreground attempt, configuration, executor, timestamps and immutable terminal outcome |
| ToolCall | Provider call identity, parameters, effect class, execution state and structured result |
| Approval | Pending/approved/declined/expired decision bound to a call and reviewed operation |
| Event | Task-local monotonic sequence, schema version, IDs and execution linkage |
| ArtifactRef | Digest, name and size of retained tool/verification output |

Task states are `queued`, `running`, `waiting_approval`, `completed`, `failed`,
`cancelled` and `interrupted`. Runs use `succeeded` as their successful terminal
state. Tool calls move from `planned` to `running`, then to `succeeded`, `failed`,
`declined`, `cancelled` or `unknown`; planned work can be declined/cancelled before
starting. `unknown` means the effect cannot be established from durable records.
It is not interchangeable with cancellation or failure.

Store writes use SQLite transactions. Partial indexes reject multiple active
runs for the same task. Advisory file locks cover the task and canonical writable
workspace for the whole foreground turn, across processes using the same user
cache directory. Separate `MELDRA_HOME` directories still contend on workspace
ownership in that cache. Locks release when their process exits; their files are
not deleted, because unlinking a held lock would allow a different inode to be
locked. Editors and unrelated tools do not participate in this protocol.

SQLite cannot atomically commit an external command or several file changes with
its tool record. The implementation provides explicit uncertainty and recovery,
not exactly-once execution of arbitrary side effects.

## Exit and recovery

The process entry handles Ctrl-C, SIGTERM and SIGHUP. Cancellation reaches model
requests and tools; command execution kills its owned process group and bounds
pipe cleanup. The task adapter gives terminal-state persistence a bounded
five-second context independent of the cancelled request. Normal EOF after
`--prompt` finishes the request before ending input. A real terminal hangup ends
the foreground task.

SIGKILL, power loss and children that escape their process group remain limits:
cleanup and final persistence may not happen. Starting Meldra or listing records
does not resume them. A recorded running state can remain stale until explicit
recovery, and inspection says so.

`task resume ID` reacquires ownership and marks abandoned attempts interrupted.
Previously planned operations are cancelled; operations that had started but
lack a saved result become unknown. Supported file operations are reconciled
against all recorded hashes, existence and modes. Complete postimages confirm a
completed change; complete preimages indicate no net change; mixed states or user
edits remain unresolved. This reconciliation does not overwrite current files.

Unknown external commands require inspection and an explicit
`task resolve ID CALL_ID --outcome succeeded|failed --reason TEXT`. Resolution
records evidence without invoking a tool or changing an old Run's outcome.
Unresolved calls prevent a new run. Re-delivery of the same provider call ID and
parameter hash uses its recorded result; that check is not a promise to recognize
arbitrarily reworded commands with new IDs.

Recovery context includes the original goal, missing-history markers and a
bounded excerpt of recent tool results, alongside conversation messages and
summary. The full stored ledger remains inspectable; the entire history is not
sent to the model on every resume.

## Storage and limits

Under `MELDRA_HOME` (default `~/.meldra`):

```text
config.toml / credentials.env   settings and credentials
sessions/                      preserved legacy 0.1.x snapshots
tasks/tasks.db                 SQLite schema version 1, WAL mode, FULL sync
tasks/sessions/                 current bounded conversation snapshots
tasks/artifacts/<sha256>        retained output referenced by task records
```

Execution lock files live in `os.UserCacheDir()/meldra/locks`, not inside the task
database. The store requires private real directories/files and rejects unsafe
symlinks. Unknown newer database schemas are rejected instead of downgraded.

| Bound | Current limit |
| --- | --- |
| Tool arguments / approval evidence object | 1 MiB |
| Persisted tool excerpt / model-facing command output | 256 KiB |
| Retained command log before artifact persistence | 16 MiB |
| Event data | 64 KiB |
| One store artifact | 16 MiB |
| Total artifact files per store | 256 MiB |
| SQLite database pages | 256 MiB |
| One JSON session snapshot | 2 MiB, at most 100 messages |
| Built-in command duration | 60 seconds by default, at most 120 seconds |

Commands capture stdout/stderr separately for a 256 KiB model-facing excerpt
and a log of up to 16 MiB, which is persisted as an artifact. Log output beyond
that bound is discarded with a trailing truncation marker. Patch/edit and other
tool-result artifacts retain their already bounded text. Database, WAL/SHM, snapshots, locks and temporary
files are separate, so the database page cap is not a hard cap on the whole data
directory. There is no automatic pruning or task-deletion command. Limit errors
stop recording/execution rather than silently discarding history. Back up stopped
stores before maintenance; copying only a live `tasks.db` is unsafe.

## CLI inspection and legacy compatibility

`tasks [--json]` returns the latest 1000 task records across workspaces. It has no
list-pagination flag in 0.2.0; retain known IDs to inspect older tasks. `task show
ID --json` includes runs, calls, approvals and artifact references. `task events
ID --json [--after N]` returns at most 1000 sequenced JSONL events. Advance the
cursor to the last returned `sequence` until the result is empty. Inspection
returns the recorded state at query time; it is not a live subscription and does
not acquire an execution lease or call a model.

Legacy `sessions`, `resume SESSION_ID` and the session picker remain available.
`resume latest` chooses from the current workspace (or explicit `--workspace`).
Lists merge legacy and current snapshots, prefer the current snapshot with the
same ID, and report invalid snapshots as skipped. Empty/corrupt snapshots are not
a complete inventory of task execution: a ledger record can outlive a usable
conversation snapshot. Import validates and retains original bytes, deduplicates
the source ID/content digest, and marks missing historical tool records instead
of inventing successes. New messages are written under `tasks/sessions/`.

See [release preparation](release-0.2.0.md) for consistent backups, upgrade,
downgrade and known limitations. Restoring a 0.1.x session cannot undo repository
changes or read the 0.2 execution ledger.

## Verification and next increments

The root remains the build target (`go build .`), and `-X main.version=...`
continues to identify binaries. `make check` covers formatting, vet, module
consistency, race tests, aggregate coverage and build. Store tests cover state
transitions, ownership, crash boundaries, schema rejection and imports. App tests
cover real tools, persistence failures, recovery, legacy compatibility, signals,
owned subprocesses and actual PTY closure. Report tooling distinguishes runtime
performance, offline correctness and live model quality.

The release gates remain in [the 0.2.0 roadmap](roadmap-0.2.0.md). Implemented tests
must be run on the exact reviewed candidate; their existence alone is not release
evidence. Native four-platform packaging and the separately budgeted live quality
cohort remain publication gates.

After this runtime is validated, independent child workspaces, parent/child
contracts, inherited budgets and serial result integration can build on these
records. No shared-workspace parallel writer, distributed scheduler or cloud
service is implied by the current architecture.
