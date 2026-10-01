# 0.2.0 implementation verification

This records evidence for the implementation PR, not an announcement that 0.2.0
has shipped. Source checkpoint: `dd06e00`. No production tags/releases or merge
to main were performed. Subsequent documentation-only commits do not change the
measurement source. Any later runtime change needs proportionate revalidation.

## Requirements and evidence

| Requirement | Implementation / evidence |
| --- | --- |
| Foreground-only lifetime; no daemon or automatic resume | `app.Main`, `Agent.RunTurn`, `taskExecution`; real subprocess SIGINT/SIGTERM/SIGHUP and PTY-close tests |
| Task/Run/ToolCall/Approval/Event contracts | `internal/task`; status-transition tests; stable JSON event schema |
| Durable state and events | SQLite WAL/FULL sync, transactional writes in `internal/store`; transaction fault and SIGKILL rollback tests |
| Run terminal history remains immutable | Store transition guards; hard-kill recovery tests preserve the interrupted original Run and create a new Run |
| Cross-process execution ownership | OS flock for task + canonical workspace in a shared user-cache namespace; separate-process/config-directory and symlink-alias tests |
| Intent recorded before side effects; result afterward | `taskExecution.invoke`; ledger failures stop execution; four required crash windows plus pending/approved approval windows tested |
| Structured tool outcomes | `tool.Result` plus built-in observations; no parsing of display prose for exit/decline/unknown status |
| Bounded logs and artifacts | 256 KiB model excerpt, separate command log up to 16 MiB, SHA-256 artifact references; >256 KiB tail-preservation test |
| Approval binding and durable waiting state | Pending request saved before asking user; exact arguments + file pre/post fingerprints; decision persisted before execution |
| Explicit recovery, no unknown-effect retries | Known call-ID redelivery reuses recorded outcome; unknown outcomes block StartRun; explicit user reconciliation command |
| File recovery does not overwrite user changes | Before/after fingerprint inspection only; hard-kill followed by user edit stays unknown and unchanged |
| Command ownership and bounded cancellation | Owned process groups, command.started PID/group record, bounded pipe wait; real parent/child cancellation tests |
| Initial task with no JSON snapshot remains resumable | Minimal Session reconstructed only when snapshots are absent; corruption is never silently replaced |
| Legacy JSON migration | Original files retained, idempotent source ID/hash import, source-change detection, task snapshots preferred; legacy tool history marked missing |
| Task inspection and JSON events without inference | `tasks`, `task show`, `task events --json --after`; inspection tests work without an API key |
| Dependency boundary | Production app does not import model SDK; task contracts do not depend on UI/SDK; boundary test |
| Context compaction optimization | Exact byte accounting, order/metadata preservation and fuzz tests; `performance-0.2.0.md` has matched before/after evidence |
| Reproducible offline evaluation | Versioned app/store manifests, complete attempt accounting, environment/source metadata and raw samples |
| Fixed live-quality cohort and independent grading | Five Go/Python fixtures with isolated Docker hidden graders; broken fixtures 0/5 and authored repairs 5/5 validate graders only |
| Release channel preparation | Manual explicit-SHA alpha/rc workflow, default artifact-only, optional draft; stable manifest remains 0.1.0 |
| Release version/artifact verification | 17 Python tests, actionlint, GoReleaser config check; clean fixture packages four targets and native Darwin arm64 smoke |

## Local verification

On Apple M2 / Darwin arm64 / Go 1.26.6:

- `make check`: passed, including race; total statement coverage 81.2%.
- `make benchmark-report-test`: 19 report tests and 6 cohort tests passed.
- `python3 -m unittest discover -s scripts -p 'test_release.py'`: 17 passed.
- `govulncheck@v1.8.0 ./...`: no vulnerabilities found.
- actionlint v1.7.12: changed workflows passed.
- GoReleaser v2.14.0 `check`: passed.
- Pure-Go SQLite package cross-build: Darwin/Linux, amd64/arm64, CGO disabled.
- App offline-runtime-v2: 62 cases × 3 repetitions, **186/186 passed**.
- Store offline-task-store-v1: 17 cases × 3 repetitions, **51/51 passed**.
- Microbenchmarks: 48 cases × 5 samples, **240 samples recorded**.

Machine-local reports and raw logs are ignored artifacts under
`benchmarks/results/dd06e00-{app,store,micro}.{json,log}`. They contain no live
model quality score. The completed micro run used five samples, 200 ms and CPU=1; report metadata
confirms the clean source checkpoint and retains all 240 samples.

## Outstanding publication gates

- Remote PR CI and four native platform smoke jobs must be green on the final
  candidate. Local cross-compilation is not evidence of execution on all four.
- The fixed five-task cohort has **not** been solved by a live model. Grader
  fixture checks do not establish agent quality. Model/provider/prices/expense
  authorization remain a required human decision; no paid API calls were made.
- `benchmarks/live/run.py` intentionally does not run a model until enforceable
  request/token admission limits exist. A timeout or post-response usage report
  is not a hard dollar cap. External isolated evaluation under an explicitly
  agreed budget remains possible; do not claim the current scaffold enforces it.
- Alpha user acceptance, candidate freeze and formal publication remain release
  operations after review. This PR does not alter the last released manifest or
  publish a version.

## Limits that remain explicit

- Known outcomes are durable, but arbitrary external commands cannot be made
  transactionally atomic with SQLite. Unknown outcomes require reconciliation.
- SIGKILL, power loss and deliberately detached descendants may prevent graceful
  cleanup. Stored PID/group data is diagnostic; no later process is killed solely
  by a potentially reused PID.
- Same provider call ID means the same operation within a task. Providers that
  reuse IDs for unrelated operations fail closed rather than executing again.
- Automatic file reconciliation checks recorded hashes and mode. It does not
  infer external system state or certify that the user's overall goal is solved.
- Session-tool side effects interrupted before their result is recorded remain
  unknown for explicit handling; a saved plan alone is not a tool receipt.
- SQL schema migration, source-session preservation and artifact integrity are
  tested locally. Backup/downgrade guidance is in `release-0.2.0.md`.
