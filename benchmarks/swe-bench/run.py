#!/usr/bin/env python3
"""Generate SWE-bench predictions for Meldra.

Usage:
    export OPENAI_API_KEY=...
    go build -o meldra .
    python3 benchmarks/swe-bench/run.py --max-instances 5
    # Submit to cloud evaluation:
    sb-cli submit swe-bench_lite test \
        --predictions_path benchmarks/swe-bench/predictions.jsonl \
        --run_id meldra-$(date +%s)
"""

import argparse
import json
import os
import subprocess
import sys
import tempfile
from pathlib import Path


def run_command(cmd: list[str], cwd: Path, check: bool = True, timeout: int = 300) -> subprocess.CompletedProcess:
    return subprocess.run(
        cmd,
        cwd=str(cwd),
        check=check,
        capture_output=True,
        text=True,
        timeout=timeout,
    )


def checkout_instance(repo: str, base_commit: str, workspace: Path) -> None:
    """Clone the repository and checkout base_commit.

    Uses a shallow clone followed by a fetch of the specific commit, which is
    usually faster than a full clone for the SWE-bench corpus.
    """
    if workspace.exists():
        run_command(["rm", "-rf", str(workspace)], cwd=Path.cwd())
    workspace.parent.mkdir(parents=True, exist_ok=True)

    # Shallow clone the default branch first.
    run_command(
        ["git", "clone", "--depth=1", f"https://github.com/{repo}.git", str(workspace)],
        cwd=Path.cwd(),
        timeout=600,
    )

    # Try to fetch and checkout the specific commit. GitHub supports fetching
    # individual commits, but fall back to a full clone if this fails.
    try:
        run_command(["git", "fetch", "--depth=1", "origin", base_commit], cwd=workspace, timeout=300)
        run_command(["git", "checkout", base_commit], cwd=workspace, timeout=60)
    except subprocess.CalledProcessError:
        run_command(["rm", "-rf", str(workspace)], cwd=Path.cwd())
        run_command(
            ["git", "clone", f"https://github.com/{repo}.git", str(workspace)],
            cwd=Path.cwd(),
            timeout=900,
        )
        run_command(["git", "checkout", base_commit], cwd=workspace, timeout=60)


def run_meldra(workspace: Path, prompt: str, timeout: int) -> str:
    """Run Meldra on the workspace with the given prompt and return its output."""
    env = os.environ.copy()
    # Keep MELDRA_HOME if the caller set it; otherwise let Meldra default to
    # ~/.meldra so it can use the user's local credentials and model config.
    if "MELDRA_HOME" not in env:
        env["MELDRA_HOME"] = str(Path.home() / ".meldra")

    cmd = ["meldra", "--workspace", str(workspace), "--yes", "--prompt", prompt]
    result = subprocess.run(
        cmd,
        cwd=str(workspace),
        env=env,
        capture_output=True,
        text=True,
        timeout=timeout,
    )
    return result.stdout + result.stderr


def extract_patch(workspace: Path) -> str:
    """Return the working-tree diff produced by Meldra."""
    result = run_command(["git", "diff"], cwd=workspace, check=False, timeout=60)
    return result.stdout


def process_instance(instance: dict, work_root: Path, timeout: int) -> dict:
    instance_id = instance["instance_id"]
    repo = instance["repo"]
    base_commit = instance["base_commit"]
    problem_statement = instance["problem_statement"]

    workspace = work_root / instance_id.replace("/", "__")
    print(f"  checkout {repo}@{base_commit[:8]} -> {workspace}")
    checkout_instance(repo, base_commit, workspace)

    prompt = (
        "You are fixing a real issue in this repository. "
        "Read the relevant files, understand the problem, and apply the minimal correct fix. "
        "Run any available tests to verify your change.\n\n"
        f"Issue:\n{problem_statement}"
    )

    print(f"  running meldra on {instance_id}")
    run_meldra(workspace, prompt, timeout)

    patch = extract_patch(workspace)
    print(f"  patch size: {len(patch)} bytes")

    return {
        "instance_id": instance_id,
        "model_name_or_path": "meldra",
        "model_patch": patch,
    }


def main() -> int:
    parser = argparse.ArgumentParser(description="Generate SWE-bench predictions with Meldra")
    parser.add_argument("--dataset", default="princeton-nlp/SWE-bench_Lite", help="HuggingFace dataset name")
    parser.add_argument("--split", default="test", help="Dataset split")
    parser.add_argument("--output", default="benchmarks/swe-bench/predictions.jsonl", help="Output JSONL path")
    parser.add_argument("--max-instances", type=int, default=None, help="Limit instances for a dry run")
    parser.add_argument("--timeout", type=int, default=600, help="Per-instance timeout in seconds")
    parser.add_argument("--keep-workspaces", action="store_true", help="Do not delete temporary workspaces")
    args = parser.parse_args()

    if "OPENAI_API_KEY" not in os.environ:
        creds = Path.home() / ".meldra" / "credentials.env"
        if creds.exists():
            for line in creds.read_text().splitlines():
                line = line.strip()
                if line.startswith("OPENAI_API_KEY="):
                    os.environ["OPENAI_API_KEY"] = line.split("=", 1)[1].strip().strip('"')
                    break
    if "OPENAI_API_KEY" not in os.environ:
        print("error: OPENAI_API_KEY is not set", file=sys.stderr)
        return 1

    try:
        from datasets import load_dataset
    except ImportError:
        print("error: 'datasets' package is required; install with: pip install datasets", file=sys.stderr)
        return 1

    dataset = load_dataset(args.dataset, split=args.split)
    output_path = Path(args.output)
    output_path.parent.mkdir(parents=True, exist_ok=True)

    work_root = Path(tempfile.mkdtemp(prefix="meldra-swe-bench-"))
    print(f"workspaces: {work_root}")

    try:
        with open(output_path, "w") as out:
            for i, instance in enumerate(dataset):
                if args.max_instances is not None and i >= args.max_instances:
                    print("reached --max-instances limit")
                    break

                print(f"[{i + 1}/{len(dataset)}] {instance['instance_id']}")
                try:
                    pred = process_instance(instance, work_root, args.timeout)
                except Exception as exc:
                    print(f"  failed: {exc}")
                    pred = {
                        "instance_id": instance["instance_id"],
                        "model_name_or_path": "meldra",
                        "model_patch": "",
                    }
                out.write(json.dumps(pred) + "\n")
                out.flush()
    finally:
        if not args.keep_workspaces:
            run_command(["rm", "-rf", str(work_root)], cwd=Path.cwd(), check=False)

    print(f"predictions written to {output_path}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
