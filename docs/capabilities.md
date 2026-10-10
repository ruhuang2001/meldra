# Project context, execution modes, and foreground control

These capabilities are part of the 0.2.x foreground runtime. Use a binary built
from this source tree; older published binaries may not expose these flags.

## Project instructions

Meldra reads `AGENTS.md` from the canonical workspace root before requesting a
model response. When a file is accessed, it also discovers `AGENTS.md` files in
that file's ancestor directories inside the workspace. Root instructions apply
throughout the workspace; nested instructions apply only to their directory and
its descendants. Deeper instructions take precedence within that scope.

```sh
meldra context --workspace /path/to/project --path src/main.go
meldra context --workspace /path/to/project --path src/main.go --json
```

The context inspector shows the source path, scope, contents, and digest. It
requires no model request. Parent directories outside the workspace, recursive
includes, and alternate instruction filenames are not loaded.
Within an idle chat, `/context src/main.go` shows the applicable sources without
submitting another model request. JSON inspection includes the rule contents;
the human-readable view lists paths, scopes, and digests.

If an edit discovers instructions the model has not yet seen, that operation is
rejected and the model must reconsider it with the new context. A changed
instruction digest invalidates a pending operation. This also covers files
created by a patch. Rules never grant runtime permissions or authorize their own
edits. Arbitrary programs may affect files outside their working directory, so
per-file instruction discovery cannot determine a program's complete write set.

Applicable instruction files must be regular UTF-8 text files without symbolic
links. Each file is limited to 32 KiB. The active project-instruction payload is
limited to 128 KiB, including its explanatory prefix, JSON-encoded paths, scopes,
digests, and escaped content. Invalid or oversized instructions produce a visible
error before they are acknowledged or sent to the model.

## Explicit file references

Reference a file in a direct user message:

```text
Review @src/main.go
Compare @src/main.go:10-40 with @src/util.go:1-20
Read @"path with spaces/example.txt":2-5
```

In the TUI, press `Ctrl-F` to choose a file by name. Type to filter, use the
arrow keys to select, press Enter to insert a quoted reference, or Escape to
return to the original draft. The picker reads filenames only and is unavailable
while an approval or MCP form owns input.

Ranges use one-based, inclusive line numbers. Quote paths containing spaces;
Unicode paths are supported. Escape a literal reference as `\@example.txt`.
Email addresses and references inside inline code or fenced code blocks remain
ordinary text. Missing files, invalid ranges, protected paths, and non-text
references are rejected before submission to the model.

References save the original request, path, range, digest, and a content snapshot
in local task artifacts. A resumed task can identify the submitted snapshot even
when the current file has changed. A reference is quoted file data; it cannot
grant instructions or permissions. Only the dedicated `AGENTS.md` loader assigns
project-instruction meaning. References embedded in a remote MCP prompt do not
automatically attach local files.

A source file may be at most 1 MiB. Retained content is bounded to 64 KiB per
reference and 128 KiB per request, with at most 32 references. Truncation is
marked; use narrower line ranges for large files. Snapshots remain private local
history and are subject to task storage limits.

## Plan and Build modes

```sh
meldra --mode plan --prompt 'Inspect the project and propose a change.'
meldra --mode build --permissions workspace-edit
meldra mcp serve --workspace /path/to/project --mode plan
```

| Mode | Allowed work |
| --- | --- |
| `plan` | Read and search workspace files, inspect diffs, and save the task's plan or summary. |
| `build` | Use available tools within the selected permission policy. |

Plan denies workspace edits, undo, tests, builds, formatting, process creation,
and unclassified external effects at runtime. `--auto-approve`, Skills, project
instructions, and MCP `readOnlyHint` annotations cannot override that boundary.
Plan is initially local-only: starting a Plan session does not connect configured
external MCP servers. A mode change requires a user action.

Mode and permission are separate. `interactive`, the default permission policy,
requires confirmation for file modifications and programs that execute project
code. `--permissions workspace-edit` grants the checked built-in file operations;
it still requires confirmation for project programs and unknown remote effects.
An automatic policy grant is recorded distinctly from a human approval.

Approved programs run as the host OS user. Executable restrictions and filtered
environment variables are not an OS sandbox. Use `--auto-approve` only in an
isolated environment; it never expands Plan permissions.

## Commands and managed processes

```sh
meldra --command-timeout 900
meldra --allow-executable /absolute/path/to/tool
```

The command timeout defaults to 600 seconds and may be configured from 1 to
86400 seconds. A tool call can request a bounded timeout. A quiet process is not
stopped merely because it produces no output. Output streams to the interface
and is retained within bounded excerpts and artifacts.

Execution uses an executable and argument array without shell expansion. The
default command allowlist remains available; repeat `--allow-executable` to
authorize additional absolute executable identities for approval. This flag does
not itself approve execution, remove path checks, or create a sandbox.

The agent can use `start_process`, `process_status`, `wait_process`, and
`stop_process` to manage a command within its current Run. Starting successfully
does not imply successful command completion. Status reports lifecycle state,
exit information, effect certainty, bounded output, and a byte cursor. The agent
must wait for or stop a process before finishing its Run.

Only one potentially writing command may run at a time. While it is running,
other commands and built-in file modifications are blocked. Processes do not
survive Run completion, terminal closure, or a new chat turn. Managed handles are
not exposed through `mcp serve`, whose tool requests have separate Runs.

An interrupted program can leave unknown effects even when its process is known
to have stopped. Inspect those effects before resolving the process record:

```sh
meldra task show TASK_ID --json
meldra task resolve-process TASK_ID PROCESS_ID --reason 'Inspected the command effects.'
```

Recovery never restarts an unfinished process or signals a PID from an old
record. Closing the terminal stops execution; there is no background daemon.
SIGKILL, power loss, and processes that escape their process group can still
leave unresolved outcomes.

## Control during a task

The same commands are available in the TUI and line interface:

| Input | Action |
| --- | --- |
| `/plan` | Enter Plan after incompatible active work has settled. |
| `/build` | Enter Build within the current permission profile. |
| `/stop` | Stop the current turn while keeping the application open. |
| `/steer correction` | Apply a correction to the active task. |
| `/queue request` | Save a separate follow-up request for after the current turn. |
| `/continue-queue` | Explicitly apply queued requests recovered after a restart. |

Ordinary text submitted while the TUI is busy acts as steering. In the line
interface, use `/steer` explicitly; ordinary piped lines retain their FIFO order.
A correction cancels active inference, prevents unstarted calls from its old
response, and starts a new response with the correction and known results. An admitted atomic file write
may finish before the correction applies. A normal correction waits for an
active command's safe boundary; use `/stop` to cancel that command.

Controls are acknowledged only after their received record is saved. Received
does not mean applied: stopping owned processes and reconciling outcomes may
take additional time. Invalidated approval prompts cannot authorize replacement
operations. Queued requests retain their own identities even when their text is
identical. After a crash, pending requests are shown on resume but are not
automatically executed. Use `/continue-queue` to apply them; opening history or
submitting another ordinary resume prompt leaves that recovered queue pending.

`Ctrl-C` still exits the application. `/stop` preserves the chat for another
request. In the TUI, `Ctrl-X` also requests a turn stop, including while a form or
approval owns the input. MCP forms and approvals keep their own input ownership; a form answer
is not interpreted as an ordinary task message.
Press `Ctrl-G` in the TUI to compose a correction while an approval or MCP form
is pending. `Escape` returns to that interaction without submitting a correction.

## Local verification

The offline capability suite uses a real binary, isolated directories, and a
scripted loopback Responses server. It does not contact a real model or use
configured credentials:

```sh
go build -o dist/meldra .
python3 scripts/capability-e2e.py --binary dist/meldra --output dist/capability-e2e
python3 scripts/capability-e2e.py --binary dist/meldra --output dist/capability-controls --controls --processes
python3 scripts/capability-e2e.py --binary dist/meldra --output dist/capability-long --long-command --only long-command
```

Each run requires a fresh output directory and retains transcripts, provider
requests, task records, and checksummed reports. The long-command check really
runs longer than 120 seconds and is manual. Deterministic runtime tests establish
permission and lifecycle contracts; this suite makes no claim about real-model
task completion quality.
