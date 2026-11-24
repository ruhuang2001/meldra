---
title: "Run commands"
description: "How Meldra runs programs, manages long-running processes, and lets you inspect what an interrupted command did."
---

Meldra runs a program only after you approve it. Commands run with an executable
and argument array — no shell expansion, no pipes, no redirection — as your OS
user. The default allowlist covers common read-only tools; approve anything else
explicitly.

```sh wrap
meldra --command-timeout 900
meldra --allow-executable /absolute/path/to/tool
```

The command timeout defaults to 600 seconds and accepts 1 to 86400 seconds. A tool
call can request a shorter timeout. A quiet process is not stopped just because it
prints nothing: output streams to the interface and is retained within bounded
excerpts and artifacts.

`--allow-executable` adds an absolute executable identity that may be approved for
this session. It does not approve execution by itself, remove path checks, or
create a sandbox. Pass it more than once to allow several executables.

## Long-running processes

The agent drives long-running work — a dev server, a watch task — through four
tools: `start_process`, `process_status`, `wait_process`, and `stop_process`.
Starting a process successfully does not mean the command succeeded.

Status reports lifecycle state, exit information, effect certainty, bounded
output, and a byte cursor. The agent must wait for or stop a process before
finishing its run. Only one potentially writing command runs at a time; while it
runs, other commands and built-in file edits are blocked.

Processes do not survive run completion, terminal closure, or a new chat turn.
Handles are not exposed through `mcp serve`, whose tool requests have separate
runs.

## After an interrupted command

A program can be interrupted and still leave effects behind. Inspect before you
resolve:

```sh wrap
meldra task show TASK_ID --json
meldra task resolve-process TASK_ID PROCESS_ID --reason 'Inspected the command effects.'
```

Recovery never restarts an unfinished process or signals a PID from an old record.
Closing the terminal stops execution, so there is no background daemon. SIGKILL,
power loss, and processes that escape their process group can still leave an
unknown outcome; those need your own inspection. See
[task records and recovery](../reference/task-records.md).

## Git checkpoints

<Note>
**Unreleased:** `git_commit` is on the development branch and is not in the latest
published release.
</Note>

`git_commit(paths, message)` commits an explicit list of up to 32
workspace-relative files. It shows `git_review` evidence plus the selected diff
and new text-file contents through the normal command approval path. Plan mode
denies it, and the `workspace-edit` profile alone does not approve it.
`run_command` still allows only read-only Git operations.

It requires an existing initial commit, a clean staging area, and repository-local
`user.name` and `user.email`. Modified, deleted, and new text files are supported;
directories, symlinks, protected paths, new binary files, and repositories with
configured Git filters are rejected. Each file is bounded by the edit limit and
preview evidence is capped at 1 MiB. Existing staged work is never included
silently, and hooks and signing stay disabled.

HEAD, index, and file fingerprints are checked again after approval, then staged
blobs are checked before committing. A failure can leave the selected files
staged; inspect `git_review` before retrying. This is not automatic committing or
pushing, and it does not extend `undo_last_change`.
