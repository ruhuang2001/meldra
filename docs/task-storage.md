# Foreground task storage contract (0.2.0)

Status: implemented schema 1. The database stores execution evidence; opening
it never resumes a model, a tool, a command, or a pending approval.

## Driver and layout

The task ledger uses `modernc.org/sqlite v1.60.1`, a pure Go SQLite driver that
requires Go 1.26.0 (the project builds with Go 1.26.6). It compiles with
`CGO_ENABLED=0` for Darwin/Linux on both amd64 and arm64. SQLite runs locally
in WAL mode with `synchronous=FULL`, foreign keys, a five-second busy timeout,
and a single connection per Store. Mutation transactions use `BEGIN IMMEDIATE`
to serialize competing processes before they read mutable state. This is a local-filesystem design; network
filesystems that do not implement SQLite/advisory-lock semantics are unsupported.

The application places the ledger below `MELDRA_HOME/tasks/`:

```text
tasks/
  tasks.db              # schema and immutable execution history
  tasks.db-wal          # SQLite recovery journal while open
  tasks.db-shm          # SQLite WAL coordination
  artifacts/<sha256>    # deduplicated complete tool outputs
```

Directories are mode 0700 and files are mode 0600. Storage paths reject symlink
components, except macOS's system `/var`, `/tmp`, and `/etc` aliases. Existing
database/WAL/SHM files must be regular files. These files can contain source,
command output, and approved operations: private permissions are not encryption.
The application persists provider identifiers without URL credentials or query
strings, never the API key or raw provider reasoning. Tool output can itself
contain secrets, so users must protect ledger backups as project data.

## Records and transitions

| Record | Meaning |
| --- | --- |
| Task | User goal, canonical workspace, session identity, current status |
| Run | One foreground attempt, configuration, owner label, timestamps and reason |
| ToolCall | Raw bounded arguments and digest, effect, execution phase and result |
| Approval | Exact call/parameters, operation, workspace evidence and decision |
| Event | Stable event ID, task-local sequence, schema, status/reason and associations |
| ArtifactRef | Content digest, name and length of an immutable output blob |

Task status is `queued`, `running`, `waiting_approval`, `completed`, `failed`,
`cancelled`, or `interrupted`. `completed` means the model finished its turn;
it does not prove that a patch passes tests. Verification remains separate
tool results and artifact evidence.

Run transitions are `running <-> waiting_approval`, then one terminal result:
`succeeded`, `failed`, `cancelled`, or `interrupted`. A terminal Run cannot be
changed. Explicit resume creates another Run, preserving all earlier attempts.

Tool transitions are:

```text
planned -> running | declined | cancelled
running -> succeeded | failed | declined | cancelled | unknown
unknown -> succeeded | failed | declined | cancelled  (explicit reconciliation only)
```

Ordinary execution cannot transition out of `unknown`. The separate
`ResolveTool` API requires an evidence reason and a finished original Run,
records `tool.reconciled`, and never executes the operation. It never rewrites
the original Run's terminal state. New Runs are rejected while any outcome
remains unknown. A missing result is not evidence of failure or success.

Approvals are scoped to one ToolCall and its exact raw argument digest. They
cannot be reused by a new call or Run. A pending decision can be consumed once
as approved/declined; unresolved decisions expire when a Run ends. File hashes
and patch evidence are supplied by the application, which must revalidate
them immediately before performing a side effect. The ledger does not
authorize an operation by itself.

## Ownership and transaction boundaries

`Acquire` takes two nonblocking OS advisory locks: task identity within this
store, and canonical workspace identity across stores. Locks live under the
user's OS cache `meldra/locks/`, so changing `MELDRA_HOME` does not allow a
second writer. Canonicalization resolves workspace symlinks before hashing.
This serializes Meldra executions in the same workspace; it does not prevent
an editor, Git, another program, or a process ignoring advisory locks from
changing files. The application compares before/after evidence on recovery.

The lock descriptors are close-on-exec and held for the Lease lifetime.
They are released by normal close or process death, including SIGKILL. Lock
files are intentionally never removed: removing a locked inode would allow
another process to acquire a different inode for the same path. A PID or stale
lock file is never considered proof that an execution is alive. Unsupported
platforms reject execution ownership instead of falling back to a process-local
mutex. Inspection does not acquire a workspace or task Lease.

All execution mutations require the matching live Lease. Each state change and
its event append share one SQLite transaction. Events are task-local monotonic
sequences, queried by `after` cursor in pages of at most 1,000. The protocol
includes schema version 1, event ID, Run/ToolCall association, status and reason.
Provider-call replay lookup uses a task-scoped expression index and loads at
most one bounded record; it does not decode the whole history for every tool.
The index is rebuildable and added idempotently to existing schema 1 stores;
older binaries can still read those stores. Empty provider IDs are not replay
identities. The query-plan test prevents an accidental full-history scan.

The application commits tool intent before starting the handler and commits
the outcome after completion. SQLite cannot atomically commit an external
command or filesystem edit. Explicit recovery marks formerly planned calls
cancelled and formerly running calls unknown, then marks their Run interrupted.
It runs no tools and expires pending approvals. Normal cancellation also keeps
unrecorded started outcomes unknown rather than asserting that no effect occurred.

## Limits, retention and corruption

- Tool arguments and approval evidence: 1 MiB each.
- Tool result text: 256 KiB; error/reason text: 64 KiB.
- Event data: 64 KiB; results may reference at most 128 artifacts.
- Artifact: 16 MiB each, at most 256 MiB in the artifact directory.
- SQLite: 256 MiB, translated to a page-count limit using the database page size.
- WAL: checkpoint every 256 pages, target retained journal size 4 MiB.

The WAL retention target is not a peak disk-usage guarantee: an in-flight write
or reader can delay checkpointing. Quota failures stop recording/execution
rather than silently dropping earlier evidence. Artifact writes use a private
temporary file, file sync, rename and directory sync. References verify both
length and SHA-256 when read. A crash can leave an unreferenced artifact;
it does not turn an incomplete tool into success. No automatic retention job
deletes execution history in 0.2.0. Back up and archive the entire closed store
when necessary; deleting only the WAL while a database is live is unsafe.

`PRAGMA user_version` is checked before migration or enabling WAL. Schema 0
(an empty pre-ledger database) migrates transactionally to schema 1; repeated
opens leave the schema unchanged. Versions newer than this binary are rejected.
Corrupt JSON records and digest mismatches are reported, not silently repaired.

Legacy JSON sessions are validated by the app and imported transactionally by
source session ID plus SHA-256. Repeated import of an identical source is a
no-op; a changed snapshot is associated with the same task identity rather than
creating duplicate tasks. Original files are retained. The imported record
explicitly marks missing historical tool evidence; no outcomes are fabricated.

## Verification

`go test -race ./internal/task ./internal/store` covers exhaustive Run/tool
transitions, terminal immutability, approval binding and expiry, commit faults
at the intent/start/result/Run boundaries, unknown-outcome gates, idempotent
legacy import, corruption and limits, private paths, cross-store symlink aliases,
actual subprocess lock competition and SIGKILL lock release. The application
tests own signal propagation, file reconciliation and command process groups.

Store benchmarks measure task reads, indexed replay lookup after 1,000 calls,
event append/page, a complete tool
lifecycle, recovery scanning, and large-log artifact deduplication. These are
storage/latency metrics, not model quality or evidence that an unknown external
command can be safely replayed.
