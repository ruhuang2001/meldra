---
title: "Benchmarking and RSI"
description: "Test quality analysis, agent benchmarking, and recursive self-improvement"
---

## Test Quality

`make check` enforces a minimum statement coverage of 75% (see
`COVERAGE_MIN` in the `Makefile` and [Development](development.md)). Treat that
gate as the source of truth for the current number; quoting a snapshot here
would go stale as soon as the suite changes.

What the target is meant to guarantee:

- Tests assert behavior, not just that a call returns (`t.Fatal`/`t.Error`)
- Boundary cases cover limits, state transitions, and error paths
- Side effects are verified (database invariance, transaction rollback)
- End-to-end suites exist (`capability-e2e`, `mcp-e2e`)

**Real example** (`internal/store/validation_test.go`):

```go
// Not just checking the return value — verifies the DB is unchanged
for table, want := range counts {
    var count int
    if err := f.s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&count);
       err != nil || count != want {
        t.Fatalf("rejected operation changed %s count", table)
    }
}
```

Coverage is necessary but not sufficient: it says the system works, not that it
solves real problems. That is what the agent benchmarks below measure.

## Agent Benchmarking: SWE-bench

> **Unreleased.** The repository adapter and workflows below are development
> tooling. Run them from this checkout with an explicitly selected Meldra binary;
> they are not included in the latest published binary.

### Why Agent Benchmarking?

Code coverage tells you **if the system works**. Agent benchmarking tells you
**if it solves real problems**. For RSI (recursive self-improvement), you need
to answer:

- "Did this change help me fix more real bugs?"
- "What types of tasks do I fail at?"
- "Where should I improve next?"

### SWE-bench Lite

- **What**: 300 real GitHub issues from Python repositories
- **Validation**: a patch counts only if the project's own tests pass
- **Comparison**: public leaderboard results track the state of the art; read
  the current numbers from the
  [leaderboard](https://www.swebench.com/leaderboard) rather than from this page

Start with a small sample to validate the pipeline, then grow.

### Quick Start

Configure the model provider and `OPENAI_API_KEY` first; see
[Configuration](configuration.md). Run these commands from the repository root.

```bash
# Install dependencies (swebench pulls in datasets)
pip install swebench sb-cli typing-extensions

# Build Meldra
CGO_ENABLED=0 make build

# Generate patches for 5 tasks
python3 scripts/swe-bench-lite.py --binary dist/meldra --max-tasks 5
```

The script prints one line per instance as patches land:

```
[1/5] django__django-11099: empty patch, 42.1s
[2/5] django__django-11133: 812 byte patch, 96.4s
...
Predictions: /path/to/benchmarks/swe-bench/meldra-<timestamp>/predictions.jsonl

Evaluate locally with:
  cd .../meldra-<timestamp> && python -m swebench.harness.run_evaluation ...
```

A resolved-rate summary (for example `Resolved 3/20 (15.0%)`) is printed only
after `--evaluate`, once the official harness has run the tests.

### Offline Adapter Verification

The adapter regression suite runs as part of `make check` and
`make check-fast` without network access or model credentials. To run it alone:

```bash
python3 scripts/swe-bench-adapter-e2e.py --output .artifacts/adapter-e2e
```

Use a fresh output directory. It retains `report.json`, invocation logs,
predictions, attempt records, and process-cleanup evidence for repeatable review.

### How It Works

`scripts/swe-bench-lite.py` generates patches and can invoke a separate evaluator:

1. **Loads** the SWE-bench subset from HuggingFace.
2. **For each task**: clones the repo at `base_commit` into a temporary
   directory, runs Meldra with the problem statement and `--auto-approve`, then
   stages the result and captures the diff.
3. **Records** every attempt in `attempts.jsonl`, including agent errors and
   partial patches. Only successful attempts enter `predictions.jsonl`; a
   successful empty patch remains a legitimate prediction.
4. **Resumes** with the same `--run-id`: successful predictions are retained,
   failed instances are retried, and the predictions file is rewritten
   atomically to remove invalid records rather than appending behind a damaged
   trailing record.

If any requested instance fails, the batch exits nonzero and skips both local
and hosted evaluation. Inspect `attempts.jsonl` and `agent-logs/`, address the
failure, then rerun the same command with the same `--run-id`. An empty patch
from a successful attempt does not trigger a retry merely because it is empty.

Generation runs on the host with automatic approval enabled by default. Each
checkout is temporary; commands still run with the host user's permissions.
No container or additional opt-in is required for generation. The adapter's
default 10-minute agent timeout sends SIGTERM first to allow Meldra to cancel and clean
up its commands, then force-kills processes that outlive the grace period.

Verification is a separate step: `--pull --evaluate` fetches the selected
instance images and runs the official SWE-bench harness in Docker; `--submit`
sends the predictions to hosted evaluation. The adapter does not directly run
the task's tests during generation, although Meldra can run commands and tests.
See [SWE-bench setup](swe-bench-setup.md) for hosted credentials and reports.

### Establish a Baseline

```bash
# Generate predictions for 50 tasks
python3 scripts/swe-bench-lite.py --binary dist/meldra --max-tasks 50 \
    --run-id baseline-v0.4.2 --pull --evaluate
```

Save the harness report next to the predictions and commit both, so later runs
have a reference. `.gitignore` ignores `benchmarks/swe-bench/` but not
`benchmarks/baseline-*`:

```bash
mkdir -p benchmarks
cp benchmarks/swe-bench/baseline-v0.4.2/predictions.jsonl \
   benchmarks/baseline-v0.4.2.predictions.jsonl
cp benchmarks/swe-bench/baseline-v0.4.2/meldra.baseline-v0.4.2.json \
   benchmarks/baseline-v0.4.2.report.json
git add benchmarks/baseline-v0.4.2.*
git commit -m "docs(docs): record SWE-bench Lite baseline for v0.4.2"
```

The harness names its report `<model_name>.<run_id>.json`; adjust the copy if
you run it with a different model name.

### Cost Estimate

Cost scales with the number of instances, the model, and how long each turn
runs. Measure one 5-task sample with your configured model and multiply before
committing to a 300-task run.

## RSI: Recursive Self-Improvement

### The Loop

```
1. Run benchmark → Establish baseline
2. Analyze failures → Identify weakness
3. Agent proposes fix → Implement change
4. Re-run benchmark → Verify improvement
5. If better → commit; else rollback
6. Update baseline
7. Repeat
```

### Example RSI Cycle

**Step 1: Identify weakness**

```bash
python3 scripts/swe-bench-lite.py --binary dist/meldra --max-tasks 50 --pull --evaluate
# Result: 10% resolve rate
# Analysis: fails on multi-file changes
```

**Step 2: Agent proposes improvement**

```
Meldra self-analysis:
"I often miss related files. Improvement: before editing,
 search for imports and callers of modified functions."
```

**Step 3: Implement and verify**

```bash
# Agent implements the improvement in internal/app/...
make check  # tests still pass

# Re-benchmark
python3 scripts/swe-bench-lite.py --binary dist/meldra --max-tasks 50 --pull --evaluate
# Result: 12% resolve rate (+2%)
```

**Step 4: Accept or reject**

```bash
# Improvement confirmed
git add internal/app/search_context.go
git commit -m "feat(agent): search related files before editing"

# Update baseline for the next cycle
```

### Metrics to Track

```json
{
  "version": "v0.4.3",
  "resolve_rate": 12.0,
  "delta_from_baseline": 2.0,
  "failure_categories": {
    "multi_file_changes": 15,
    "test_understanding": 8,
    "timeout": 2
  },
  "next_focus": "test_understanding"
}
```

### Guardrails

- Always run `make check` before benchmarking
- Keep baseline files in git for comparison
- Reject changes that reduce resolve rate
- Document what each improvement targets
- Do not overfit to the benchmark; check on a holdout set

## Performance Benchmarking (Go)

Go benchmarks track **system performance**, not agent capability.

### Run Benchmarks

```bash
# All benchmarks
go test -bench=. -benchmem ./internal/app ./internal/store

# Specific
go test -bench=BenchmarkSearchFiles_SmallRepo -benchmem ./internal/app
```

### Current Baselines (M4 Pro)

```
BenchmarkSearchFiles_SmallRepo-12    970    123542 ns/op    100274 B/op    480 allocs/op
BenchmarkSearchFiles_LargeRepo-12    435    241503 ns/op    222807 B/op   1573 allocs/op
BenchmarkToolLifecycle-12            142    881191 ns/op    139969 B/op   1633 allocs/op
BenchmarkEventAppend-12              974    105480 ns/op     19397 B/op    190 allocs/op
```

Numbers come from the benchmark files under `internal/app` and
`internal/store`; rerun the commands above on your machine rather than assuming
the same figures.

### What to Monitor

- **No regression**: new code should not be significantly slower
- **Allocation count**: lower is better (less GC pressure)
- **Before/after comparison**:

  ```bash
  go test -bench=BenchmarkSearchFiles -benchmem ./internal/app > old.txt
  # Make changes
  go test -bench=BenchmarkSearchFiles -benchmem ./internal/app > new.txt
  go install golang.org/x/perf/cmd/benchstat@latest
  benchstat old.txt new.txt
  ```

## Integration with CI

### Daily Benchmark (GitHub Actions)

This example generates predictions only. Add the repository secret
`OPENAI_API_KEY` for model access. Configure `OPENAI_MODEL` and
`OPENAI_BASE_URL` as environment variables too if you use a custom provider.
Hosted evaluation additionally needs `SWEBENCH_API_KEY` and `--submit`;
local evaluation needs Docker and `--pull --evaluate`.

```yaml
name: Daily Benchmark

on:
  schedule:
    - cron: '0 2 * * *'  # 2 AM daily
  workflow_dispatch:

jobs:
  benchmark:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - uses: actions/setup-python@v5
        with:
          python-version: '3.11'

      - name: Install dependencies
        run: |
          pip install swebench sb-cli typing-extensions
          CGO_ENABLED=0 make build

      - name: Run SWE-bench Lite (sample)
        env:
          OPENAI_API_KEY: ${{ secrets.OPENAI_API_KEY }}
          MELDRA_HOME: ${{ runner.temp }}/meldra-benchmark
          MELDRA_DISABLE_UPDATE_CHECK: '1'
        run: python3 scripts/swe-bench-lite.py --binary dist/meldra --max-tasks 10

      - name: Upload results
        if: always()
        uses: actions/upload-artifact@v4
        with:
          name: benchmark-results
          path: benchmarks/swe-bench/
```

### Performance Regression Check

Commit a baseline first, then compare against it:

```bash
# One-time: record the baseline
go test -bench=. -benchmem ./internal/... > benchmarks/baseline-bench.txt
git add benchmarks/baseline-bench.txt
```

```yaml
# In .github/workflows/ci.yml
- name: Run benchmarks
  run: go test -bench=. -benchmem ./internal/... > bench.txt

- name: Check for regression
  run: |
    go install golang.org/x/perf/cmd/benchstat@latest
    if [ -f benchmarks/baseline-bench.txt ]; then
      benchstat benchmarks/baseline-bench.txt bench.txt
    else
      echo "No committed baseline; skipping comparison"
    fi
```

`benchstat` reports deltas with significance instead of asserting a pass/fail
threshold, so review its output rather than gating blindly on it.

## FAQ

**Q: Why not use HumanEval?**
A: It is too simple to reflect real-world complexity. SWE-bench uses real
issues with real test suites.

**Q: Can I use other benchmarks?**
A: Yes. Consider Aider's polyglot benchmark, APPS, or CodeContests.

**Q: How often to re-benchmark?**
A: Daily smoke test (10 tasks), weekly sample (50), monthly full (300).

**Q: What if resolve rate drops?**
A: Revert the change. RSI only accepts improvements.

**Q: How to avoid overfitting?**
A: Keep a holdout set (for example 50 tasks never used for iteration).

## Next Steps

1. **Today**: run 5 tasks and verify the pipeline works
2. **This week**: establish a 50-task baseline
3. **This month**: complete the first RSI improvement cycle
4. **Ongoing**: track metrics and iterate

**Start now**:

```bash
pip install swebench sb-cli typing-extensions
python3 scripts/swe-bench-lite.py --binary dist/meldra --max-tasks 5
```
