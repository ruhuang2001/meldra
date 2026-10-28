---
title: "SWE-bench Setup Guide"
description: "Register for SWE-bench hosted evaluation and configure multimodal input"
---

## SWE-bench Hosted Evaluation (sb-cli)

> **Unreleased.** The Meldra adapter described here is repository development
> tooling. Run it from this checkout with an explicitly selected binary.

Install the CLI and its missing runtime dependency:

```bash
python3 -m pip install sb-cli typing-extensions
```

`sb-cli` 0.1.5 installs without `typing-extensions`, but its command fails at
startup in a clean environment because the package does not declare that
dependency.

### Registration

The commands below match `sb-cli` 0.1.5. Request a key for your email address:

```bash
sb-cli gen-api-key you@example.com
```

The service emails an API key and a verification code. Set the key in your
shell, then verify it with the emailed code:

```bash
export SWEBENCH_API_KEY='replace-with-your-api-key'
sb-cli verify-api-key 'replace-with-your-verification-code'
```

`SWEBENCH_API_KEY` authenticates hosted evaluation; Meldra separately needs
its model-provider credentials (`OPENAI_API_KEY` and any provider overrides).
See [Configuration](configuration.md).

### Usage

Run from the repository root after `CGO_ENABLED=0 make build`:

```bash
# Submit to hosted evaluation (no local Docker required)
python3 scripts/swe-bench-lite.py \
    --binary dist/meldra \
    --max-tasks 50 \
    --run-id hosted-baseline \
    --submit

# List submitted runs for the same subset and split
sb-cli list-runs swe-bench_lite test

# Retrieve the report for the submitted run
sb-cli get-report swe-bench_lite test hosted-baseline \
    --output_dir sb-cli-reports
```

`sb-cli submit` waits for evaluation and generates a report by default.
`get-report` retrieves a run's report; `status` and `download` are not commands
in `sb-cli` 0.1.5. Use the same subset, split, and run ID when retrieving results.
For `--subset verified`, use `swe-bench_verified test` in the direct CLI commands.
Check your account's remaining hosted quota with:

```bash
sb-cli get-quotas
```

Hosted evaluation does not need local Docker. Patch generation still runs
locally, and the model provider is billed separately. Hosted availability and
quotas are controlled by the service.

### When to Use Hosted vs Local

| Scenario | Recommended | Why |
|----------|-------------|-----|
| Development and debugging | Local (`--pull --evaluate`) | Inspect local harness logs and containers |
| Establishing a baseline | Either, with a fixed subset and run ID | Retain predictions and the evaluator report |
| Evaluation without local Docker | Hosted (`--submit`) | Run the evaluation through the hosted service |
| CI integration | Either | Configure model credentials and the chosen evaluator |

## SWE-bench-m (Multimodal Support)

> **Unreleased.** The provider-level image input below is not wired into the
> CLI, TUI, or the Meldra agent loop yet, and no released version includes it.

### What is SWE-bench-m?

SWE-bench-m is a multimodal variant of SWE-bench whose tasks need **image
input**, for example screenshot-driven UI bug fixes, chart generation, and
visualization issues.

### Meldra Multimodal Support

The provider package exposes a constructor for user messages that mix text and
images.

#### API

```go
// internal/provider/conversation.go

// ImageContent represents an image input for multimodal requests.
type ImageContent struct {
    URL    string // https://, http://, or a data: URL carrying base64 image data
    Detail string // "low", "high", "auto", or "original"; empty selects "auto"
}

// UserItemsWithImages fails locally for images without a URL and for messages
// with no content, instead of emitting an uninterpretable request.
func UserItemsWithImages(text string, images []ImageContent) (Items, error)
```

#### Example Usage

```go
import "meldra/internal/provider"

// Image from a URL
items, err := provider.UserItemsWithImages(
    "Fix the UI bug shown in the screenshot",
    []provider.ImageContent{{URL: "https://example.com/screenshot.png", Detail: "high"}},
)
if err != nil {
    return err
}

// Image from local bytes, as a base64 data URL
imageData := base64.StdEncoding.EncodeToString(imageBytes)
items, err = provider.UserItemsWithImages(
    "Describe what is broken here",
    []provider.ImageContent{{URL: "data:image/png;base64," + imageData, Detail: "high"}},
)
```

The `URL` field accepts a plain `http(s)` URL or a `data:` URL with inline
base64 data; there is no separate base64 field.

#### SWE-bench-m Integration

Still to do before this reaches users:

- [ ] Accept image input from the CLI and TUI
- [ ] Wire image content through the agent turn builder in `internal/app`
- [ ] Write `scripts/swe-bench-m.py` to pull images from the dataset
- [ ] End-to-end tests against a mocking provider

## Troubleshooting

### Empty Patches and Failed Attempts

A successful Meldra run can produce an empty patch. This is a legitimate
attempt: it stays in `predictions.jsonl` and counts in the benchmark denominator.
An empty patch alone is not an infrastructure error.

Checkout failures, agent failures, timeouts, and diff failures are recorded in
`attempts.jsonl`. These failed attempts are excluded from `predictions.jsonl`,
even when the workspace contains a partial patch. Any instance failure makes
the batch exit nonzero and skip evaluation or submission.

Inspect the attempt records and `agent-logs/`, fix the underlying error, then
rerun with the same `--run-id` to retry failed instances. Successful predictions
are retained. Resume atomically rewrites the predictions file so a damaged
trailing record cannot corrupt a newly appended prediction.

### Streaming Timeout

**Symptom**: `response stream was idle for 90s without a terminal event`.

**Resolution**: this means the configured gateway did not finish a Responses
stream. Point Meldra at a gateway with Responses streaming support, or increase
the provider timeout at the gateway. The provider reports the condition as an
error so a failed turn is never mistaken for a finished one.

### Agent Deadline

The adapter allows each Meldra process 10 minutes by default (`--agent-timeout
600`). On timeout, it sends SIGTERM and allows 15 seconds by default
(`--termination-grace 15`) for Meldra to cancel subprocesses and clean up before
force-killing remaining processes. Both options take positive seconds. The
timeout is recorded as a failed attempt, so a partial patch is not submitted as
a successful run.

### Docker Image Pull Failures

Use `--pull` to fetch the selected instance images before local evaluation.
`--mirror` changes the pull source; it does not enable pulling by itself.

```bash
# Use a mirror (the default)
python3 scripts/swe-bench-lite.py --binary dist/meldra --pull --evaluate \
    --mirror docker.1ms.run

# Or pull from Docker Hub directly
python3 scripts/swe-bench-lite.py --binary dist/meldra --pull --evaluate \
    --mirror ""

# Pull selected images with more parallelism, then generate and evaluate
python3 scripts/swe-bench-lite.py --binary dist/meldra --pull --evaluate \
    --pull-workers 16
```

### Commands Are Declined During a Benchmark Run

The script passes `--auto-approve` by default because stdin is closed. Without
it, command approvals read EOF and are declined. Each instance uses a temporary
checkout on the host, and commands run with the host user's permissions.
Automatic approval remains enabled for host runs; there is no extra opt-in or
container requirement. Docker is needed only for local harness evaluation.

## References

- [SWE-bench website](https://www.swebench.com)
- [SWE-bench GitHub](https://github.com/princeton-nlp/SWE-bench)
- [sb-cli documentation](https://github.com/swe-bench/sb-cli)
- [Leaderboard](https://www.swebench.com/leaderboard)
- **Meldra benchmarking guide**: [Benchmarking](benchmarking.md)
