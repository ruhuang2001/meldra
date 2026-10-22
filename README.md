# Meldra

<p align="center">
  <img src="docs/assets/icon.png" alt="Meldra" width="160">
</p>

<p align="center">
  <strong>A local, foreground command-line coding agent with explicit safety boundaries and durable execution records.</strong>
</p>

<p align="center">
  <a href="https://github.com/ruhuang2001/meldra/actions/workflows/ci.yml"><img src="https://github.com/ruhuang2001/meldra/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="https://github.com/ruhuang2001/meldra/releases/latest"><img src="https://img.shields.io/github/v/release/ruhuang2001/meldra" alt="Latest Release"></a>
  <a href="LICENSE"><img src="https://img.shields.io/github/license/ruhuang2001/meldra" alt="License"></a>
</p>

Inspired by Amp's article [How to Build an Agent](https://ampcode.com/notes/how-to-build-an-agent).

> **Early stage:** Meldra is experimental. Review every change it makes and avoid running it in directories with sensitive or irreplaceable files.

## Highlights

- **Interactive TUI & CLI fallback** — Full-screen streaming terminal UI in supported terminals, with automatic fallback to a line-based interface for pipes and scripts.
- **Durable task ledger & explicit recovery** — SQLite-backed task, run, tool, and approval records allow safe inspection and explicit reconciliation after crashes or interruptions.
- **MCP client & server** — Connect to external Model Context Protocol servers over stdio or Streamable HTTP (with OAuth, sampling, and elicitation), or expose Meldra's workspace tools via `meldra mcp serve`.
- **On-demand Skills** — Discover local and user `SKILL.md` packages with progressive loading and strict path confinement.

**Available since v0.4.0:** The published platform archives include:

- Scoped project context and file references, including `AGENTS.md` rules and
  `@path:start-end` references.
- Plan and Build modes, foreground steering and queue controls, and managed
  process records and recovery.
- `meldra update` and cached release notices.

## Requirements

- macOS or Linux (`amd64` / `arm64`)
- An OpenAI API key (or a compatible Responses API endpoint)

## Quick start

### 1. Install

Install the latest release with the official installer script (verifies SHA-256 checksums and installs to `~/.local/bin` by default):

```bash
curl -fsSL https://raw.githubusercontent.com/ruhuang2001/meldra/main/scripts/install.sh | sh
```

Or download the archive for your platform manually from [GitHub Releases](https://github.com/ruhuang2001/meldra/releases):

```bash
# example: macOS arm64
curl -fsSL -o meldra.tar.gz https://github.com/ruhuang2001/meldra/releases/latest/download/meldra_Darwin_arm64.tar.gz
tar -xzf meldra.tar.gz
cp meldra ~/.local/bin/
```

### 2. Configure and run

```bash
meldra config init
vi ~/.meldra/credentials.env   # add your OPENAI_API_KEY
cd /path/to/project
meldra
```

Press `Ctrl-C` to exit.

In a supported interactive terminal, Meldra opens a full-screen TUI and streams assistant responses. Non-interactive and unsupported terminals use the line-based interface.

## Project context and task control

Meldra loads scoped `AGENTS.md` instructions and accepts explicit file references.
Use Plan mode to inspect a project before allowing implementation:

```sh
meldra --mode plan --prompt 'Review @src/main.go:10-40 and propose a change.'
meldra context --path src/main.go --json
meldra --permissions workspace-edit --command-timeout 900
```

Plan mode permits reads and saved plans/summaries while blocking project writes and
program execution, including with `--auto-approve`. It does not start configured
external MCP servers. The `workspace-edit` permission profile grants checked file
edits while retaining confirmation for programs that execute project code.

Use `/plan` and `/build` to change mode, `/steer TEXT` to correct active work,
`/queue TEXT` to save a follow-up, and `/stop` to stop the current turn without
quitting the application. Commands stream output and can run with configurable
timeouts; managed process handles remain owned by the current Run.

See [Project context, execution modes, and foreground control](docs/capabilities.md)
for reference syntax, limits, permission boundaries, and recovery behavior.

## Configuration

```text
~/.meldra/config.toml       # model, provider URL, and limits
~/.meldra/credentials.env   # OPENAI_API_KEY
~/.meldra/mcp.json          # optional trusted MCP server configuration
~/.meldra/update-check.json # cached 24-hour stable release check metadata
~/.meldra/sessions/         # preserved legacy 0.1.x session snapshots
~/.meldra/tasks/tasks.db    # versioned task, run, tool, process, approval, and event records
~/.meldra/tasks/sessions/   # current conversation snapshots
~/.meldra/tasks/artifacts/  # bounded tool/verification output, named by digest
```

Set `MELDRA_HOME` to use a different directory. Configuration precedence:

1. `OPENAI_API_KEY`, `OPENAI_MODEL`, `OPENAI_BASE_URL` environment variables
2. Files under `~/.meldra`
3. Built-in defaults

Provider responses are capped at 32 MiB by default. Set
`max_provider_response_bytes` in `config.toml`, or
`MELDRA_MAX_PROVIDER_RESPONSE_BYTES` for a process override, to choose a
positive limit up to 256 MiB.

Custom-provider replay context is capped at 4 MiB by default; older tool outputs
are compacted first when needed, and if a turn still exceeds the budget, Meldra
saves the session and pauses so you can send `continue`. Set
`max_custom_turn_input_bytes` or `MELDRA_MAX_CUSTOM_TURN_INPUT_BYTES` to choose
a positive limit up to 64 MiB.

Custom provider URLs must use HTTPS. For local development only, an HTTP URL
on `localhost` or another loopback address can be enabled explicitly:

```toml
base_url = "http://localhost:8080/v1"
allow_insecure_base_url = true
```

```bash
meldra config init   # create starter config files
meldra config        # show effective values and config paths (hides API key)
meldra version
```

## MCP

Meldra connects to trusted MCP servers over stdio or Streamable HTTP. Add
servers to `~/.meldra/mcp.json` (or `$MELDRA_HOME/mcp.json`), with a `0700`
directory and a `0600` file. It supports tool calls, resource and prompt
catalog access, OAuth login, text sampling, and CLI/TUI form or URL elicitation.

```bash
meldra mcp login SERVER                # OAuth servers configured in mcp.json
meldra mcp logout SERVER               # remove local OAuth credentials
meldra mcp resources SERVER            # inspect one page of resources
meldra mcp prompts SERVER              # inspect one page of prompt templates
meldra mcp prompt SERVER NAME KEY=VALUE --run  # choose and confirm a workflow
meldra mcp serve --workspace /path/to/project  # expose Meldra over MCP stdio
```

MCP clients can request Meldra's workspace tools, resources, and review prompt.
Required approvals travel through the client using elicitation. Text sampling
is opt-in per server and requires explicit consent before calling the model
and before sharing its result; `--auto-approve` does not bypass this consent.

See [MCP Support](docs/mcp.md) and
[MCP Authentication and Server Interactions](docs/mcp-auth-interactions.md).
These are implemented capability subsets, not a claim of full MCP specification
conformance. External MCP servers use their own privileges and are not confined
by Meldra's file-tool boundary.

## Skills

Meldra discovers direct `<name>/SKILL.md` packages in these locations, in
priority order:

1. Workspace `.meldra/skills`
2. Workspace `.agents/skills`
3. `$MELDRA_HOME/skills` (default `~/.meldra/skills`)
4. `~/.agents/skills`

Duplicate names keep the first valid package and produce a warning.

```bash
meldra skills --workspace /path/to/project --json
meldra --workspace /path/to/project --prompt 'Use $review-checklist to review this change.'
```

Each package requires YAML `name` and `description` fields followed by Markdown
instructions. The initial model request includes only catalog metadata; the
`read_skill` tool loads instructions and relative text resources on demand.
`$name` is a prompt convention that asks the model to load the skill, not a
frontend slash command. Restart or resume to discover changed metadata.

Skill content cannot grant permissions: scripts still use approved workspace
tools. User packages are read-only through `read_skill`; configuration, task
storage, execution locks, symlinks, path traversal, and non-text resources remain
blocked. Only install skills whose contents you have reviewed.

See [Skills](docs/skills.md) for comparison with other agents, exact limits,
compatibility boundaries, and repeatable E2E verification.

## Safety

- File tools are constrained to the workspace root (resolves `..`, symlinks, absolute paths) and block access to `.git` and Meldra's own config/session directory.
- Every edit prints a diff. The default `interactive` policy waits for confirmation; `--permissions workspace-edit` grants checked built-in file edits and records that policy decision.
- Command execution is allowlisted. Commands that compile or execute workspace code require explicit approval; restricted read-only Git commands and `gofmt -d` do not. Approved commands run as your OS user and may access the filesystem and network; environment filtering is not a sandbox. Use `--auto-approve` only inside an isolated container or VM.
- The `verify` tool detects root project markers and selects bounded presets for Make, Go, Python/pytest, Node/npm or pnpm, and Rust/Cargo projects. A Makefile's explicit `check` or `test` target takes priority, and the complete command plan is approved once before execution.
- Custom providers have a 4 MiB default replay-context budget (configurable up to 64 MiB); older tool results are compacted first when needed.
- Session files are bounded and validated while loading; conflicting saves from another process are rejected instead of silently overwriting newer state. Task execution also holds advisory locks for the task and canonical workspace, preventing concurrent Meldra writers in the same workspace across configurations. Persistent ownership lives in `~/.meldra-locks`; cache locks remain for older-build compatibility. These locks do not block your editor or other programs. The TUI keeps at most 200 rendered history entries in memory without applying that display limit to session persistence.
- Ordinary repository contents and tool output are untrusted data. Scoped `AGENTS.md` files and explicitly loaded Skills provide guidance subordinate to user instructions and runtime permissions.

## Tasks and recovery

The first request records a task ID. Each attempt has a separate Run; tool
intent, outcome, approvals, and events are recorded around execution. A completed
task means the attempt finished; review its verification results separately.

```bash
meldra tasks
meldra tasks --json
meldra task show TASK_ID
meldra task show TASK_ID --json       # includes structured results and artifact references
meldra task events TASK_ID --json
meldra task events TASK_ID --json --after 1000
meldra task resume TASK_ID
```

Inspection requires no model credentials and starts no model or tool execution.
`tasks` lists up to 1000 most recently updated records across workspaces; it is
not a complete export when there are more records. `task show` inspects a known
ID. `task events` emits up to 1000 JSON objects, one per line, in sequence order.
Use the last returned `sequence` as `--after` for the next page; repeat until
empty to read the available history. These commands return snapshots, not a live
subscription. A recorded `running` state can be stale after a crash.

**Closing the terminal ends execution.** `Ctrl-C`, `SIGTERM`, and `SIGHUP` cancel the
model request and attempt to stop owned command process groups and save confirmed
outcomes. There is no daemon, detach mode, or automatic restart. EOF after a
`--prompt` is normal input completion; it does not cancel that prompt early.

After an exit, only an explicit resume continues a task. Recovery reconciles supported file
operations using recorded before/after fingerprints. Unknown command effects or
files changed since the recorded operation block further execution. After
inspecting the actual effects, record a resolution and resume:

```bash
meldra task resolve TASK_ID CALL_ID --outcome succeeded --reason "verified command output and files"
meldra task resolve-process TASK_ID PROCESS_ID --reason "verified process effects"
meldra task resume TASK_ID
```

Use `--outcome failed` when that is the verified result. Resolution records your
decision and runs no tools. It does not undo filesystem changes or external
effects. `SIGKILL`, power loss, and processes that escape their process group can
leave unknown outcomes; arbitrary commands do not have an exactly-once guarantee.

Task history is private local data, not an encrypted secrets vault. Output and
artifact limits are described in [Architecture](docs/architecture.md#storage-and-limits).
Command artifacts retain up to 16 MiB of output separately from the 256 KiB
model-facing excerpt, with a marker when the log itself is truncated. Other
tool artifacts retain their bounded output. No automatic history pruning is
performed.

## Existing sessions

```bash
meldra sessions
meldra resume                 # choose a saved session
meldra resume SESSION_ID      # resume a specific session
meldra resume latest          # latest usable session for the current workspace
```

The picker and `latest` are scoped to the current workspace unless
`--workspace PATH` is supplied. Explicit session IDs use their saved workspace.
Session lists include preserved legacy JSON and current task snapshots, prefer
current snapshots with the same ID, and omit empty/invalid snapshots with a
diagnostic for invalid files. A session listing is not the task ledger: a task
can have execution records even when its conversation snapshot is unavailable.

Resuming a legacy session imports its source ID and content digest without
rewriting the original JSON. New messages go to `tasks/sessions/`. Historical
tool outcomes that 0.1.x never recorded remain missing.

## Upgrade and downgrade

Binary-only updates can be installed while sessions are running. Existing
processes keep their current version; newly started processes use the update.
Updating never restarts sessions or hot-loads new code into them.

Stop all Meldra processes before copying data or applying an incompatible data
migration. Back up the entire `MELDRA_HOME` directory (default `~/.meldra`) with
private permissions, including
credentials, legacy snapshots, `tasks/`, and any SQLite WAL/SHM files. Copying only
`tasks.db` from a running process is not a supported backup. Keep the previous
binary alongside the backup. Execution locks in `~/.meldra-locks` and the user cache must not be
removed while held.

**Upgrade to v0.4.0 from v0.3.0 and earlier:** these versions do not provide
`meldra update`. Use the installer above or a checksum-verified v0.4.0 platform
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

## Documentation

The documentation site uses [Mintlify](https://www.mintlify.com/) with `docs/` as
its content root. Start with the [Quickstart](docs/quickstart.md); see
[Maintaining the documentation](docs/documentation.md) for local preview,
validation, and hosting setup. Existing Markdown guides remain readable here:

- [Changelog](docs/changelog.mdx) — User-facing release highlights and upgrade notes; [CHANGELOG.md](CHANGELOG.md) retains the generated technical history.
- [Capabilities & Foreground Controls](docs/capabilities.md) — `AGENTS.md` project context, `@file` references, Plan/Build modes, managed processes, and interactive controls.
- [MCP Support](docs/mcp.md) — MCP client/server configuration, catalog CLI, workspace server (`meldra mcp serve`), and boundaries.
- [MCP Authentication & Interactions](docs/mcp-auth-interactions.md) — OAuth login/refresh, opt-in text sampling, and CLI/TUI elicitation.
- [Skills](docs/skills.md) — `SKILL.md` package format, discovery precedence, progressive loading, and security boundaries.
- [Architecture & Runtime Boundaries](docs/architecture.md) — Package layout, execution lifecycle, recovery rules, and storage limits.
- [Task Storage Contract](docs/task-storage.md) — SQLite ledger schema, state transitions, advisory lock ownership, and artifact retention.

## Development

Requires Go 1.26.10. Python 3 is required for the real-terminal integration
tests; building the binary itself requires only Go.

```bash
make check           # formatting, vet, modules, race tests, E2E suites, coverage, and build
make install-hooks   # enable .githooks for pre-commit and pre-push checks
make check-staged    # quick checks of staged whitespace and Go formatting
make check-local-ci  # full make check in a temporary copy of the index
```

The test suite must maintain at least 75% statement coverage.

The Git hooks run quick staged checks before each commit and full local checks
before pushing. The push check uses the exact committed revisions being pushed;
the manual `make check-local-ci` command uses staged contents, so stage new or
modified files first. Temporary checks exclude untracked projects and do not
rewrite working files. Full local checks time out after 15 minutes and stop
their process group; set `MELDRA_LOCAL_CI_TIMEOUT` to a positive number of
seconds to adjust this limit.

Additional manual E2E suites:

```bash
python3 scripts/skills-e2e.py --output dist/skills-e2e-new-run
```

Choose a new output directory for each run.

Remote CI uses one Linux job for race tests, the build, and a vulnerability
scan (`make check-ci` plus `govulncheck`). Formatting, dependency tidiness,
standalone vet, and coverage remain in full local checks; `go test` also runs its
default vet checks remotely. Core CI runs for Go, module, and check configuration
changes, skips draft PRs and documentation-only changes, and runs when a draft
is marked ready for review.

The four native platform checks run manually through **Actions → Platform
smoke → Run workflow** before a release or when investigating platform behavior.
Select one platform for a focused check (the default is `linux-amd64`), or choose
`all` to verify all four release platforms.

### PR test binaries & releases

PR checks do not build downloadable artifacts by default. To request one from a
PR, an owner or collaborator comments `/meldra package`. GitHub Actions
acknowledges the request, builds the PR's merge result, and replies with a link
to a seven-day artifact containing the Darwin/Linux `amd64` and `arm64`
archives and checksums. The **Build PR test artifacts** workflow is also
available under **Actions** as a manual fallback.

Meldra follows Semantic Versioning. Release Please prepares a release PR
against `main`; review its version, manifest, and [CHANGELOG](CHANGELOG.md)
before merging.

## License

[MIT](LICENSE)
