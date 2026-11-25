---
title: "Development"
---

Requires Go 1.26.11. Local CI uses [uv](https://docs.astral.sh/uv/getting-started/installation/)
and the `python3` on your `PATH` (3.10 or
newer); no specific minor version is required. Only the optional live browser
checks need Python 3.11+ to read TOML credentials configuration. Building the
binary itself requires only Go.

```bash
make check           # formatting, vet, modules, race tests, E2E suites, coverage, and build
make install-hooks   # enable .githooks for pre-commit and pre-push checks
make check-staged    # quick checks of staged whitespace and Go formatting
make check-local-ci  # full make check in a temporary copy of the index
```

The test suite must maintain at least 75% statement coverage. Repeatable JSONL
test logs can be retained with
`go test -race -json -count=3 -timeout=5m ./...`; real-model quality evaluations
are outside the deterministic suite.

The store capacity tests use Python 3's `sqlite3` module to prepare files at
the production 256 MiB limit. The Go store performs recovery and migration
under the race detector. To measure the cost on a CI runner, run:

```bash
go test -race -count=1 -v ./internal/store -run 'Test(OpenRecoversWALLargerThanDatabaseLimit|IndexMigrationAtCapacityPreservesReopenability)$'
```

On one macOS arm64 run, the interrupted writer left a 268,957,752-byte WAL
(1.67 s test time); the index migration used a 267,329,536-byte database
(3.10 s). Both timings include fixture preparation.

### Source comments

**Unreleased:** Run `make comment-lint` to check first-party Go, Python, shell
scripts and hooks, Make, Go modules, YAML workflows, TOML, JavaScript, HTML,
CSS and Git ignore rules. The scanner includes tracked and non-ignored
untracked sources. Markdown, Python docstrings and literal fixture data lie
outside the count. Interpreter lines, Go build constraints and module
dependency markers have toolchain meaning and are retained.

The first run creates a virtual environment with pinned parsers under
`.artifacts/comment-lint-venv` using Python 3.10 or newer. Use
`make -s comment-inventory` for JSON with original line numbers and
standalone line counts. Each legacy allowance in
`scripts/source-comments-baseline.json` identifies one occurrence by file and
exact comment text. Remove its entry when you delete the comment. The initial
cleanup reached an empty baseline. PR checks compare it with the base commit
and reject new allowances. Use `make comment-lint COMMENT_BASE=origin/main`
to run that comparison locally.

Run `/delete-comment [paths]` in Meldra to repeat the audit. The command loads
the repository's `$delete-comment` skill. The original classifications and
decision records are in the [comment audit](https://github.com/ruhuang2001/meldra/blob/main/engineering/history/comment-audit.md).
`make comment-lint-e2e` verifies the rule with disposable repositories and
writes `.artifacts/checks/comment-lint.json`. Both local check targets and CI
run this gate.

### Local Git hooks

The Git hooks run quick staged checks before each commit and full local checks
before pushing. The push check uses the exact committed revisions being pushed;
the manual `make check-local-ci` command uses staged contents, so stage new or
modified files first. Temporary checks exclude untracked projects and do not
rewrite working files. Full local checks time out after 15 minutes and stop
their process group; set `MELDRA_LOCAL_CI_TIMEOUT` to a positive number of
seconds to adjust this limit.

**Unreleased:** snapshot checks retain logs, reports, and `coverage.out` in a
unique `.artifacts/checks/local-ci-*` directory outside the disposable sources,
including on failure or timeout. Set the `CHECK_DIR` environment variable to
choose the parent evidence directory. Verification rejects Python `-O` and
`PYTHONOPTIMIZE`, which would otherwise disable E2E assertions. Script regression
checks run in `make check`, `make check-fast`, and remote CI.

Additional manual E2E suites:

```bash
python3 scripts/skills-e2e.py --output dist/skills-e2e-new-run
python3 scripts/update-e2e.py --output .artifacts/checks/update-e2e-run
CGO_ENABLED=0 go build -o /tmp/meldra-integrated .
python3 scripts/plan-integration-e2e.py --binary /tmp/meldra-integrated --output /tmp/meldra-integration-evidence
```

`skills-e2e` and `update-e2e` cover their own features. The plan integration
suite is an all-features acceptance run over a combined checkout, so it is
deliberately outside `make check`; run it before a minor release and against the
exact revision you intend to tag. Its expectations are in the
[plan integration failure matrix](./verification/integration-failures.md).

Choose a new output directory for each run.

**Unreleased:** optional [real browser checks](./verification/local-e2e-suites.md#browser-checks)
exercise inline screenshot transport and live-model page repair using installed
Chrome DevTools MCP. The repair driver uses a disposable broken app and independent
before/after form, layout, error-recovery, and task-evidence checks. Browser
installation and paid model inference are not part of `make check`; run these
explicitly and retain unsuccessful attempts when reporting reliability.

**Unreleased (CI configuration):** Remote CI splits race tests across three Ubuntu
jobs. Two partition `internal/app` with complementary `-run` / `-skip` filters:
command/process/output/verification tests and everything else (including new
tests, examples, and fuzz seeds). The third covers every other package discovered
by `go list ./...` and runs fresh non-race storage tests, verification-script
regressions, a CGO-disabled build, and the pinned vulnerability scan. No existing remote check is omitted. Logs
are retained for seven days. Formatting, dependency tidiness, standalone vet,
E2E, and coverage remain in full local/release checks; `go test` also runs its
default vet checks remotely.

Only the latest run for each PR continues; every eligible `main` push has its
own concurrency group, including pending runs. Draft PRs wait until ready for
review. Documentation-only changes are excluded with `paths-ignore`; scripts
and other non-documentation changes now trigger CI too.

There are currently no enforced required status checks. **Do not require this
path-filtered workflow's jobs directly:** GitHub leaves skipped required checks
pending. Before enabling required checks, add an unfiltered dispatcher with a
stable final gate that accepts documentation-only skips and rejects failures
or cancellations from required jobs. Do not add an independently passing docs
workflow with the same check name; mixed changes could bypass validation.

Go uses setup-go's built-in module/compiler cache. On an exact dependency-key
miss, CI restores a previous cache for the same OS image, architecture, and Go
version; Go validates individual entries and setup-go saves the new exact key.
The build/scan shard is the sole writer; the app shards restore without saving,
so a faster app job cannot freeze a cache missing production build outputs.
Keys include both module files and the workflow, allowing build-flag changes to
refresh the immutable cache. No source-per-commit cache, fixture homes, or
evidence is cached. Race instrumentation remains enabled; `GORACE` removes only
the runtime's default one-second exit sleep, also used by the E2E harness.
PR packaging
uses the prebuilt pinned GoReleaser action and retains its full `make check`
before hook. Upstream publishes no govulncheck release binaries, so its pinned
`go run` build continues to reuse the compiler cache.

To reproduce Actions timing analysis (requires authenticated `gh`):

```bash
python3 scripts/actions-timings.py --limit 50 --output .artifacts/actions-timings
# Regenerate the report without GitHub requests:
python3 scripts/actions-timings.py --runs .artifacts/actions-timings/runs.json \
  --jobs .artifacts/actions-timings/jobs.json --output .artifacts/actions-timings-repeat
```

Reports retain raw API responses, per-step classification, CSV, and workflow/job
medians. Queue estimates include dependency waiting; execution includes builds
and scans. Cleanup and artifact overhead are separate. Compare like-for-like
cache states and source revisions, not only historical averages.

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
against `main`; review its version, manifest, and [CHANGELOG](https://github.com/ruhuang2001/meldra/blob/main/CHANGELOG.md)
before merging.

## License

[MIT](https://github.com/ruhuang2001/meldra/blob/main/LICENSE)

## Faster local feedback

`make check-fast` provides local feedback. It currently
runs ordinary Go tests and selected non-MCP E2E suites, including PTY exit checks.
The [driver](https://github.com/ruhuang2001/meldra/blob/main/scripts/mcp-check.py) and
its report's `suite_order` identify the selected suites. It skips race, coverage, and MCP-specific E2E
suites and does not replace `coverage.out`. Run the full `make check` before
submitting changes.

Full checks run up to three independent E2E suites concurrently with separate
HOME, XDG, temporary, and coverage directories. Use `make check E2E_WORKERS=1`
on constrained machines. Reports, timings, logs, and checksums are retained in
`.artifacts/checks`.

Expensive suites, starting with compaction and capability, are
submitted first; `report.json` records `suite_order` alongside suite timings.
On a 12-core development machine, try `make check E2E_WORKERS=6`; the default
remains three for smaller machines. Shared capability fixtures preserve the
harness's `GORACE` and update-check controls, avoiding the race runtime's
per-process exit sleep while retaining race instrumentation and coverage.

`make check` skips deliberately slow E2E scenarios (currently
`serve-approval-budget`, which waits 121s past the old fixed call deadline).
`make check E2E_SLOW=1` and `make release-check` include them.

## Verify updater changes

From the source checkout, run the updater E2E suite with a new output directory:

```bash
python3 scripts/update-e2e.py --output .artifacts/checks/update-e2e-run
```

It builds two version-labeled copies of the current source and uses loopback
release and model fixtures. It checks progress before delayed responses in pipe
and PTY modes, replacement and failure behavior, and saved-session continuity.
The output retains `report.json`, transcripts, binary and source hashes, and a
`replay.sh` command. It does not download published releases or certify
cross-version storage compatibility.
