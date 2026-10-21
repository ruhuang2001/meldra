# Meldra Agent Rules

## Start Here

Meldra is a local, serial foreground coding agent, not a daemon or scheduler.
Read affected code and tests before editing, and check the authoritative docs:

- [README.md](README.md): installation, configuration, commands, and development.
- [docs/architecture.md](docs/architecture.md): dependency direction and runtime boundaries.
- [docs/task-storage.md](docs/task-storage.md): ledger, ownership, and recovery.
- [docs/capabilities.md](docs/capabilities.md): project context and foreground controls.
- [docs/mcp.md](docs/mcp.md) and [docs/skills.md](docs/skills.md): integration contracts.

## Code Ownership

- `main.go`: thin process entry and link-time version.
- `internal/app`: CLI/TUI, orchestration (`task_execution.go`), policy, workspace, sessions, MCP, Skills.
- `internal/provider`: inference contracts, Responses SDK adaptation, streaming. Keep model SDK imports here.
- `internal/tool`: tool definitions, validated dispatch, structured outcomes (no UI or model SDK deps).
- `internal/task`: record types and state transitions only (no execution or scheduling).
- `internal/store`: SQLite records, artifacts, cross-process leases (depends on `internal/task` only).
- `internal/testenv`: test-only home/config isolation shared with helper processes.
- `scripts` & `.goreleaser.yaml`: installer, E2E verification, and release artifacts.

Modify the existing owner of a behavior rather than adding parallel paths. Keep execution semantics consistent across CLI, TUI, and MCP.

## Runtime Invariants

- **Serial execution**: Never parallelize turns or tool calls across a shared `Agent` or `Workspace`.
- **Policy & approvals**: Check policy at dispatch and side-effect admission. Plan mode cannot gain write/exec permissions via auto-approval, Skills, or `AGENTS.md`. Approvals bind to the exact reviewed arguments/files.
- **Durable evidence**: Persist intent before side effects and outcomes after. Stop on persistence failure. Missing outcome evidence is `unknown`, never `succeeded` or a no-op.
- **Explicit recovery**: Inspection and recovery must not silently execute work or replay operations.
- **Leases & locks**: Hold task/workspace leases during execution. Never unlink lock files to clear busy states (unlinking a locked inode allows a second writer).
- **Sessions & schema**: Preserve atomic session replacement, dir sync, and stale-save rejection. Reject unknown newer DB schemas explicitly.
- **Cancellation lifetimes**: Keep app, turn, inference, and subprocess lifetimes distinct; stopping a turn must not exit chat.
- **Security & protocols**: Keep credentials and raw reasoning out of durable records. Preserve filesystem/state permissions (command approval is not an OS sandbox). Keep stdout/JSON/MCP protocols free of interactive notices.

## Development and Verification

Build with the `go.mod` toolchain (`CGO_ENABLED=0` on Darwin/Linux, amd64/arm64) and Python 3 for integration tests. Prefer the standard library over new dependencies.

- **Iterate**: Run focused tests (`go test ./internal/app -run '^TestName' -count=1`, plus `-race` for concurrency/lifecycle changes).
- **Verify code**: Run `make check` (fmt, vet, mod tidy, race tests, E2E, >=75% coverage, build; outputs to `.artifacts/checks` or `CHECK_DIR`/`COVERAGE_FILE`). `make check-ci` is not a substitute. Do not edit Go sources while collecting coverage.
- **Verify docs/staged**: Docs-only changes need link/command review and `git diff --check`. `make check-staged` checks staged formatting; `make check-local-ci` tests only the staged index.
- **Test isolation**: Keep home/config isolated, use fake providers/loopback fixtures, and keep auto-update checks disabled unless testing them with local fixtures.
- **Terminal & docs**: Exercise affected line/PTY modes and inspect rendered output for TUI changes. Update help text and `docs/` when behavior changes.

## Project Language & Commits

Use English for all docs, CLI/TUI messages, help text, and comments; preserve Unicode/multilingual test fixtures.

Format commits as `<type>(<scope>): <imperative lowercase summary>` (scopes: `agent`, `cli`, `config`, `ci`, `session`, `stream`, `tui`, `workspace`, `docs`, `release`; e.g., `feat(tui): add approval scrolling`). Inspect `git log --format='%s' -20` before committing, stage only intended files, run relevant checks, and never push unless asked.
