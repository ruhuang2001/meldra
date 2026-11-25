---
title: "Local E2E suites"
description: "Repeatable harnesses for the streaming, provider, web, and MCP boundaries, and where their evidence lives."
---

Each suite below runs the real binary against isolated homes and offline
fixtures, so none needs provider credentials or paid inference. They are evidence
for application behavior, not certification of a third-party service or a live
model's judgment.

| Area | Suite |
| --- | --- |
| Web fetch and search | `scripts/web-e2e.py` |
| Stream JSON and approval resume | `scripts/capability-e2e.py --stream-json --only stream-json` |
| Custom Responses providers | `scripts/provider-compat-e2e.py` |
| MCP and skills | `scripts/mcp-check.py`, `scripts/skills-e2e.py` |
| Interactive MCP flows | `scripts/mcp-auth-interaction-e2e.py` |
| Store capacity and recovery | `go test -race ./internal/store` |

## Web tools

```sh wrap
python3 scripts/web-e2e.py --binary /absolute/path/to/meldra --output /tmp/web-evidence
```

The harness isolates homes and uses offline HTTP/TLS fixtures. It writes
`report.json`, protocol and provider transcripts, task records, content
artifacts, and binary/overlay provenance.

Positive-network tests replace only DNS/raw dialing/TLS trust with a build
overlay; address validation and production policy remain active, and release
binaries have no bypass. Fixture builds normalize the source path and `PWD`, then
verify an overlay-only startup marker before any request. The suite repeats
configuration checks with canonical and symlink source paths and records the
build paths and probe output. `make check` runs this suite. See the
[web failure matrix](./web-failures.md).

## Stream JSON

```sh wrap
CGO_ENABLED=0 go build -o /tmp/meldra-stream .
python3 scripts/capability-e2e.py --binary /tmp/meldra-stream --output /tmp/meldra-stream-evidence --stream-json --only stream-json
```

Use a new output directory. The suite uses isolated homes, fake credentials, and
a loopback provider, and preserves stdout/stderr, provider requests, ledger
snapshots, a report, binary checksum, and evidence checksums. The schema snapshot
is `scripts/fixtures/stream-json-v1.json`; see the
[stream JSON failure cases](./stream-json-failures.md).

## Providers

`make check` exercises custom Responses SSE/JSON handling, tool replay, limits,
errors, and cancellation through Go regressions and a real-binary loopback E2E.
Run `python3 scripts/provider-compat-e2e.py --binary dist/meldra --output /tmp/meldra-provider-check`
for the dedicated compatibility scenarios and retained wire/ledger evidence.

## MCP

`make check` covers Go race tests, the real-binary MCP and application E2E
suites, and the 75% coverage gate. `scripts/mcp-check.py` defines suite selection;
each report records `suite_order` and omitted checks. Suites write a fresh
`.artifacts/checks/mcp-check-*` directory. Override it with
`make check CHECK_DIR=/path/to/checks` or
`scripts/mcp-check.py --output-dir /path/to/checks`. Keep evidence outside
`dist/`, which GoReleaser requires to be empty after its before hooks. CI also
runs `govulncheck`.

The tests cover implemented local integration paths. They do not certify every
third-party server, real OAuth provider, HTTP hosting, subscriptions,
completion, dynamic notifications, or multimodal sampling.

```sh wrap
mkdir -p dist
go build -race -o dist/meldra-mcp-race .
python3 scripts/mcp-auth-interaction-e2e.py --binary dist/meldra-mcp-race --output dist/mcp-auth-interaction-repeat
```

Choose a fresh output directory to preserve evidence. Scripts use local protocol
and model fixtures without real accounts or model fees. Reports, checksums, wire
records, provider requests, and transcripts support reproduction. OAuth evidence
records grant and refresh types without printing access or refresh tokens.

### Browser checks

The opt-in driver uses a real Meldra binary, real Chrome MCP, and a synthetic
image-only board. An offline Responses fixture selects navigation and capture,
then checks the actual wire payload and persisted PNG. It needs no browser or
network inference in ordinary `make check` runs.

```sh wrap
go build -o /tmp/meldra-images .
python3 scripts/mcp-browser-check.py --binary /tmp/meldra-images \
  --server-script /absolute/path/to/installed/chrome-devtools-mcp.js \
  --output .artifacts/checks/browser-new-run
```

Use the entrypoint from the installed package's `bin.chrome-devtools-mcp` field
and a fresh output directory. The driver requires Python 3.11+ and accepts
`--node /absolute/path/to/node`. `--expect-text-only` records the pre-feature
baseline. With explicit `--live-config /path/to/private/MELDRA_HOME`, the driver
forwards the final image-bearing request to that configured model, incurs model
usage, and checks the fresh visual code and shapes in its answer. It does not
copy credentials into evidence. This verifies visual interpretation, not
autonomous tool selection or a page-repair workflow.

`scripts/mcp-browser-repair.py` runs the configured live model for the entire
task against a disposable copy of `scripts/fixtures/browser-repair`. Unlike the
screenshot check above, it does not script model responses or tool selections.
The intentionally broken demo has a failing reservation action and horizontal
overflow on phones. The model must reproduce them through Chrome, inspect
screenshots/console/network, shut down cleanly, repair only the frontend,
restart, and operate the form again.

```sh wrap
go build -o /tmp/meldra-browser .
python3 -B scripts/mcp-browser-repair.py --binary /tmp/meldra-browser \
  --server-script /absolute/path/to/installed/chrome-devtools-mcp.js \
  --node /absolute/path/to/node --chrome /absolute/path/to/chrome \
  --live-config /path/to/private/MELDRA_HOME \
  --output .artifacts/checks/browser-repair-new-run
```

This opts in to paid inference and automatic approvals for the disposable task.
It isolates HOME, state, browser profile, and workspace, but is **not an OS
sandbox**: the configured model and MCP server are trusted. Credentials go only
to the Meldra process, not the MCP environment or evidence files. The driver
requires Python 3.11+, Node.js, Chrome, and the tested MCP package. Its
independent verifier uses that package's bundled Puppeteer; pass
`--puppeteer-module` if the installed package has a different layout.

An independent browser, whose assertions are not supplied to the repairing
model, checks real HTTP results and calculated prices, required/invalid fields
and seat limits, reload, recovery after an injected HTTP 503, and control
geometry at 320/390/768/1280 pixels. The task audit checks before/after
interaction, screenshots, console/network observations, artifact digests, clean
managed-process exits, and the permitted file-change scope. Reports, diffs,
screenshots, and task records are retained even for failed attempts when
available. Use a fresh output directory per run; model outcomes vary, so repeat
and report failures as well as successes. The default model-run bound is 900
seconds (`--timeout`). These opt-in live checks are outside ordinary `make check`
and do not establish reliability on arbitrary sites, providers, or browsers.

## Task records

`go test -race ./internal/task ./internal/store` covers exhaustive Run/tool
transitions, terminal immutability, approval binding and expiry, commit faults
at the intent/start/result/Run boundaries, unknown-outcome gates, idempotent
legacy import, corruption and limits, private paths, cross-store symlink aliases,
actual subprocess lock competition, and SIGKILL lock release. The application
tests own signal propagation, file reconciliation, and command process groups.
