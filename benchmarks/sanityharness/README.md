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
