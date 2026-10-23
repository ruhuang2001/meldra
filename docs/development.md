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
