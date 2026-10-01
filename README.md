# Meldra

Meldra is a command-line coding agent.

Inspired by Amp's article [How to Build an Agent](https://ampcode.com/notes/how-to-build-an-agent).

> **Early stage:** Meldra is experimental. Review every change it makes and avoid running it in directories with sensitive or irreplaceable files.

This branch implements the upcoming **0.2.0 foreground task runtime**. It is
under integration and release validation; the latest published binary may not
include the task commands below. See [release preparation](docs/release-0.2.0.md)
for the remaining gates and upgrade instructions.

## Requirements

- macOS or Linux (amd64 / arm64)
- An OpenAI API key

## Quick start

1. Download the archive for your platform from [GitHub Releases](https://github.com/ruhuang2001/meldra/releases):

   ```bash
   # example: macOS arm64
   curl -fsSL -o meldra.tar.gz https://github.com/ruhuang2001/meldra/releases/latest/download/meldra_Darwin_arm64.tar.gz
   tar -xzf meldra.tar.gz
   cp meldra ~/.local/bin/
   ```

2. Configure and run:

   ```bash
   meldra config init
   $EDITOR ~/.meldra/credentials.env   # add your OpenAI API key
   cd /path/to/project
   meldra
   ```

Press `Ctrl-C` to exit.

In a supported interactive terminal, Meldra opens a full-screen TUI and streams assistant responses. Non-interactive and unsupported terminals retain the line-based interface.

## Configuration

```text
~/.meldra/config.toml       # model, provider URL, and limits
~/.meldra/credentials.env   # OPENAI_API_KEY
~/.meldra/sessions/         # preserved 0.1.x session snapshots
~/.meldra/tasks/tasks.db    # versioned task, run, tool, approval and event records
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

Custom-provider replay context is capped at 4 MiB by default. Set
`max_custom_turn_input_bytes` or `MELDRA_MAX_CUSTOM_TURN_INPUT_BYTES` to choose
a positive limit up to 64 MiB.

Custom provider URLs must use HTTPS. For local development only, an HTTP URL
on `localhost` or another loopback address can be enabled explicitly:

```toml
base_url = "http://localhost:8080/v1"
allow_insecure_base_url = true
```

Meldra warns before a configured API key is sent to a custom provider host.

```bash
meldra config init   # create starter config files
meldra config        # show effective values and config paths (hides API key)
meldra version
```

## Safety

- File tools are constrained to the workspace root (resolves `..`, symlinks, absolute paths) and block access to `.git` and Meldra's own config/session directory.
- Every edit prints a diff and waits for confirmation before writing.
- Command execution is allowlisted. Commands that compile or execute workspace code require explicit approval; restricted read-only Git commands and `gofmt -d` do not. Approved commands run as your OS user and may access the filesystem and network; environment filtering is not a sandbox. Use `--auto-approve` only inside an isolated container or VM.
- The `verify` tool detects root project markers and selects bounded presets for Make, Go, Python/pytest, Node/npm or pnpm, and Rust/Cargo projects. A Makefile's explicit `check` or `test` target takes priority, and the complete command plan is approved once before execution.
- Custom providers have a 4 MiB replay-context budget; older tool results are compacted first when needed.
- Session files are bounded and validated while loading; conflicting saves from another process are rejected instead of silently overwriting newer state. Task execution also holds advisory locks for the task and canonical workspace, preventing concurrent Meldra writers using the same user cache. These locks do not block your editor or other programs. The TUI keeps at most 200 rendered history entries in memory without applying that display limit to session persistence.
- Repository contents and tool output are treated as untrusted data, not instructions.

## Tasks and recovery

The first request records a task ID. Each attempt has a separate Run; tool
intent, outcome, approvals and events are recorded around execution. A completed
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

**Closing the terminal ends execution.** Ctrl-C, SIGTERM and SIGHUP cancel the
model request and attempt to stop owned command process groups and save confirmed
outcomes. There is no daemon, detach mode or automatic restart. EOF after a
`--prompt` is normal input completion; it does not cancel that prompt early.

After an exit, only an explicit resume continues a task. Recovery reconciles supported file
operations using recorded before/after fingerprints. Unknown command effects or
files changed since the recorded operation block further execution. After
inspecting the actual effects, record a resolution and resume:

```bash
meldra task resolve TASK_ID CALL_ID --outcome succeeded --reason "verified command output and files"
meldra task resume TASK_ID
```

Use `--outcome failed` when that is the verified result. Resolution records your
decision and runs no tools. It does not undo filesystem changes or external
effects. SIGKILL, power loss and processes that escape their process group can
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
Sessions lists include preserved legacy JSON and current task snapshots, prefer
current snapshots with the same ID, and omit empty/invalid snapshots with a
diagnostic for invalid files. A session listing is not the task ledger: a task
can have execution records even when its conversation snapshot is unavailable.

Resuming a legacy session imports its source ID and content digest without
rewriting the original JSON. New messages go to `tasks/sessions/`. Historical
tool outcomes that 0.1.x never recorded remain missing. See the
[backup and downgrade guide](docs/release-0.2.0.md#upgrade-and-recovery) before
upgrading; 0.1.x can read the preserved old snapshot, not the new execution ledger.

## Development

Requires Go 1.26.6. Python 3 is required for release/report tooling and the
real-terminal integration tests; building the binary itself requires only Go.

```bash
make check       # formatting, vet, modules, race tests, coverage, and build
make benchmark   # repeated runtime microbenchmarks; not a noisy CI gate
```

The test suite must maintain at least 75% statement coverage.

See [Benchmarks](benchmarks/README.md) for JSON performance reports, the offline
workflow suite, and comparable SanityHarness/SWE-bench evaluations.

See [Architecture](docs/architecture.md) for package boundaries, the headless
turn API, durable execution records and future multi-agent work.

## PR test binaries

PR checks do not build downloadable artifacts by default. To request one from a
PR, an owner or collaborator comments exactly `/meldra package`. GitHub Actions
acknowledges the request, builds the PR's merge result, then replies with a link
to a seven-day artifact containing the same Darwin/Linux `amd64` and `arm64`
archives and checksums used for releases. It never creates a GitHub Release.

The **Build PR test artifacts** workflow remains available under **Actions** as
a manual fallback: select **Run workflow** and enter the PR number.
The workflow must first be present on the default `main` branch before either
trigger is available.

Meldra follows Semantic Versioning. Release Please prepares a stable release PR
against `main`; merging that separate release PR triggers release creation and
artifact publication. Review its version, manifest and CHANGELOG before merging.
The manual **Prepare 0.2 prerelease** workflow tests an explicit reviewed SHA and
produces exact-version archives; optionally it creates a draft prerelease for
manual review. Source PRs do not themselves publish 0.2.0. Details and release
gates are in [release preparation](docs/release-0.2.0.md).

## License

[MIT](LICENSE)
