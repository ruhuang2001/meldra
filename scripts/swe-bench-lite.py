#!/usr/bin/env python3
"""
SWE-bench adapter for Meldra (lite / verified subsets, dev / test splits).

Meldra only generates patches. Verify either locally with the official
SWE-bench harness (Docker) or in the cloud via sb-cli submit.

Usage:
    # generate only, first 5 lite/test tasks
    python3 scripts/swe-bench-lite.py --binary dist/meldra --max-tasks 5

    # lite/dev split (cheap iteration; SWE-bench Verified ships only a test split)
    python3 scripts/swe-bench-lite.py --binary dist/meldra --subset lite --split dev

    # local Docker verification
    python3 scripts/swe-bench-lite.py --binary dist/meldra --max-tasks 5 --pull --evaluate

    # hosted cloud evaluation (needs SWEBENCH_API_KEY)
    python3 scripts/swe-bench-lite.py --binary dist/meldra --max-tasks 300 --submit

Requirements:
    pip install swebench sb-cli typing-extensions   (sb-cli omits a runtime dependency)

Meldra runs with --auto-approve in a temporary checkout with host permissions
and stdin closed; without it, command approvals read EOF and are
declined, so the agent could edit files but never run tests or search.
"""

import argparse
import json
import math
import os
import signal
import subprocess
import sys
import tempfile
import time
from concurrent.futures import ThreadPoolExecutor, as_completed
from pathlib import Path

try:
    from datasets import load_dataset
except ImportError:
    sys.exit("Error: install dependencies with: pip install swebench")

# These datasets carry the prebuilt per-instance image names the local harness needs.
# Maps --subset to (HF dataset, sb-cli subset name).
SUBSETS = {
    "lite": ("SWE-bench/SWE-bench_Lite", "swe-bench_lite"),
    "verified": ("SWE-bench/SWE-bench_Verified", "swe-bench_verified"),
}
OUTPUT_DIR = Path("benchmarks/swe-bench")
MODEL_NAME = "meldra"
AGENT_TIMEOUT_SECONDS = 600
PULL_TIMEOUT_SECONDS = 1800
TERMINATION_GRACE_SECONDS = 15


def pull_images(tasks: list, mirror: str, workers: int):
    """The harness never pulls; fetch each instance's prebuilt image in parallel."""
    targets = [t for t in tasks if t.get("image")]
    if len(targets) < len(tasks):
        print(f"{len(tasks) - len(targets)} tasks have no image; skipped", flush=True)
    if not targets:
        return []

    def pull(task):
        image = task["image"]
        source = f"{mirror}/{image}" if mirror else image
        try:
            proc = subprocess.run(
                ["docker", "pull", "--platform", "linux/amd64", source],
                capture_output=True,
                text=True,
                timeout=PULL_TIMEOUT_SECONDS,
            )
        except FileNotFoundError:
            return image, "docker is not installed or not on PATH"
        except subprocess.TimeoutExpired:
            return image, f"pull timed out after {PULL_TIMEOUT_SECONDS}s"
        if proc.returncode != 0:
            return image, (proc.stderr or proc.stdout)[-300:]
        tag = subprocess.run(["docker", "tag", source, image], capture_output=True, text=True)
        if tag.returncode != 0:
            return image, f"tag failed: {(tag.stderr or tag.stdout)[-300:]}"
        return image, None

    failed = []
    with ThreadPoolExecutor(max_workers=workers) as ex:
        futures = [ex.submit(pull, t) for t in targets]
        for i, future in enumerate(as_completed(futures), 1):
            image, err = future.result()
            if err:
                failed.append(image)
                print(f"[{i}/{len(targets)}] pull failed {image}: {err}", flush=True)
            else:
                print(f"[{i}/{len(targets)}] pulled {image}", flush=True)
    if failed:
        print(f"{len(failed)} images failed to pull; their instances will error at eval time")
    return failed


def save_predictions(path: Path, records: dict):
    """Replace complete JSONL atomically; never append after a partial tail."""
    temporary = None
    try:
        with tempfile.NamedTemporaryFile(mode="w", encoding="utf-8", dir=path.parent,
                                         prefix=".predictions-", delete=False) as out:
            temporary = Path(out.name)
            for record in records.values():
                out.write(json.dumps(record) + "\n")
            out.flush()
            os.fsync(out.fileno())
        os.replace(temporary, path)
        temporary = None
        descriptor = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(descriptor)
        finally:
            os.close(descriptor)
    finally:
        if temporary is not None:
            temporary.unlink(missing_ok=True)


def record_attempt(path: Path, record: dict):
    records = []
    original = path.read_bytes() if path.exists() else b""
    damaged = False
    for line in original.splitlines():
        if not line.strip():
            continue
        try:
            previous = json.loads(line)
            if not isinstance(previous, dict):
                raise ValueError("attempt must be an object")
            records.append(previous)
        except (ValueError, UnicodeDecodeError):
            damaged = True
    if damaged:
        with tempfile.NamedTemporaryFile(dir=path.parent, prefix="attempts-recovery-",
                                         suffix=".jsonl", delete=False) as backup:
            backup.write(original)
            backup.flush()
            os.fsync(backup.fileno())
    records.append(record)
    save_predictions(path, dict(enumerate(records)))


def load_predictions(path: Path) -> dict:
    if not path.exists():
        return {}
    original = path.read_bytes()
    records = {}
    rejected = []
    for number, line in enumerate(original.splitlines(), 1):
        if not line.strip():
            continue
        try:
            record = json.loads(line)
            if (not isinstance(record, dict)
                    or not isinstance(record.get("instance_id"), str)
                    or not record["instance_id"]
                    or not isinstance(record.get("model_patch"), str)
                    or not isinstance(record.get("model_name_or_path"), str)):
                raise ValueError("invalid prediction fields")
            if record.get("agent_error"):
                rejected.append(record)
                continue
            records[record["instance_id"]] = record
        except (ValueError, UnicodeDecodeError):
            print(f"Recovering unreadable predictions.jsonl line {number}", flush=True)
    normalized = "".join(json.dumps(record) + "\n" for record in records.values()).encode()
    if normalized != original:
        # Preserve original evidence before repairing damaged or legacy failed rows.
        with tempfile.NamedTemporaryFile(dir=path.parent, prefix="predictions-recovery-",
                                         suffix=".jsonl", delete=False) as backup:
            backup.write(original)
            backup.flush()
            os.fsync(backup.fileno())
        for record in rejected:
            record_attempt(path.with_name("attempts.jsonl"), record)
        save_predictions(path, records)
    return records


def generate_patches(meldra: Path, tasks: list, records: dict, log_dir: Path,
                     predictions_path: Path, workers: int, timeout: float, grace: float):
    """Persist outcomes before publishing only completed predictions."""
    pending = [t for t in tasks if t["instance_id"] not in records]
    failures = 0
    with ThreadPoolExecutor(max_workers=workers) as ex:
        futures = {ex.submit(generate_patch, meldra, t, log_dir, timeout, grace): t for t in pending}
        for i, future in enumerate(as_completed(futures), 1):
            task = futures[future]
            try:
                prediction = future.result()
            except Exception as exc:
                prediction = {"instance_id": task["instance_id"],
                              "model_name_or_path": MODEL_NAME, "model_patch": "",
                              "agent_error": f"generation failed: {exc}"}
            record_attempt(predictions_path.with_name("attempts.jsonl"), prediction)
            if prediction.get("agent_error"):
                failures += 1
            else:
                records[task["instance_id"]] = prediction
                save_predictions(predictions_path, records)
            changed = "empty patch" if not prediction["model_patch"] else f"{len(prediction['model_patch'].encode())} byte patch"
            print(f"[{i}/{len(pending)}] {task['instance_id']}: {changed}, "
                  f"{prediction.get('agent_seconds', 0)}s {prediction['agent_error']}", flush=True)
    return failures


def descendant_groups(pid: int) -> dict:
    """Snapshot child identities before graceful shutdown can reparent them."""
    snapshot = subprocess.run(["ps", "-axo", "pid=,ppid=,pgid="],
                              capture_output=True, text=True, check=True, timeout=5)
    rows = [tuple(map(int, line.split())) for line in snapshot.stdout.splitlines() if line.strip()]
    descendants = {pid}
    while True:
        expanded = descendants | {child for child, parent, _ in rows if parent in descendants}
        if expanded == descendants:
            break
        descendants = expanded
    return {child: group for child, _, group in rows if child in descendants and child != pid}


def stop_agent(proc, grace: float):
    # Meldra handles SIGTERM by cancelling its turn and reaping owned process groups.
    children = {}
    try:
        children = descendant_groups(proc.pid)
    except (OSError, ValueError, subprocess.SubprocessError) as exc:
        print(f"Could not inspect agent descendants: {exc}", file=sys.stderr)
    try:
        proc.terminate()
        proc.wait(timeout=grace)
    except subprocess.TimeoutExpired:
        pass
    finally:
        # A stalled parent cannot perform its normal cleanup. Only signal captured
        # child groups whose member identity is still present, never our own group.
        for child, group in children.items():
            try:
                if os.getpgid(child) == group and group != os.getpgrp():
                    os.killpg(group, signal.SIGKILL)
            except ProcessLookupError:
                pass
        if proc.poll() is None:
            try:
                os.killpg(proc.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
        proc.wait(timeout=5)


def build_prompt(task: dict) -> str:
    # Fence the issue text so a problem statement that contains headings or
    # unclosed code fences cannot absorb the instructions that follow.
    statement = task["problem_statement"]
    fence = "`" * max(3, max((len(run) for run in statement.split() if set(run) == {"`"}), default=0) + 1)
    return f"""Fix the following issue in the {task['repo']} repository.

## Problem Statement

{fence}
{statement}
{fence}

## Instructions

1. Read relevant files to understand the codebase
2. Identify the root cause of the issue
3. Make minimal, targeted code changes to fix it
4. Do NOT modify test files

Please fix the issue now.
"""


def git(workspace: Path, *args: str) -> str:
    return subprocess.run(
        ["git", *args], cwd=workspace, check=True, capture_output=True, text=True
    ).stdout


def generate_patch(meldra: Path, task: dict, log_dir: Path,
                   timeout: float = AGENT_TIMEOUT_SECONDS,
                   grace: float = TERMINATION_GRACE_SECONDS) -> dict:
    """Run Meldra on a fresh checkout and return a SWE-bench prediction."""
    with tempfile.TemporaryDirectory() as tmpdir:
        workspace = Path(tmpdir)
        start = time.time()
        error = ""
        try:
            subprocess.run(
                ["git", "clone", "--quiet", f"https://github.com/{task['repo']}.git", str(workspace)],
                check=True,
                capture_output=True,
            )
            git(workspace, "checkout", "--quiet", task["base_commit"])
        except subprocess.CalledProcessError as exc:
            stderr = (exc.stderr or b"").decode(errors="replace")[-300:] if isinstance(exc.stderr, bytes) else str(exc.stderr)[-300:]
            return {
                "instance_id": task["instance_id"],
                "model_name_or_path": MODEL_NAME,
                "model_patch": "",
                "agent_seconds": round(time.time() - start, 1),
                "agent_error": f"checkout failed: {stderr}".strip(),
            }

        # File-backed output cannot hang on a pipe inherited by a surviving child.
        with (log_dir / f"{task['instance_id']}.log").open("w", encoding="utf-8") as log:
            proc = subprocess.Popen(
                [str(meldra), "--mode", "build", "--permissions", "workspace-edit",
                 "--auto-approve", "--workspace", str(workspace), "--prompt", build_prompt(task)],
                stdin=subprocess.DEVNULL, stdout=log, stderr=subprocess.STDOUT,
                start_new_session=True,
            )
            try:
                proc.wait(timeout=timeout)
            except subprocess.TimeoutExpired:
                error = f"meldra timeout (>{timeout}s)"
                stop_agent(proc, grace)
            except BaseException:
                stop_agent(proc, grace)
                raise
            if proc.returncode != 0 and not error:
                error = f"meldra exit {proc.returncode}"

        # Stage everything so files created by the agent are part of the patch.
        # Failed attempts retain their partial diff as evidence, but generate_patches
        # excludes them from predictions. Successful empty patches are valid attempts.
        try:
            git(workspace, "add", "-A")
            patch = git(workspace, "diff", "--cached")
        except subprocess.CalledProcessError as exc:
            patch = ""
            error = f"{error} (diff failed: {exc})".strip()

        # Mark failed attempts with no changes explicitly for troubleshooting.
        if error and not patch.strip():
            patch = ""
            error = f"{error} (no changes)"

    return {
        "instance_id": task["instance_id"],
        "model_name_or_path": MODEL_NAME,
        "model_patch": patch,
        "agent_seconds": round(time.time() - start, 1),
        "agent_error": error,
    }


def main():
    parser = argparse.ArgumentParser(description="Run SWE-bench subsets on Meldra")
    parser.add_argument("--binary", required=True, help="Path to Meldra binary (e.g. dist/meldra)")
    parser.add_argument("--subset", choices=list(SUBSETS), default="lite", help="Dataset subset (default: lite)")
    parser.add_argument("--split", choices=["dev", "test"], default="test", help="Dataset split (default: test)")
    parser.add_argument("--max-tasks", type=int, default=5, help="Run the first N tasks (default: 5)")
    parser.add_argument("--instance-ids", nargs="+", help="Run specific instances instead of the first N")
    parser.add_argument("--run-id", default=time.strftime("meldra-%Y%m%d-%H%M%S"))
    evaluation = parser.add_mutually_exclusive_group()
    evaluation.add_argument("--evaluate", action="store_true", help="Verify patches locally with the official SWE-bench harness (Docker)")
    evaluation.add_argument("--submit", action="store_true", help="Submit predictions to hosted SWE-bench eval via sb-cli (needs SWEBENCH_API_KEY)")
    parser.add_argument("--max-workers", type=int, default=1, help="Harness Docker workers (default: 1)")
    parser.add_argument("--pull", action="store_true", help="Pre-pull each instance's image before evaluating")
    parser.add_argument("--mirror", default="docker.1ms.run", help="Docker Hub mirror; empty pulls directly")
    parser.add_argument("--agent-workers", type=int, default=4, help="Parallel meldra processes (default: 4)")
    parser.add_argument("--pull-workers", type=int, default=8, help="Parallel docker pulls (default: 8)")
    parser.add_argument("--agent-timeout", type=float, default=AGENT_TIMEOUT_SECONDS,
                        help="Seconds allowed per agent (default: 600)")
    parser.add_argument("--termination-grace", type=float, default=TERMINATION_GRACE_SECONDS,
                        help="Seconds for graceful agent shutdown (default: 15)")
    args = parser.parse_args()
    for name in ("max_tasks", "max_workers", "agent_workers", "pull_workers",
                 "agent_timeout", "termination_grace"):
        value = getattr(args, name)
        if not math.isfinite(value) or value <= 0:
            parser.error(f"--{name.replace('_', '-')} must be positive and finite")

    meldra = Path(args.binary).resolve()
    if not meldra.exists():
        sys.exit(f"Meldra binary not found: {meldra}")

    dataset_name, sb_subset = SUBSETS[args.subset]
    if args.subset == "verified" and args.split == "dev":
        sys.exit("SWE-bench Verified ships only a test split; use --subset lite --split dev for cheap iteration")
    dataset = load_dataset(dataset_name, split=args.split)
    if args.instance_ids:
        wanted = set(args.instance_ids)
        tasks = [t for t in dataset if t["instance_id"] in wanted]
        missing = wanted - {t["instance_id"] for t in tasks}
        if missing:
            sys.exit(f"Unknown instance ids: {', '.join(sorted(missing))}")
    else:
        tasks = list(dataset.select(range(min(args.max_tasks, len(dataset)))))

    run_dir = (OUTPUT_DIR / args.run_id).resolve()
    log_dir = run_dir / "agent-logs"
    log_dir.mkdir(parents=True, exist_ok=True)
    predictions_path = run_dir / "predictions.jsonl"

    records = load_predictions(predictions_path)
    if not predictions_path.exists():
        save_predictions(predictions_path, records)

    if args.pull:
        pull_images(tasks, args.mirror, args.pull_workers)

    pending = [t for t in tasks if t["instance_id"] not in records]
    if pending:
        failures = generate_patches(meldra, tasks, records, log_dir, predictions_path,
                                    args.agent_workers, args.agent_timeout, args.termination_grace)
        if failures:
            sys.exit(f"{failures} instance(s) failed; see {run_dir / 'attempts.jsonl'}. "
                     "Rerun with the same --run-id to retry. Evaluation was not started.")
    else:
        print("All requested instances already in predictions.jsonl")

    print(f"\nPredictions: {predictions_path}")

    if args.submit:
        subprocess.run(
            [
                "sb-cli", "submit", sb_subset, args.split,
                "--predictions_path", str(predictions_path),
                "--run_id", args.run_id,
            ],
            check=True,
        )
        return

    if not args.evaluate:
        print(f"Evaluate locally with:\n  cd {run_dir} && python -m swebench.harness.run_evaluation "
              f"-d {dataset_name} -s {args.split} -p predictions.jsonl -id {args.run_id} --max_workers {args.max_workers}")
        print(f"Or submit to hosted eval:\n  sb-cli submit {sb_subset} {args.split} "
              f"--predictions_path {predictions_path} --run_id {args.run_id}")
        return

    # The harness writes logs/ and its report into the current directory.
    subprocess.run(
        [
            sys.executable, "-m", "swebench.harness.run_evaluation",
            "-d", dataset_name,
            "-s", args.split,
            "-p", str(predictions_path),
            "-id", args.run_id,
            "-i", *[t["instance_id"] for t in tasks],
            "--max_workers", str(args.max_workers),
        ],
        cwd=run_dir,
        check=True,
    )
    for report in run_dir.glob(f"*.{args.run_id}.json"):
        summary = json.loads(report.read_text())
        total = len(tasks)
        resolved = summary.get("resolved_instances", 0)
        print(f"\nResolved {resolved}/{total} ({resolved / total * 100:.1f}%)  report: {report}")


if __name__ == "__main__":
    main()
