# Measuring Meldra

Measure runtime cost, workflow reliability, and model task quality separately.
A fast tool loop is not evidence that the model solves more coding tasks, and
scripted tool workflows must not be presented as model success rates.

## Evaluation layers

| Layer | Coverage | Measurements | Model/API required |
| --- | --- | --- | --- |
| Go microbenchmarks | 48 parameterized cases: the original 41 plus seven task-store workloads for task reads, event append/page, tool lifecycle, recovery scanning, indexed provider-call lookup and artifact deduplication | ns/op, B/op, allocs/op, throughput and artifact disk bytes where applicable; repeated samples and medians | No |
| Offline runtime v2 | 65 cases: the original 32 plus task recording, approvals, artifacts, CLI migration, six hard-kill boundaries, explicit recovery, terminal EOF/hangup, process-group cleanup and cancellation during preflight/verification | Pass/fail/skip per attempt, elapsed time, and tool/request counts where instrumented | No; provider is scripted or local |
| Offline task-store v1 | 22 cases: durable transactions, ownership across processes/config homes, killed transaction rollback, migrations, indexed provider-call lookup, filesystem identity/alias ownership, limits, corruption and explicit reconciliation | Pass/fail/skip per attempt and elapsed time | No |
| Fixed release cohort | Five small Go/Python repair tasks with hidden independent Docker graders; scaffold preparation and grading are available, live execution is gated | Per-task grades; model, token and cost evidence must be attached separately | Grading: no; actual model attempts: yes |
| SanityHarness | Small multi-language coding tasks; core and extended tiers | Actual task outcomes, task duration, attempts, caller-recorded estimated cost | Yes; Docker and SanityHarness |
| SWE-bench | Repository issue fixing on the chosen dataset/split | Official resolved-task rate after external grading | Yes; dataset and evaluation harness |

Go benchmarks and the offline suite run real Meldra code. Agent microbenchmarks
use an in-memory provider; streaming microbenchmarks feed decoded events rather
than measuring HTTP latency or SDK wire parsing. Workflow scenarios execute real
workspace tools, approvals and durable session writes. Existing regression
cases additionally exercise local HTTP/SSE behavior. No benchmark runner code
reads API credentials or calls a live model. A cold Go module cache can still
require dependency downloads before tests start.

## Quick start

Requires Go 1.26.6 and Python 3.10+ (standard library only):

```sh
make benchmark-report-test
make benchmark-report BENCH_OUTPUT=benchmarks/results/baseline-micro.json
make benchmark-eval EVAL_OUTPUT=benchmarks/results/baseline-scenarios.json
make benchmark-eval EVAL_SUITE=benchmarks/suites/store.json \
  EVAL_OUTPUT=benchmarks/results/baseline-store.json
```

Microbenchmarks default to five samples, 200 ms per adaptive Go benchmark, and
`-cpu=1`. Scenarios default to three repetitions. Reports and raw Go JSON logs
are saved under the ignored `benchmarks/results/` directory. Existing run paths
are rejected so an earlier baseline is not overwritten accidentally.

The default scenario manifest is `offline-runtime-v2` in `suites/offline.json`.
Use `EVAL_SUITE` (or `scenarios --suite PATH`) to select a manifest. The original
32-case manifest is retained byte-for-byte as `suites/offline-v1.json`, allowing
comparisons with the earlier baseline. Do not compare v1 with v2 or merge store
scores into application scores; each report records its own manifest hash and
denominator. The store manifest includes real process contention and killed
transaction tests, not only mocked failures.

The saved `dd06e00` reports describe an earlier checkpoint, not the expanded
65/22-case manifests:

- `app`: 62 cases, 186/186 passed; manifest SHA-256 `fe03cfc4f93f181f7f8f580bcf8449707d72169032b51310c0dd77fb262ea1bf`.
- `store`: 17 cases, 51/51 passed; manifest SHA-256 `ebd91de8c0eb50c490c8e2428c630853a81df78f1bf6b612011d46ea8fc2bad8`.

Those files remain unchanged. Run both current manifests again before claiming
that the expanded inventory passed; counts alone are not execution evidence.

For a quick smoke check, use `BENCH_COUNT=1 BENCH_TIME=1x`; these one-shot values
are **not** a stable performance baseline. Full runs can take several minutes,
especially on a slow disk because durable-write benchmarks include fsync.

```sh
make benchmark-report BENCH_COUNT=10 BENCH_TIME=500ms \
  BENCH_OUTPUT=benchmarks/results/candidate-micro.json
make benchmark-eval EVAL_COUNT=5 \
  EVAL_OUTPUT=benchmarks/results/candidate-scenarios.json
```

Each report records the revision, dirty status, source digest, Go version,
OS/architecture, CPU and measurement parameters. Record the model, provider,
dataset revision, task selection, attempt budget and isolation settings separately
for live model evaluations; never mix those results with runtime reports.

For focused work or standard `benchstat` input:

```sh
python3 benchmarks/run.py micro --bench 'BenchmarkProviderReplayCompaction' \
  --count 10 --benchtime 500ms --output benchmarks/results/compaction.json
make benchmark > benchmarks/results/raw-bench.txt
# With benchstat installed separately:
benchstat before.txt after.txt
```

The raw `make benchmark` target emits regular Go benchmark text. The `.log`
files from `benchmark-report` contain Go JSON events, not benchstat input.

## Compare revisions

Run the baseline and candidate with the same suite, Go toolchain, machine,
CPU setting and benchtime. Avoid running the two revisions concurrently, and
keep background load low. Use separate worktrees or commits; do not change
fixtures while comparing an implementation optimization.

```sh
python3 benchmarks/run.py compare \
  benchmarks/results/baseline-micro.json benchmarks/results/candidate-micro.json \
  --output benchmarks/results/micro-comparison.json
python3 benchmarks/run.py compare \
  benchmarks/results/baseline-scenarios.json benchmarks/results/candidate-scenarios.json
```

The comparator rejects mismatched benchmark/metric sets, scenario manifests,
incomplete runs, or environments. `--allow-environment-change` produces explicit
warnings; such timings cannot establish a code-only speedup. Source revisions
are expected to differ. Scenario names/manifest versions must be updated when
their contract changes; a matching name alone cannot prove unchanged semantics.

Micro reports retain all samples, median, minimum, maximum and nearest-rank p95.
With only five repetitions, p95 is the largest observed sample. These are
observed distributions, not statistical significance claims or service latency
percentiles. A percentage delta from a zero baseline is `null`; the absolute
before/after values remain available. Use more samples and benchstat before
claiming a small optimization.

Scenario reports count every attempt. Skips never count as passes. Assertions
that fail remain reportable/comparable; missing cases, interrupted attempts,
compilation failures and infrastructure errors do not produce a complete score.
The runner exits nonzero on failed/skipped scenarios. Instrumented workflows
report turn duration excluding fixture setup; other regression cases report
whole-test duration and are labeled accordingly. Durations include all attempts,
not just successes. Repeated deterministic scenarios measure reliability, not
independent evidence of model task-solving ability.

No automatic wall-clock regression gate is enabled: shared CI runners and
filesystem caching make small timing differences noisy. The manually dispatched
**Runtime benchmarks** workflow uploads JSON reports and raw logs for 14 days.
Normal CI runs scenario correctness with Go tests and validates the Python
report parser, without running the expensive microbenchmark matrix.

## Live model quality

The [fixed 0.2.0 release cohort](live/README.md) prepares five small Go/Python
tasks and grades submitted files in isolated Docker containers. Its budget
planner rejects unknown prices and reservations above the cap, but does not
claim runtime enforcement. Live execution stays disabled until explicit model,
provider, cohort and cost approval plus enforceable token admission controls are
available. Grader self-tests are not model quality evidence.

Use [SanityHarness](sanityharness/README.md) for quick multi-language tasks and
[SWE-bench](swe-bench/README.md) for realistic repository work. Both consume API
quota. Use a fresh isolated run, fixed task cohort, fixed attempt budget and
recorded model/provider settings. Meldra's `--auto-approve` mode requires an isolated
container or VM; a temporary directory alone is not an OS sandbox.

Create SanityHarness baselines using the existing `make benchmark-sanity-baseline`
target, then compare them:

```sh
python3 benchmarks/run.py compare-quality baseline.json candidate.json \
  --output benchmarks/results/quality-comparison.json
```

Quality comparison requires identical task identities, tier and attempts per
task, and lists newly failing/improved tasks. It recalculates task success from
individual outcomes. Different models/providers are allowed and explicitly
labeled. Estimated cost is caller-supplied, not measured billing; missing cost
stays `unknown`, and token counts are never fabricated. Duration is summed task
time, not parallel evaluation wall time. If attempts exceed one, the result is
an observed task success rate under that budget, not pass@1 or estimated pass@k.
The comparator cannot verify dataset or harness revisions absent from old
SanityHarness reports; record these alongside baselines before drawing claims.

SWE-bench `predictions.jsonl` contains proposed patches, **not scores**. Run the
official evaluator to obtain resolved-task results. Pin the dataset revision,
retain empty/failed predictions in the denominator, and compare the same cohort.

## Extending the suite

- Add microbenchmarks close to their package with named sizes/workloads and
  `b.Loop()`. Exclude setup, validate outputs, and retain allocation metrics.
- Add tool workflows in `internal/app/evaluation_test.go`. The scenario's
  expected file contents/status are graded independently of model prose.
- Add expected case names to `suites/offline.json`; missing names fail evaluation.
- Add task-store fault/recovery cases to `suites/store.json`; select explicit
  subtests where each crash boundary or signal must be counted independently.
- Change the suite version when measurement semantics change. Keep failures and
  raw samples instead of selecting only successful or fastest attempts.
- Prefer task success, regressions, latency, token/cost usage when available,
  and tool-call efficiency over a single weighted “agent score”.
