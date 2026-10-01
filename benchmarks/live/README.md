# Fixed 0.2.0 release quality cohort

This is a fixed five-task evaluation harness. It prepares independent workspaces,
validates an explicitly priced plan, runs Meldra in Docker through a host-owned
budget gateway, and grades candidate files with hidden tests in fresh containers.
The harness is implemented and tested with a scripted local upstream; **no live
model quality score has been recorded**. Paid execution requires approval of the
exact provider, model, cohort, rates and total budget before using `execute.py`.

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
EVAL_WORKDIR=$(mktemp -d /tmp/meldra-release-quality-XXXXXX)
python3 benchmarks/live/run.py prepare \
  --output "$EVAL_WORKDIR/candidate"
```

`prepare` refuses an existing run directory. It copies only starter files into
each task's `workspace`; the sibling `prompt.txt` contains its exact prompt.
No hidden grader or previous submission is copied into Agent workspaces.
Prepared workspaces must be outside the repository; generated Go sources must
not enter Meldra's recursive formatting checks. Every Go workspace has its own
module. JSON reports can remain under the ignored `benchmarks/results/` directory.

## Plan the approved budget

Record the exact provider and model, current input/output prices in USD per
million tokens, one attempt per task, and the user's approved dollar cap. The
following variables must be explicitly supplied; there is no assumed pricing:

```sh
python3 benchmarks/live/run.py plan \
  --model "$EVAL_MODEL" --provider "$EVAL_PROVIDER" \
  --input-price "$EVAL_INPUT_PRICE" --output-price "$EVAL_OUTPUT_PRICE" \
  --count-price-per-request "$EVAL_COUNT_PRICE" --max-requests 100 \
  --token-budget 10000 --max-output-tokens 1000 --max-cost 5 \
  > "$EVAL_WORKDIR/approved-plan.json"
```

The schema-2 plan's conservative upper bound is
`5 × tokens_per_task × max(input_price, output_price) / 1,000,000 +
5 × max_requests × count_price_per_request`. The counting endpoint rate must be
known; an explicit zero means the operator verified it is uncharged. There are
no assumed model prices or cache discounts. Input/output rates must cover all
applicable billable token classes, including cache writes and reasoning. The
plan itself does not execute or approve anything (`budget_enforced: false`).
Old schema-1 plans are rejected. Monetary input precision is bounded; arithmetic
uses an independent 64-digit decimal context rather than ambient rounding.

The gateway admits only the fixed model, replayed text context and client-side
function tools. It rejects hosted paid tools, external conversation IDs, custom
service tiers and other unpriced features. For each generation it:

1. Reserves the counting endpoint fee before making that request.
2. Calls `POST /responses/input_tokens` with the generation input and tool schema.
3. Reserves counted input plus configured maximum output tokens before sending
   `POST /responses`. If either token or money allowance is insufficient, it stops.
4. Retains the entire reservation even on HTTP errors, disconnects, missing usage
   or truncation. Each client retry goes through a new admission. No upstream
   retries or redirects occur inside the gateway.

`max_output_tokens` covers visible and reasoning tokens. `store=false` and
`reasoning.encrypted_content` make continuation explicit; encrypted reasoning
is replayed inside the attempt, never published as grading evidence. Unknown
input counts abort before generation. Usage beyond a reservation stops the
cohort and marks the provider contract violated. The cap is conditional on the
provider honoring its count/output-limit contract and the operator supplying
correct upper-bound prices; it is not an account-wide billing guarantee.

Official API contracts:
[Responses create](https://developers.openai.com/api/reference/resources/responses/methods/create),
[input token counts](https://developers.openai.com/api/reference/resources/responses/subresources/input_tokens/methods/count).

## Execute an approved plan

Build a Linux binary matching the trusted Docker image architecture, then use a
fresh destination outside the repository. `execute.py` performs preparation;
do not point it at an already-prepared/reused directory.

```sh
# Example for an arm64 Docker engine; use amd64 on an amd64 engine.
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o /tmp/meldra-eval .
# Explicitly provide EVAL_API_KEY through your preferred secure environment.
python3 benchmarks/live/execute.py --execute-live \
  --approved-plan "$EVAL_WORKDIR/approved-plan.json" --api-key-env EVAL_API_KEY \
  --binary /tmp/meldra-eval --image meldra-release-eval:go1.26.6 \
  --run-dir "$EVAL_WORKDIR/live-candidate" \
  --output benchmarks/results/live-candidate.json --timeout 600
```

The explicit key variable is never discovered from `~/.meldra` or forwarded to
Docker. A per-attempt nonce authorizes only the temporary host gateway. A
loopback relay inside Docker preserves Meldra's loopback-only HTTP policy;
it does not weaken production provider URL validation. The gateway listens
only during an attempt but binds a host-reachable interface for Docker Desktop;
it authenticates every request with that nonce. It accepts no arbitrary upstream
URL, and JSON logs/metadata exclude the key and nonce.

Each attempt records its reserved cost, provider-reported tokens when available,
exit status, duration, tool counts and independent grader result. Actual billed
cost remains `null`: upper-bound rates and usage are not a billing receipt.
Reported token sums remain `null` if any generation lacks usage. The full five
cases remain the denominator, including failures and skipped attempts. A
failed cleanup prevents further attempts and grading until the container is
confirmed stopped. The runner handles Ctrl-C, SIGTERM and SIGHUP; SIGKILL or a
Docker daemon outage can still require manual container cleanup. Runs cannot
be resumed or overwritten; a new attempt requires its own approval/budget.

The gateway buffers a bounded complete response and imposes a 60-second
upstream deadline before Meldra's 90-second idle watchdog. This harness measures
**gateway-mediated task elapsed time**, not native stream latency or time to
first token. Timeouts remain failed attempts with nonrefunded reservations.

For regression evidence run the same cohort against the selected baseline and
candidate binary/model. That is **ten attempts in total**, not five. The agreed
total budget must cover both runs; allocate two plans whose caps sum to at most
the approved total. Do not infer permission for the second run from a candidate-
only approval. Compare task identities and fixture hashes before classifying
newly failing tasks; the existing SanityHarness comparator is a different schema.

## Agent isolation contract

The approved runner executes each Linux Meldra binary **inside** a fresh Docker container. Mount only that task's
workspace read/write and the binary read-only; use a read-only root filesystem,
a private `/tmp`, explicit CPU/memory/PID limits, dropped capabilities and
`no-new-privileges`. Supply only explicitly approved model credentials and
configuration. Never mount the Docker socket, host home/configuration, repository
or grader directories. Never invoke host `meldra --auto-approve`. Agent network access is
limited by the execution environment; a normal Docker bridge is not a domain
allowlist and must not be described as one.

Record image ID, binary SHA-256/revision, exact model/provider, fixture hash,
attempt identity, elapsed time, tool calls and reported token use for every
attempt. Missing usage/cost stays `null`; an interrupted or failed attempt
remains in the denominator. Preserve logs privately and exclude credentials.
The runner does not automatically read or forward `~/.meldra` credentials.

## Independent Docker grading

Build the toolchain-only image (this does not call a model):

```sh
docker build -t meldra-release-eval:go1.26.6 benchmarks/live
python3 benchmarks/live/run.py grade \
  --run-dir "$EVAL_WORKDIR/candidate" \
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

## Repeat the fully offline Docker smoke test

```sh
python3 benchmarks/live/smoke.py --binary /tmp/meldra-eval \
  --image meldra-release-eval:go1.26.6 \
  --output benchmarks/results/live-budget-offline-smoke.json
```

This uses authored repairs as scripted responses, the real Linux Meldra binary,
real file tools, the HTTP gateway/relay and Docker graders. It checks encrypted
reasoning replay and completed tool output round-trips. It never reads a real
key or contacts a provider, and its report explicitly has `live_model: false`
and `model_quality_evidence: false`. Failed smoke logs are preserved beside the
report for diagnosis. On 2026-10-01 the complete path passed 5/5; this is harness
validation only. The output report is not a substitute for approved live runs.
