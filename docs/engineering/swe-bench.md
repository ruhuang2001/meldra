---
title: "SWE-bench runs"
description: "Generate patches for SWE-bench tasks, submit them for evaluation, and keep the evidence."
---

> **Repository tooling (v0.5.0).** The adapter described here is development
> tooling. Run it from this checkout with an explicitly selected binary; it is
> not part of the published Meldra binary.

## Generate and evaluate patches

`scripts/swe-bench-lite.py` generates patches and can invoke the official
evaluator. Configure the model provider and `OPENAI_API_KEY` first; see
[Configuration](../reference/configuration.md). Run these commands from the
repository root.

```bash
python3 -m pip install swebench sb-cli typing-extensions
CGO_ENABLED=0 make build
python3 scripts/swe-bench-lite.py --binary dist/meldra --max-tasks 5
```

The script prints one line per instance as patches land:

```text
[1/5] django__django-11099: empty patch, 42.1s
[2/5] django__django-11133: 812 byte patch, 96.4s
Predictions: benchmarks/swe-bench/meldra-<timestamp>/predictions.jsonl
```

A resolve rate appears only after `--evaluate`, once the official harness has
run the project's tests.

### How it works

1. Loads the SWE-bench subset from HuggingFace.
2. Clones each repository at `base_commit` into a temporary directory, runs
   Meldra with the problem statement and `--auto-approve`, stages the result,
   and captures the diff.
3. Records every attempt in `attempts.jsonl`, including agent errors and partial
   patches. Only successful attempts enter `predictions.jsonl`; a successful
   empty patch is still a legitimate prediction.
4. Resumes with the same `--run-id`: successful predictions are retained, failed
   instances are retried, and the predictions file is rewritten atomically.

Generation runs on the host as your own user with automatic approval enabled by
default, because stdin is closed and command approvals would otherwise read EOF.
Each checkout is temporary; Docker is needed only for local evaluation.

Verification is a separate step: `--pull --evaluate` fetches the selected images
and runs the official harness in Docker, while `--submit` sends predictions to
hosted evaluation.

### Establish a baseline

```bash
python3 scripts/swe-bench-lite.py --binary dist/meldra --max-tasks 50 \
    --run-id baseline-v0.5.0 --pull --evaluate
```

Save the harness report next to the predictions and commit both so later runs
have a reference. `.gitignore` ignores `benchmarks/swe-bench/` but not
`benchmarks/baseline-*`:

```bash
mkdir -p benchmarks
cp benchmarks/swe-bench/baseline-v0.5.0/predictions.jsonl \
   benchmarks/baseline-v0.5.0.predictions.jsonl
cp benchmarks/swe-bench/baseline-v0.5.0/meldra.baseline-v0.5.0.json \
   benchmarks/baseline-v0.5.0.report.json
git add benchmarks/baseline-v0.5.0.*
git commit -m "docs(docs): record SWE-bench Lite baseline for v0.5.0"
```

The harness names its report `<model_name>.<run_id>.json`; adjust the copy if you
run it with a different model name.

Cost scales with instance count, model, and turn length. Measure one 5-task
sample with your configured model and multiply before committing to a long run.

### Offline adapter verification

The adapter regression suite runs in `make check` and `make check-fast` without
network access or model credentials. To run it alone:

```bash
python3 scripts/swe-bench-adapter-e2e.py --output .artifacts/adapter-e2e
```

Use a fresh output directory. It retains `report.json`, invocation logs,
predictions, attempt records, and process-cleanup evidence.

## Hosted evaluation with sb-cli

```bash
python3 -m pip install sb-cli typing-extensions
```

`sb-cli` 0.1.5 installs without `typing-extensions`, but its command fails at
startup in a clean environment because the package does not declare that
dependency.

### Registration

Request a key for your email address:

```bash
sb-cli gen-api-key you@example.com
```

The service emails an API key and a verification code. Set the key in your shell,
then verify it with the emailed code:

```bash
export SWEBENCH_API_KEY="..."
sb-cli verify-api-key --code <code>
```

Hosted submission also needs the key in the environment when you pass `--submit`.

## SWE-bench-m (multimodal)

> **Available since v0.5.0.** CLI and TUI turns accept local PNG/JPEG references
> and preserve image snapshots. See
> [image references](../guides/project-context.md#attach-an-image).
> The dedicated SWE-bench-m dataset adapter remains unimplemented.

SWE-bench-m is a multimodal variant whose tasks need image input, for example
screenshot-driven UI fixes, chart generation, and visualization issues.

Integration status:

- [x] Accept local image references from the CLI and TUI
- [x] Wire image content through the agent turn builder
- [ ] Write `scripts/swe-bench-m.py` to pull images from the dataset
- [x] End-to-end image-reference tests against a mock provider

## Troubleshooting

### Empty patches and failed attempts

A successful Meldra run can produce an empty patch. It stays in
`predictions.jsonl` and counts in the denominator; an empty patch alone is not an
infrastructure error.

Checkout failures, agent failures, timeouts, and diff failures are recorded in
`attempts.jsonl` and excluded from `predictions.jsonl`, even when the workspace
holds a partial patch. Any instance failure makes the batch exit nonzero and skip
evaluation or submission. Inspect the attempt records and `agent-logs/`, fix the
cause, then rerun with the same `--run-id` to retry only the failures.

### Streaming timeout

`response stream was idle for 90s without a terminal event` means the configured
gateway did not finish a Responses stream. Point Meldra at a gateway with
streaming support or raise the timeout there; the failure is reported rather than
mistaken for a finished turn.

### Agent deadline

Each Meldra process gets 10 minutes by default (`--agent-timeout 600`). On
timeout the adapter sends SIGTERM and allows 15 seconds (`--termination-grace 15`)
for Meldra to cancel subprocesses before force-killing them. Both options take
positive seconds.

### Docker image pulls

Use `--pull` to fetch the selected images before local evaluation. `--mirror`
changes the pull source; it does not enable pulling by itself.

```bash
python3 scripts/swe-bench-lite.py --binary dist/meldra --pull --evaluate \
    --mirror docker.1ms.run
python3 scripts/swe-bench-lite.py --binary dist/meldra --pull --evaluate \
    --pull-workers 16
```

## Go performance benchmarks

Go benchmarks track system performance, not agent capability:

```bash
go test -bench=. -benchmem ./internal/app ./internal/store
go test -bench=BenchmarkSearchFiles -benchmem ./internal/app > old.txt
# make the change
go test -bench=BenchmarkSearchFiles -benchmem ./internal/app > new.txt
go install golang.org/x/perf/cmd/benchstat@latest
benchstat old.txt new.txt
```

Compare with `benchstat` on your own machine rather than quoting figures here;
allocate fewer bytes and allocations per operation where the code allows it.

## References

- [SWE-bench](https://www.swebench.com) · [source](https://github.com/princeton-nlp/SWE-bench)
- [sb-cli](https://github.com/swe-bench/sb-cli) · [leaderboard](https://www.swebench.com/leaderboard)
- [Test gates and local checks](./development.md)
