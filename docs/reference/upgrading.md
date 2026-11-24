---
title: "Upgrade and downgrade"
description: "Self-update a release build, back up state safely, and know what a rollback can and cannot recover."
---

## Update

```sh wrap
meldra update
```

The updater checks the latest stable release and installs it if it is newer,
without confirmation and without asking running sessions to close. If you are
already current, nothing changes.

Installing is safe while sessions run. Existing processes keep their version;
newly started processes use the new binary. An update never restarts a session or
hot-loads new code into it.

```
Checking for updates (current: v0.4.0)...
Updating Meldra: v0.4.0 -> v0.4.1
Downloading checksums...
Downloading meldra_Darwin_arm64.tar.gz...
Verifying download...
Installing update...
Updated Meldra: v0.4.0 -> v0.4.1.
```

These are stage notices, not a percentage, and they survive redirected output.

The updater downloads the GitHub release, verifies the archive against
`meldra_checksums.txt`, and atomically replaces the resolved executable with a
synced `0755` binary. User symlinks are preserved.

What it does not do:

- It never migrates or rewrites the task ledger.
- It does not keep a rollback copy — save the old binary yourself.
- It does not downgrade: a known newer version stays installed.
- It does not update Homebrew, npm, or Nix installations; use the package manager.
- Builds without a valid SemVer version, including `dev`, cannot self-update.

SHA-256 checks detect a corrupted download, not a compromised publisher.

### Continue a conversation after updating

Saved conversations are not pinned to the binary that created them. Exit the
running chat normally, then resume:

```sh wrap
meldra resume SESSION_ID
meldra resume latest      # most recent session in this workspace
```

### Automatic update notices

Interactive chat shows a notice when a newer stable version is known. Checks use a
private `MELDRA_HOME/update-check.json` cache for 24 hours, refresh in the
background with a three-second timeout, and silently ignore failures. Set
`MELDRA_DISABLE_UPDATE_CHECK=1` to turn off automatic checks; `meldra update`
still works. Non-chat commands, piped input and output, and `--prompt` sessions
never start a check.

## Before a risky upgrade

Stop every Meldra process before copying data or applying an incompatible data
migration. Back up the whole `MELDRA_HOME` directory (default `~/.meldra`) with
private permissions, including `credentials.env`, legacy snapshots, `tasks/`, and
any SQLite WAL/SHM files. Copying only `tasks.db` from a running process is not a
supported backup. Keep the previous binary next to the backup.

Do not remove execution locks in `~/.meldra-locks` or the user cache while they
are held.

## Version history

<AccordionGroup>
  <Accordion title="v0.5.0">
    Run `meldra update`, then restart Meldra to use context compaction, web tools,
    stream JSON output, custom slash commands, image references, regex
    `search_files`, and session titles. No task-store migration: the ledger
    remains schema 2.

    Two behavior changes to know before upgrading:

    - **Automatic compaction is on by default.** At 80% of the configured byte
      budget, Meldra asks the configured model to summarize older messages. Set
      `compact_threshold_percent = 0` to disable it; `/compact [focus]` stays
      available. See [context compaction](../guides/context-compaction.md).
    - **External reads need approval.** `web_fetch` and optional `web_search` are
      recorded as `external_read` and ask before dispatch, including in Plan mode.
      `--auto-approve` also approves network disclosure and billing.

    Downgrading keeps the ledger readable, but v0.4.2 cannot read v0.5.0 session
    snapshots: it reports `skipped 1 unreadable or invalid saved session file`
    and lists no sessions. The snapshot files are not deleted. The ledger,
    `task show`, `task show --json`, and `task events` remain readable, including
    `external_read` records. If you need an older conversation after a downgrade,
    restore its pre-upgrade backup.
  </Accordion>
  <Accordion title="v0.4.2">
    Run `meldra update`, then restart Meldra to use the self-update stage notices
    and clearer version output. The old updater performs this upgrade, so the new
    progress output appears from the next invocation. No task-store migration.
  </Accordion>
  <Accordion title="v0.4.1">
    Run `meldra update`, then restart Meldra to use the clean terminal-exit fix,
    quieter Skills warnings, and corrected TUI policy labels. No task-store
    migration.
  </Accordion>
  <Accordion title="v0.4.0">
    Includes the Go `crypto/x509` certificate-chain fix tracked by
    [GO-2026-6809](https://pkg.go.dev/vuln/GO-2026-6809) and CVE-2026-111943.
    Restart existing processes after upgrading so they use the fixed binary.
    The inspected v0.3.0 binaries were built with Go 1.26.9; all four v0.4.0
    binaries were verified to use Go 1.26.10.
  </Accordion>
  <Accordion title="v0.3.0 and earlier">
    These versions have no `meldra update`. Use the installer in the
    [Quickstart](../quickstart.md) or a checksum-verified platform archive. Before
    first opening the new ledger, stop all old processes and make the backup
    described above: v0.4.0 migrates schema 1 to schema 2 on open, and v0.3.0
    cannot read schema 2. To roll back, use the previous binary with the
    pre-upgrade backup or a separate `MELDRA_HOME`. Restoring the backup loses
    later task records and does not undo project changes.
  </Accordion>
</AccordionGroup>

## Downgrade

Downgrading to 0.1.x reads only the preserved legacy snapshots; it cannot read
task history or undo repository changes. Prefer a separate `MELDRA_HOME` when
downgrading, and keep the backup. Unknown newer database schemas are rejected
rather than rewritten.

After any version change, check the running binary in a new process with
`meldra version`. Inspect `meldra tasks` and `meldra task show` before you
explicitly resume a closed conversation.

Updating never acquires or removes task and session locks, opens SQLite, or
migrates data. Two releases may share data only while their storage contracts
stay compatible, which incompatible schema changes break by design.
