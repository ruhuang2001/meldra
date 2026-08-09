# SanityHarness benchmark for Meldra

This directory configures [SanityHarness](https://github.com/lemon07r/SanityHarness) to evaluate Meldra on a compact set of coding tasks.

## Requirements

- Go 1.26+
- Docker running locally
- `OPENAI_API_KEY` exported in your environment
- `sanity` CLI installed from [SanityHarness](https://github.com/lemon07r/SanityHarness)

## Run

From the repository root:

```bash
make benchmark-sanity
```

This builds the current `meldra` binary and runs the core SanityHarness tasks:

```bash
sanity --config benchmarks/sanityharness/sanity.toml eval --agent meldra --tier core
```

## Results

After evaluation finishes, inspect:

```bash
ls benchmarks/sanityharness/eval-results/
cat benchmarks/sanityharness/eval-results/*/summary.json
```

`summary.json` contains the weighted score, per-task results, and pass counts.

## Release Baselines

For an important release, record one fresh evaluation as a small, reviewable
JSON baseline. It captures the Meldra version, model, provider, date, task
outcomes, elapsed time, and a caller-supplied estimated cost without copying
prompts, raw model output, or task source code.

```bash
make benchmark-sanity-baseline \
  SANITY_MODEL=gpt-5.6-luna \
  SANITY_PROVIDER=openai \
  SANITY_COST='$1.23' \
  SANITY_BASELINE=benchmarks/sanityharness/baselines/v0.1.0-alpha.4.json
```

The recorder accepts one `result.json` per language/task pair. It deliberately
rejects duplicate attempts, which prevents old sessions from being mixed into a
new score. Start from a fresh SanityHarness session directory or set
`SANITY_RESULTS` to the explicit result files from one run. Review and commit
the generated baseline only when its version, model, provider, task set, and
cost are known. Existing ignored local session artifacts are not a formal
baseline.

## Customization

Edit `sanity.toml` to change the model or timeout. The agent is configured to
run from SanityHarness's isolated task workspace with `--yes --prompt "{prompt}"`.
It deliberately does not pass `--workspace /workspace`: `/workspace` is the
Docker validation path, while the agent runs in a separate temporary workspace.

The configuration sets `MELDRA_HOME=/tmp/meldra`, so each task gets ephemeral
Meldra sessions and no host `~/.meldra` directory is mounted into the sandbox.
Export `OPENAI_API_KEY` before running the benchmark; a key stored only in
`~/.meldra/credentials.env` is intentionally not used.

To run all tasks (including extended tier):

```bash
sanity --config benchmarks/sanityharness/sanity.toml eval --agent meldra --tier all --parallel 2
```
