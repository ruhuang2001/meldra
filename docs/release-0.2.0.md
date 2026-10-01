# Meldra 0.2.0 release preparation

Status: unreleased. This document is the release checklist and draft release notes;
it does not claim that an alpha, release candidate, or stable version exists.
The source implementation and release preparation are being assembled for a
pull request. Creating or merging that source PR is separate from publishing a
release. The stable manifest still records the last published `0.1.0`.

## Current implementation status

The working branch now contains SQLite task/run/tool/approval/event records,
cross-process ownership, structured tool outcomes, foreground signal handling,
explicit recovery and resolution commands, and preserved legacy session imports.
The application adapter wires these into the CLI and TUI; there is no daemon.
Context compaction has been optimized, and the runtime/offline evaluation suite
has been extended. Integration and final-candidate verification are still in
progress, so the feature list is not a claim that every release gate has passed.

Local release-validator tests, actionlint and GoReleaser configuration validation
have passed during development. A disposable clean-source fixture exercised
four-platform packaging and Darwin arm64 startup/version checks. Real-process
signal, child-group, PTY-hangup and EOF tests have also passed locally, including
repeated race-enabled runs. These are development evidence; rerun the gates on
the exact final candidate and record the reports below before publication.

Still required for release qualification:

- A clean final candidate, its full test/coverage and versioned offline/performance
  reports, and green remote CI for that commit.
- Native startup/version smoke for all four packaged targets in the prerelease
  workflow. Local cross-compilation alone does not prove the other three native
  startup checks passed.
- The fixed small live-model cohort, using the approved provider/model and an
  enforceable cost budget, followed by independent grading and failure analysis.
  The [live evaluation scaffold](../benchmarks/live/README.md) is preparation;
  it is not a model success-rate result.
- A reviewed prerelease/release decision, frozen CLI/schema and release notes.
  No release tag, GitHub Release or publication is created by this implementation
  work; the source PR is intended to remain unmerged for review.

## Scope

The 0.2.0 theme is **recoverable, inspectable foreground tasks**. A task records
its execution attempts, tool calls, approvals, and events. Exiting the terminal
ends the foreground execution. Starting Meldra again does not schedule old work;
continuation requires an explicit `task resume` command.

The version does not include a daemon, detached execution, automatic worktree
management, or multi-agent scheduling. A completed task means the agent finished
its attempt; review the recorded verification results before trusting a change.

## Upgrade and recovery

Before replacing a 0.1.x binary:

1. Stop all Meldra processes using the same configuration directory. Keep the old
   binary or its download/checksum, and preserve the Git state of your projects.
2. Back up the entire Meldra data directory with private permissions. The default
   is `~/.meldra`; `MELDRA_HOME` overrides it. For example, after all runs stop:

   ```sh
   meldra_data=${MELDRA_HOME:-"$HOME/.meldra"}
   backup_parent=$(mktemp -d "$HOME/meldra-backup.XXXXXX")
   chmod 700 "$backup_parent"
   cp -pR "$meldra_data" "$backup_parent/data"
   ```

   The backup includes credentials and possibly private tool output. Keep it
   private. Do not copy only `tasks.db`: if SQLite WAL/SHM files exist, retain them
   with the database and its artifacts. Copying an active database's files is not
   a supported backup procedure. No automatic downgrade migration is provided.
3. Install the explicitly selected published version and verify `meldra version`.
   For prereleases, use an exact tag rather than the installer's stable `latest`:

   ```sh
   VERSION=v0.2.0-alpha.1 sh scripts/install.sh
   meldra version
   ```

   This command only works after that prerelease is published; draft assets are
   available through the authenticated workflow/release UI instead.
4. Inspect existing sessions and task records before resuming execution:

   ```sh
   meldra sessions
   meldra tasks
   meldra task show TASK_ID
   meldra task events TASK_ID --json
   meldra task resume TASK_ID
   ```

Legacy `meldra resume SESSION_ID` remains the import/compatibility entry point.
Import preserves the original JSON snapshot and records its source ID and content
digest; repeating import does not create another task for the same snapshot.
Old snapshots lack complete historical tool records. Missing records remain
missing; they must not be interpreted as evidence that a tool succeeded.

New task state lives under `MELDRA_HOME/tasks/`: `tasks.db` stores the ledger,
`sessions/` stores the current conversation snapshots, and `artifacts/` stores
bounded retained output by digest. The original `MELDRA_HOME/sessions/` files
remain available for downgrade. Execution lock files live separately in the
user cache's `meldra/locks` directory; never remove held lock files.

Unknown newer schema versions are rejected instead of rewritten. See `task show`
for recorded runs and unfinished operations; `task show TASK_ID --json` includes
structured outcomes and artifact references. Resume creates another run and
preserves previous run outcomes.

`tasks [--json]` lists up to 1000 most recently updated tasks across workspaces;
there is currently no task-list pagination flag. Use a known ID for older records.
`task events TASK_ID --json` emits at most 1000 JSONL events. Pass the last emitted
`sequence` as `--after N` to read the next page, and repeat until empty. Listing
is a snapshot, not a live subscription, and stale running states are only
reconciled by explicit resume/resolution. `sessions` lists usable conversation
snapshots, so it is not a complete inventory of durable tasks.

For a command whose outcome cannot be established, inspect the workspace and
external effects before recording an explicit resolution:

```sh
meldra task resolve TASK_ID CALL_ID --outcome succeeded --reason "verified the recorded command's output"
meldra task resume TASK_ID
```

Choose `failed` when that is the verified outcome. Resolving an unknown result
records a human decision; it is not a sandbox, rollback, or automatic proof.
Never resolve an unknown operation merely to bypass the recovery check.

## Exit and durability limits

Ctrl-C, SIGTERM, terminal hangup, and normal UI exit cancel the foreground run.
The application tries to stop ordinary owned child process groups and save
confirmed results within a bounded shutdown period. SIGKILL, power loss, OS
failure, or a child that escapes its process group can prevent orderly cleanup.

An external command or file write cannot be atomically committed with its
database record. An operation can therefore have happened even if no completion
record exists. Recovery must show that uncertainty, reconcile supported file
changes, and require explicit handling for unresolved effects. It must never
promise exactly-once execution of arbitrary commands.

Task records are local, private execution history, not an encrypted secrets
vault. Command output and tool parameters can contain sensitive user content;
review them before sharing records or benchmark evidence. Configuration snapshots
must omit API keys and raw provider reasoning.

The store caps database pages at 256 MiB and aggregate artifact files at 256 MiB;
an individual artifact may be at most 16 MiB. Commands retain a log of up to
16 MiB separately from their 256 KiB model-facing excerpt; a trailing marker
identifies truncation beyond the log cap. Other tool artifacts retain their
bounded result text. Session snapshots are limited to 2 MiB and 100 messages.
WAL/SHM, snapshots and temporary files are separate from the database page cap.
There is no automatic pruning; hitting a limit stops further recording/execution.
See [storage limits](architecture.md#storage-and-limits) before data maintenance.

## Downgrade to 0.1.x

Stop all Meldra processes first and take another private backup of the 0.2 data.
Reinstall the selected 0.1.x binary and use the preserved pre-upgrade JSON session
snapshot, preferably via a separate `MELDRA_HOME` directory. Avoid overwriting the
0.2 directory while investigating a problem.

0.1.x cannot interpret the new task database, tool/approval history, or recovery
decisions. It can read only the older JSON snapshot. Work performed after that
snapshot will not appear in the downgraded session; repository files and external
command effects are not rolled back by restoring Meldra data. Keep the 0.2 backup
until the upgrade is resolved.

## Alpha and release-candidate channel

Stable Release Please remains on `main`, with `prerelease: false` and the manifest
at the last stable release. Do not put `0.2.0-alpha.N` into that manifest. The
manual **Prepare 0.2 prerelease** workflow is independent of the stable channel:

1. Select an immutable, reviewed full 40-character commit SHA containing this
   workflow, the implementation, and updated release notes. The workflow must be
   available from the repository's default branch before GitHub offers dispatch;
   do not merge a source PR merely to run it without normal review.
2. Supply `0.2.0-alpha.1`, successive `alpha.N`, then optionally `0.2.0-rc.1` and
   successive `rc.N`. Existing fetched tags determine progression. No skipped
   numbers, replaced tags, return from rc to alpha, or prereleases after stable
   0.2.0 are permitted. An untagged draft should be reviewed or deleted before
   preparing another draft with the same name.
3. Leave `create_draft` false to get **workflow artifacts only**. The build has
   read-only repository permissions and creates its version tag only locally.
   GoReleaser's `make check` hook, report tests, release-validator tests, and
   repeated offline scenarios must pass before packaging.
4. All four exact release archives are checksum verified. Their Go build metadata
   must match the reviewed SHA, clean source state, version, OS, architecture,
   and `CGO_ENABLED=0`. Linux amd64/arm64 and Darwin amd64/arm64 runners execute
   their respective packaged binary's `--version`; no rebuilt substitute is used.
   A missing hosted runner is a failed/incomplete gate, not a passed smoke test.
5. Set `create_draft` true only when a draft is wanted. A separate job with
   `contents: write` downloads the already tested artifacts and creates a **draft
   prerelease**. It does not execute candidate code, change the stable manifest,
   or publish the release. Keep the exact workflow run URL as release evidence.
6. Review notes, known limitations, all gates, checksums, and version/source
   identity before manually publishing the draft. Publishing is a distinct
   maintainer action. Once published, do not move its tag or replace its assets.

The workflow intentionally does not use `--snapshot`: prerelease binary versions
must be exactly `0.2.0-alpha.N` or `0.2.0-rc.N`, matching the release tag without
the leading `v`. Installer filenames remain compatible with 0.1.x. Prereleases
are not selected by GitHub's stable `latest` download endpoint.

## Stable 0.2.0 preparation

After the gates below have evidence, let Release Please create/update the stable
release PR. Review that it changes the manifest to `0.2.0`, adds the matching
CHANGELOG entry, and requests tag `v0.2.0`. If automatic version inference differs,
use Release Please's documented `Release-As: 0.2.0` commit footer and review the
resulting release PR; do not manually pretend the version was already released.

Only merge that separate release PR when publication is authorized: the existing
stable workflow creates the release and uploads GoReleaser artifacts. Its repair
dispatch remains available for an existing release's failed artifact upload.
This implementation PR must not merge the release PR, push a release tag, or
publish a stable or prerelease version.

## Required release evidence

Record the exact candidate SHA, command/environment, report paths or workflow
URLs, and the outcome. Replace pending entries only with evidence from that
candidate. Historical benchmark numbers and another commit's CI are not proof.

**Final candidate evidence: pending.** The table specifies the proof still to
attach to the reviewed candidate; it does not mark development tests as release
qualification. Live-model results and remote/native workflow evidence remain
pending until their corresponding runs have actually completed.

| Gate | Evidence required before stable publication |
| --- | --- |
| Go correctness | `make check`: formatting, vet, tidy diff, race tests, >=75% coverage, build |
| Task invariants | Legal state transitions, transactional writes, duplicate ownership, migration and unknown schema rejection |
| Real execution | File/patch/command paths, approval rejection, timeout, storage failure, consistent CLI/TUI states |
| Recovery | Four tool boundaries with interrupted processes; no repeated known effects; user changes preserved; unknown effects visible |
| Terminal exit | Real terminal hangup and signal tests; ordinary child groups exit; forced-termination limitations recorded |
| Upgrade/downgrade | Old JSON fixture, repeat/interrupted import, corrupt/missing/moved workspace; private backup restoration |
| Offline evaluation | All required scenarios pass without hidden skips; versioned JSON report and raw samples |
| Performance | Same-machine comparable baseline/candidate reports, including context compaction and task-store growth |
| Live quality | Fixed small task set, explicitly approved model/provider/cost budget, pass rate/regressions, time and available token/cost data |
| Packaging | Four target archives, native startup/version smoke, checksums, tag/SHA/version consistency |
| Release review | Schema/CLI freeze, reviewed notes and limitations, Release Please manifest/tag/CHANGELOG agreement |

Live model quality is not implied by scripted/offline tests. If a live task set or
budget is unapproved, record it as pending and do not claim release qualification.
Full SWE-bench is optional for the first 0.2.0; the small external evaluation is
still required by the roadmap before stable publication.
