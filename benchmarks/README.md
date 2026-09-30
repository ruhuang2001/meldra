# Measuring Meldra

Measure runtime cost, workflow reliability, and model task quality separately.
A fast tool loop is not evidence that the model solves more coding tasks, and
scripted tool workflows must not be presented as model success rates.

## Evaluation layers

| Layer | Coverage | Measurements | Model/API required |
| --- | --- | --- | --- |
| Go microbenchmarks | 41 parameterized cases across agent turns, tool dispatch, discovery, reads, patch preparation, durable edits/undo, sessions, streaming, replay compaction, JSON normalization and terminal rendering | ns/op, B/op, allocs/op, throughput where applicable; repeated raw samples and medians | No |
| Offline runtime suite | 32 cases: 18 end-to-end tool/session workflows plus cancellation, concurrency, rollback, gateway, stream and persistence regressions | Pass/fail/skip per attempt, elapsed time, and tool/request counts where instrumented | No; provider is scripted or local |
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
```

Microbenchmarks default to five samples, 200 ms per adaptive Go benchmark, and
`-cpu=1`. Scenarios default to three repetitions. Reports and raw Go JSON logs
are saved under the ignored `benchmarks/results/` directory. Existing run paths
are rejected so an earlier baseline is not overwritten accidentally.

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

Use [SanityHarness](sanityharness/README.md) for quick multi-language tasks and
[SWE-bench](swe-bench/README.md) for realistic repository work. Both consume API
quota. Use a fresh isolated run, fixed task cohort, fixed attempt budget and
recorded model/provider settings. Meldra's `--yes` mode requires an isolated
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
- Change the suite version when measurement semantics change. Keep failures and
  raw samples instead of selecting only successful or fastest attempts.
- Prefer task success, regressions, latency, token/cost usage when available,
  and tool-call efficiency over a single weighted “agent score”.
