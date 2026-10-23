---
title: "Upgrade and downgrade"
---

## Upgrade and downgrade

Binary-only updates can be installed while sessions are running. Existing
processes keep their current version; newly started processes use the update.
Updating never restarts sessions or hot-loads new code into them.
Saved conversations are not pinned to the executable version that created them.
After updating, exit the running chat normally, then run `meldra resume SESSION_ID`
to continue that conversation with the installed version. From the same workspace,
`meldra resume latest` selects its most recently saved session. You do not need to
start a new conversation.

Stop all Meldra processes before copying data or applying an incompatible data
migration. Back up the entire `MELDRA_HOME` directory (default `~/.meldra`) with
private permissions, including
credentials, legacy snapshots, `tasks/`, and any SQLite WAL/SHM files. Copying only
`tasks.db` from a running process is not a supported backup. Keep the previous
binary alongside the backup. Execution locks in `~/.meldra-locks` and the user cache must not be
removed while held.

**Upgrade from v0.4.0 to v0.4.1:** Run `meldra update`, then restart Meldra
to use the terminal-exit, Skill-warning, and TUI policy-label fixes. This patch
introduces no task-store schema migration.

**Upgrade to v0.4.0 from v0.3.0 and earlier:** these versions do not provide
`meldra update`. Use the installer in the [Quickstart](quickstart.md) or a checksum-verified v0.4.0 platform
archive. Before first opening its ledger, stop all old processes and make the
complete backup described above. The new binary migrates schema 1 to schema 2 when the
ledger is opened. v0.3.0 cannot read schema 2. To roll back, use the previous
binary with the pre-upgrade backup or a separate `MELDRA_HOME`; restoring the
backup loses later task records and does not undo project changes.

**Available since v0.4.0:** Meldra includes the Go `crypto/x509` certificate-chain
security fix tracked by [GO-2026-6809](https://pkg.go.dev/vuln/GO-2026-6809) and
CVE-2026-111943. Restart existing Meldra processes after upgrading so they use
the fixed binary. The inspected v0.3.0 platform binaries were built with Go
1.26.9; all four v0.4.0 binaries were verified to use Go 1.26.10.

**Available since v0.4.0:** The updater and automatic update notices described
below are not available in v0.3.0.

For release-archive installations that already support the updater on macOS or
Linux (amd64/arm64):

```bash
meldra update
```

This checks the latest stable release and installs it immediately if newer,
without confirmation or requiring other sessions to close. If already current
or ahead of the latest release, it leaves the installation unchanged.

**Unreleased:** The update command prints a message before checking for a release,
then reports download, verification, and installation stages. The result includes
both the original and installed versions. These are stage notices, not a byte-level
percentage; they also remain readable when output is redirected. For example:

```text
Checking for updates (current: v0.4.0)...
Updating Meldra: v0.4.0 -> v0.4.1
Downloading checksums...
Downloading meldra_Darwin_arm64.tar.gz...
Verifying download...
Installing update...
Updated Meldra: v0.4.0 -> v0.4.1.
```

The versions above illustrate the new output format; the published v0.4.1 updater
does not yet include these progress messages.

The updater downloads the GitHub release, verifies its archive
against `meldra_checksums.txt`, and atomically replaces the resolved executable
with a synced 0755 binary. User symlinks are preserved. It never migrates or
rewrites the task ledger. Keep your own copy of the old binary; the updater does
not create a rollback copy. SHA-256 checks detect corruption, not a compromised
release publisher. Homebrew, npm, and Nix installations must use their package
manager instead. Builds without a valid SemVer version (including `dev`)
cannot self-update; install a release build first. Known versions are never
downgraded.

Updating does not acquire or remove task/session locks, open SQLite, or migrate
stored data. New and old releases may share data only while their storage
contracts remain compatible; this is not a guarantee across incompatible
schema changes. Unknown newer database schemas remain rejected.

Interactive chat sessions show a notice when a newer stable version is known.
Checks use a private `MELDRA_HOME/update-check.json` cache for 24 hours, refresh
in the background with a three-second timeout, and silently ignore failures.
Set `MELDRA_DISABLE_UPDATE_CHECK=1` to disable automatic checks. Non-chat commands,
piped input/output, and `--prompt` sessions never start automatic checks. Explicit
`meldra update` still works when automatic checks are disabled.

After installing the selected version, check `meldra version` in a new process.
Existing sessions can continue. Inspect `tasks` and `task show` before explicitly
resuming a closed session. Downgrading to 0.1.x can read only the
preserved legacy snapshots; it cannot read task history or undo repository
changes. Prefer a separate data directory when downgrading, and keep your data
backup. Unknown newer database schemas are rejected, not rewritten.

## Verify updater changes

**Unreleased:** From the source checkout, run the updater E2E suite with a new
output directory:

```bash
python3 scripts/update-e2e.py --output .artifacts/checks/update-e2e-run
```

It builds two version-labeled copies of the current source and uses loopback
release and model fixtures. It checks progress before delayed responses in pipe
and PTY modes, replacement and failure behavior, and saved-session continuity.
The output retains `report.json`, transcripts, binary/source hashes, and a
`replay.sh` command. It does not download published releases or certify arbitrary
cross-version storage compatibility. Run `make check` for the full repository gates.
