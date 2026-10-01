# Fixed 0.2.0 release quality cohort

This is a five-task **evaluation scaffold**, not a recorded model score. It
prepares small independent workspaces, plans a priced token reservation, and
grades candidate files using hidden tests in Docker. It makes no model requests
and deliberately has no live execution command until Meldra can enforce the
approved cumulative token budget before admitting each request.

| Task | Language | Capability | External acceptance |
| --- | --- | --- | --- |
| `go-clamp` | Go | Correct a boundary bug | Inclusive, negative and singleton ranges |
| `go-chunks` | Go | Coordinate two source files | Size normalization, remainder, empty input, unchanged input |
| `python-slug` | Python | Implement a precise text contract | ASCII policy, repeated separators, punctuation and Unicode |
| `python-ledger` | Python | Repair parsing and its caller | Exact money arithmetic, invalid input, voided records |
| `python-config` | Python | Recursive data transformation | Nested merge, type replacement, independent copies |

Each task gets one attempt. Prompts and allowed edits are frozen in `cohort.json`.
The manifest hash includes the prompts, starter files and hidden graders.
Change the suite version when changing its semantics. Go templates have `.txt`
suffixes so intentional bugs and hidden grader packages do not enter `go test
./...` for Meldra itself. All fixture code uses the standard library.

## Why a separate small cohort

The installed SanityHarness `v1.8.12-1-g34ba88c` has hard/expert tasks, and its
task loader does not accept Python. Its Docker validation does not by itself
mean that the Agent process runs inside Docker. Therefore the existing
SanityHarness pipeline remains a separate evaluation; these five small tasks
are not mislabeled as SanityHarness results and are not accepted by
`benchmarks/run.py compare-quality` as if they had the same schema or dataset.

## Prepare without API access

From the repository root:

```sh
python3 -m unittest discover -s benchmarks/live -p test_run.py -v
python3 benchmarks/live/run.py prepare \
  --output benchmarks/results/release-quality-candidate
```

`prepare` refuses an existing run directory. It copies only starter files into
each task's `workspace`; the sibling `prompt.txt` contains its exact prompt.
No hidden grader or previous submission is copied into Agent workspaces.

## Plan the approved budget

Record the exact provider and model, current input/output prices in USD per
million tokens, one attempt per task, and the user's approved dollar cap. The
following variables must be explicitly supplied; there is no assumed pricing:

```sh
python3 benchmarks/live/run.py plan \
  --model "$EVAL_MODEL" --provider "$EVAL_PROVIDER" \
  --input-price "$EVAL_INPUT_PRICE" --output-price "$EVAL_OUTPUT_PRICE" \
  --token-budget 10000 --max-output-tokens 1000 --max-cost 5
```

The conditional upper bound is `5 × tokens_per_task × max(input_price,
output_price) / 1,000,000`. It conservatively treats every admitted token as the
more expensive class, without assuming prompt-cache discounts. It rejects
unknown, non-finite, zero or negative prices, invalid caps, and reservations
above the approved cost. This is a **plan**, and always reports
`budget_enforced: false` and `live_execution_enabled: false`.

Before enabling live execution, the binary or a verified provider-side budget
mechanism must enforce the cumulative input/output allowance, including replayed
input, retries, failed responses and reasoning tokens. A timeout, response byte
limit, output-only token limit, or post-response usage check is insufficient.
Do not set `budget_enforced` merely because the user supplied a dollar amount.
Do not run models when price or token-counting semantics are unknown. Prices
must cover the selected provider's billable token classes; any other billable
features need a separate reservation.

The official Responses API defines `max_output_tokens` as covering visible
output and reasoning tokens:
[Responses create reference](https://developers.openai.com/api/reference/resources/responses/methods/create).
That cap does not bound input tokens or total spend across multiple requests.

## Agent isolation contract

Once the user approves the plan and enforceable limits exist, execute each
Linux Meldra binary **inside** a fresh Docker container. Mount only that task's
workspace read/write and the binary read-only; use a read-only root filesystem,
a private `/tmp`, explicit CPU/memory/PID limits, dropped capabilities and
`no-new-privileges`. Supply only explicitly approved model credentials and
configuration. Never mount the Docker socket, host home/configuration, repository
or grader directories. Never invoke host `meldra --yes`. Agent network access is
limited by the execution environment; a normal Docker bridge is not a domain
allowlist and must not be described as one.

Record image ID, binary SHA-256/revision, exact model/provider, fixture hash,
attempt identity, elapsed time, tool calls and reported token use for every
attempt. Missing usage/cost stays `null`; an interrupted or failed attempt
remains in the denominator. Preserve logs privately and exclude credentials.
The scaffold does not automatically read or forward `~/.meldra` credentials.

## Independent Docker grading

Build the toolchain-only image (this does not call a model):

```sh
docker build -t meldra-release-eval:go1.26.6 benchmarks/live
python3 benchmarks/live/run.py grade \
  --run-dir benchmarks/results/release-quality-candidate \
  --image meldra-release-eval:go1.26.6 \
  --output benchmarks/results/release-quality-candidate-grades.json
```

The grader resolves the image ID and runs each submission in a separate
container with `--network none`, read-only mounts, no credentials, private
temporary storage, resource limits and bounded logs/time. It copies candidate
files and trusted hidden tests into container-private scratch storage. Hidden
tests therefore cannot be changed by a preceding Agent run. Changed protected
files, new files and symlinks fail validation. Timeouts kill the grading
container, not merely its Docker client. Existing reports are not overwritten.

Every initial broken fixture is expected to fail. The local scaffold tests
confirm all five failures and separately confirm that independent reference
repairs pass their graders. Those self-tests run only authored fixture/reference
code on the host; model-generated submissions always go through Docker.

On 2026-10-01, the Docker grading path was also smoke-tested using image
`sha256:e279468f7e0732d9447c126a14036689dfecd8d1bfe0787f335eb5f48864a86e`:
the five original fixtures scored **0/5**, and the five authored reference
repairs scored **5/5**. No model or API was involved. Local evidence is retained
under `benchmarks/results/release-cohort-grader-check/`.

Grader success is independent of model prose. Its report intentionally says
`model_quality_evidence: false`: attach verified Agent attempt and model/budget
records before treating grades as live model evidence. This small cohort is a
release smoke test, not a broad coding benchmark or a SWE-bench result. A release
quality comparison also needs the same cohort executed against the chosen base
binary/model under the same attempt and budget policy.
