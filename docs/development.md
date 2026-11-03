---
title: "Development"
---

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
python3 scripts/update-e2e.py --output .artifacts/checks/update-e2e-run
CGO_ENABLED=0 go build -o /tmp/meldra-integrated .
python3 scripts/plan-integration-e2e.py --binary /tmp/meldra-integrated --output /tmp/meldra-integration-evidence
```

`skills-e2e` and `update-e2e` cover their own features. The plan integration
suite is an all-features acceptance run over a combined checkout, so it is
deliberately outside `make check`; run it before a minor release and against the
exact revision you intend to tag. Its expectations are in the
[plan integration failure matrix](verification/plan-integration-failures.md).

Choose a new output directory for each run.

**Unreleased (CI configuration):** Remote CI splits race tests across two Ubuntu
jobs: `internal/app` and every other package discovered by `go list ./...`.
The second job also runs fresh non-race storage tests, a CGO-disabled build,
and the pinned vulnerability scan. No existing remote check is omitted. Logs
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
No source-per-commit cache, fixture homes, or evidence is cached. PR packaging
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

**Available since v0.4.1:** `make check-fast` runs ordinary Go tests and the capability E2E
suite, including PTY exit checks. It skips race, coverage, and MCP-specific E2E
suites and does not replace `coverage.out`. Run the full `make check` before
submitting changes.

Full checks run up to three independent E2E suites concurrently with separate
HOME, XDG, temporary, and coverage directories. Use `make check E2E_WORKERS=1`
on constrained machines. Reports, timings, logs, and checksums are retained in
`.artifacts/checks`.

**Available since v0.5.0:** Expensive suites, starting with compaction and capability, are
submitted first; `report.json` records `suite_order` alongside suite timings.
On a 12-core development machine, try `make check E2E_WORKERS=6`; the default
remains three for smaller machines. Shared capability fixtures preserve the
harness's `GORACE` and update-check controls, avoiding the race runtime's
per-process exit sleep while retaining race instrumentation and coverage.

**Available since v0.5.0:** `make check` skips deliberately slow E2E scenarios (currently
`serve-approval-budget`, which waits 121s past the old fixed call deadline).
`make check E2E_SLOW=1` and `make release-check` include them.
