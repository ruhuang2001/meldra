# SanityHarness benchmark for Meldra

This directory configures [SanityHarness](https://github.com/lemon07r/SanityHarness) to evaluate Meldra on a compact set of coding tasks.

## Requirements

- Go 1.25+
- Docker running locally
- `OPENAI_API_KEY` exported in your environment (or the agent will fail to authenticate)
- Meldra built at the repository root (`go build -o meldra .`)

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
run with `--workspace /workspace --yes --prompt "{prompt}"`, so it auto-approves
tool operations inside the harness container.

To run all tasks (including extended tier):

```bash
sanity --config benchmarks/sanityharness/sanity.toml eval --agent meldra --tier all --parallel 2
```
