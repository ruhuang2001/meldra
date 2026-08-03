# SWE-bench evaluation harness for Meldra

This directory contains a lightweight harness that runs Meldra against the
[SWE-bench](https://www.swebench.com/) dataset and produces a
`predictions.jsonl` file suitable for official evaluation.

## Requirements

- Go 1.26+ (to build Meldra)
- Python 3.10+ with `datasets` installed:
  ```bash
  pip install -r benchmarks/swe-bench/requirements.txt
  ```
- Meldra built at `dist/meldra` (run `make build`)
- `OPENAI_API_KEY` exported in your environment, or configured in
  `$MELDRA_HOME/credentials.env` (default: `~/.meldra/credentials.env`)
- `git`

## Generate predictions

Run on the full SWE-bench Lite test split:

```bash
export OPENAI_API_KEY=...
make build
python3 benchmarks/swe-bench/run.py
```

The output is written to `benchmarks/swe-bench/predictions.jsonl`.

The runner uses `dist/meldra` by default, then falls back to `meldra` on your
`PATH`. To use another build explicitly:

```bash
python3 benchmarks/swe-bench/run.py --meldra-bin /path/to/meldra
```

### Dry run on a small subset

```bash
python3 benchmarks/swe-bench/run.py --max-instances 5
```

## Evaluate predictions

### Cloud evaluation via sb-cli (recommended)

```bash
pip install sb-cli
sb login

sb-cli submit swe-bench_lite test \
  --predictions_path benchmarks/swe-bench/predictions.jsonl \
  --run_id meldra-$(date +%s)

sb-cli get-report swe-bench_lite test <run_id> -o benchmarks/swe-bench/reports
```

### Local evaluation (requires Docker)

```bash
python -m swebench.harness.run_evaluation \
  --dataset_name princeton-nlp/SWE-bench_Lite \
  --predictions_path benchmarks/swe-bench/predictions.jsonl \
  --max_workers 4 \
  --run_id meldra-local
```

On Apple Silicon macOS, add `--namespace ''` so images are built locally instead of pulled for x86_64:

```bash
python -m swebench.harness.run_evaluation \
  --dataset_name princeton-nlp/SWE-bench_Lite \
  --predictions_path benchmarks/swe-bench/predictions.jsonl \
  --max_workers 4 \
  --namespace '' \
  --run_id meldra-local
```

## How it works

1. Loads the SWE-bench dataset from HuggingFace.
2. For each instance:
   - Clones the target repository and checks out the `base_commit`.
   - Runs the selected Meldra binary with `--workspace <repo> --yes --prompt "<issue>"`.
   - Extracts tracked changes and untracked files as `model_patch`.
3. Writes one prediction per line to `predictions.jsonl`.

## Limitations

- Meldra can run workspace-local `python3 -m pytest` tests and supported Go
  checks, but its command allowlist can still prevent language-specific test
  commands. The official SWE-bench harness validates every generated patch
  externally.
- Some repositories require a full clone if a shallow fetch of `base_commit` fails.
