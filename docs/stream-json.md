# Stream JSON protocol

**Unreleased.** The foreground CLI accepts `--output-format text|stream-json`.
The default is `text`. Stream JSON runs exactly one `--prompt`, never reads stdin,
and never enters the TUI, input picker or interactive approval flow.

```sh
meldra --workspace /path/to/project --prompt 'Inspect this project.' --output-format stream-json
meldra task resume TASK_ID --approve-detail-hash HASH
```

Stdout contains only JSON objects, one per line. Human tool previews and error
summaries go to stderr. Stream consumers must read promptly: an output failure
or stalled pipe stops execution. A broken transport may leave an incomplete
last line; use the durable task record to inspect outcomes.

## Version 1 envelope

Every event contains `schema_version: 1`, a strictly increasing process-local
`seq` starting at 1, UTC RFC3339 `time`, `kind`, and object `data`. `task_id`,
`run_id`, and `tool_call_id` are present when available. Tool call IDs are provider
call IDs; lookup the corresponding `provider_call_id` in `task show --json` for
the durable ledger ID. An explicitly approved replay receives a distinct
`approved_<original-ledger-call-id>` identity in its new Run.

| Kind | Data |
| --- | --- |
| `session_start` | `session_id`, `workspace`, `mode`, `permissions` |
| `turn_start` | `turn_id` (Run ID), `index` |
| `assistant_delta` | `text` |
| `assistant_message` | `text` |
| `tool_started` | `tool_call_id`, `name` |
| `tool_finished` | `tool_call_id`, `name`, `status`, `duration_ms`, `truncated`, optional `error` |
| `approval_required` | `tool_call_id`, `operation`, `title`, `detail`, `detail_hash` |
| `notice`, `error` | `text` |
| `metrics` | `input_tokens`, `output_tokens`, `context_bytes` |
| `turn_end` | `status` |
| `session_end` | `status`, `exit_reason` |

Consume either deltas or complete `assistant_message` text for each response;
concatenating both duplicates the answer. Complete messages are emitted even
when deltas were delivered. Several model responses can occur in one turn.
Usage metrics are cumulative for that turn. A tool status describes the tool,
while successful turn completion describes completion of the model interaction;
it does not assert that every proposed tool succeeded. Reasoning and provider
request bodies are excluded from this presentation protocol.

Consumers must ignore unknown kinds. The checked schema snapshot is
`scripts/fixtures/stream-json-v1.json`; internal UI and ledger events are separate
contracts. Startup failures may emit `error` and `session_end` before a session
exists. Usage errors write only a human diagnostic to stderr.

## Approval and recovery

An operation requiring human approval records its pending approval durably, emits
`approval_required`, and stops the tool batch. The call is recorded as cancelled
before side-effect admission, and the Task/Run remain `waiting_approval`. The
process releases its leases and exits **3**. No later calls in that response run.

After reviewing the operation, use the emitted task ID and `detail_hash`:

```sh
meldra task resume TASK_ID --approve-detail-hash HASH
```

This defaults to `stream-json` and resumes in the saved workspace. The hash is
an opaque SHA-256 binding to that pending approval, its original parameter
hash, full preview digest, file/command fingerprints, policy and inspected
project instructions. The new foreground process acquires the same leases,
validates the pending record, recovers the old Run, and re-prepares exactly the
stored operation before any new model request. Its new approval records the
consumed hash and source `detail-hash`. It grants one operation only; further
operations require their own approval. It cannot expand Plan permissions.

A wrong, expired, already used, cross-task or crash-incomplete hash grants
nothing. Changed files, command specification, project instructions or policy
require fresh review. Generic resume without a hash uses existing explicit
recovery, expires old approvals and asks the model to continue; it does not
authorize the old operation. Command approval binds the exact executable,
arguments and execution configuration, not every file arbitrary code might read.
Command execution still uses your OS user privileges.

`--auto-approve` should be used **only in an isolated environment**, such as a
disposable CI container or VM. It is the noninteractive equivalent of approving
eligible operations; it is not an OS sandbox and never expands Plan mode. It
cannot be combined with `--approve-detail-hash`.

Exit codes are **0** for completed execution, **1** for execution/configuration/IO
errors, **2** for invalid stream options, and **3** for required approval. Normal
completion ends with `session_end` status `succeeded`, reason `completed`;
approval uses `waiting_approval`/`approval_required`, errors use `failed`/`error`,
and cancellation uses `cancelled`/`cancelled`.

## Verification

`make check` includes the real-binary stream suite. To repeat it independently:

```sh
CGO_ENABLED=0 go build -o /tmp/meldra-stream .
python3 scripts/capability-e2e.py --binary /tmp/meldra-stream --output /tmp/meldra-stream-evidence --stream-json --only stream-json
```

Use a new output directory. The suite uses isolated homes, fake credentials and
a loopback provider, and preserves stdout/stderr, provider requests, ledger
snapshots, a report, binary checksum and evidence checksums.
