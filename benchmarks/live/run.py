#!/usr/bin/env python3
"""Prepare and independently grade the fixed release cohort without model calls.

Live execution is deliberately absent until the binary enforces token admission
limits. A timeout or a post-response usage check cannot enforce a dollar budget.
"""
from __future__ import annotations

import argparse
from datetime import datetime, timezone
from decimal import Decimal, InvalidOperation
import hashlib
import json
import os
from pathlib import Path
import subprocess
import threading
import time
import uuid

ROOT = Path(__file__).resolve().parent
MAX_LOG = 64 * 1024
MAX_FILE = 2 * 1024 * 1024


def sha(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def cohort() -> dict:
    return json.loads((ROOT / "cohort.json").read_text())


def cohort_hash() -> str:
    digest = hashlib.sha256((ROOT / "cohort.json").read_bytes())
    for directory in ("fixtures", "graders"):
        for path in sorted((ROOT / directory).rglob("*.txt")):
            digest.update(str(path.relative_to(ROOT)).encode() + b"\0" + path.read_bytes())
    return digest.hexdigest()


def templates(task: dict, kind: str) -> dict[str, bytes]:
    return {p.name.removesuffix(".txt"): p.read_bytes()
            for p in sorted((ROOT / kind / task["id"]).glob("*.txt"))}


def prepare(output: Path) -> dict:
    # Generated/model-written Go sources must never enter this repository's
    # recursive formatter or tests, even when an ignored results directory is
    # used. Keep submitted workspaces external; JSON reports may stay local.
    if output.resolve().is_relative_to(ROOT.parents[1]):
        raise ValueError("prepare workspaces outside the repository; only reports belong in benchmarks/results")
    output.mkdir(mode=0o700, parents=True, exist_ok=False)
    manifest = {"schema_version": 1, "suite": cohort()["suite"],
                "cohort_sha256": cohort_hash(), "created_at": datetime.now(timezone.utc).isoformat(),
                "live_model": False, "attempts_per_task": 1, "tasks": []}
    for task in cohort()["tasks"]:
        directory = output / task["id"]
        workspace = directory / "workspace"
        workspace.mkdir(parents=True)
        files = templates(task, "fixtures")
        for name, contents in files.items():
            (workspace / name).write_bytes(contents)
        prompt = task["prompt"] + "\nOnly modify these files: " + ", ".join(task["editable"]) + ".\n"
        (directory / "prompt.txt").write_text(prompt)
        manifest["tasks"].append({"id": task["id"], "initial_sha256": {name: sha(data) for name, data in files.items()}})
    (output / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
    return manifest


def budget_plan(model: str, provider: str, input_price: str, output_price: str, token_budget: int,
                max_output_tokens: int, max_cost: str) -> dict:
    try:
        prices = [Decimal(input_price), Decimal(output_price)]
        cost = Decimal(max_cost)
    except InvalidOperation as exc:
        raise ValueError("provide known numeric prices and cost limit") from exc
    if any(not value.is_finite() or value <= 0 for value in [*prices, cost]):
        raise ValueError("prices and cost limit must be positive, finite, and known")
    if not model.strip() or not provider.strip() or token_budget < 1 or max_output_tokens < 1 or max_output_tokens > token_budget:
        raise ValueError("provide a model/provider and positive per-task/per-response token caps")
    reserved = Decimal(len(cohort()["tasks"]) * token_budget) * max(prices) / 1_000_000
    if reserved > cost:
        raise ValueError(f"worst-case reservation ${reserved} exceeds ${cost}; lower token caps")
    return {"schema_version": 1, "suite": cohort()["suite"], "cohort_sha256": cohort_hash(),
            "model": model, "provider": provider, "tasks": len(cohort()["tasks"]), "attempts_per_task": 1,
            "input_price_per_million": str(prices[0]), "output_price_per_million": str(prices[1]),
            "total_tokens_per_task": token_budget, "max_output_tokens_per_response": max_output_tokens,
            "max_cost_usd": str(cost), "conditional_cost_upper_bound_usd": str(reserved),
            "budget_enforced": False, "live_execution_enabled": False,
            "required_before_live": ["explicit approval of model, provider, cohort and prices",
                                     "binary admission control for cumulative input and output token cap",
                                     "max_output_tokens on every model request, including reasoning",
                                     "Docker-only agent execution; no host --yes"]}


def validate_submission(workspace: Path, task: dict) -> list[str]:
    errors = []
    expected = templates(task, "fixtures")
    for path in workspace.rglob("*"):
        if path.is_symlink():
            errors.append(f"symlink forbidden: {path.relative_to(workspace)}")
        elif path.is_file() and str(path.relative_to(workspace)) not in expected:
            errors.append(f"unexpected file: {path.relative_to(workspace)}")
    for name, original in expected.items():
        path = workspace / name
        if path.is_symlink() or not path.is_file() or path.stat().st_size > MAX_FILE:
            errors.append(f"missing, unsafe or oversized file: {name}")
        elif name not in task["editable"] and path.read_bytes() != original:
            errors.append(f"protected support file changed: {name}")
    return errors


def grader_argv(image: str, workspace: Path, grader: Path, language: str, name: str) -> list[str]:
    validation = "go test -count=1 -timeout=15s ./..." if language == "go" else "python3 -m unittest discover -v -p test_hidden.py"
    command = ('mkdir -p /tmp/work && cp -R /submission/. /tmp/work/ && '
               'for f in /grader/*.txt; do base=${f##*/}; cp "$f" "/tmp/work/${base%.txt}"; done && '
               'cd /tmp/work && exec ' + validation)
    return ["docker", "run", "--rm", "--name", name, "--network", "none", "--read-only",
            "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--pids-limit", "128",
            "--memory", "1g", "--cpus", "1", "--user", f"{os.getuid()}:{os.getgid()}",
            "--tmpfs", "/tmp:rw,exec,nosuid,size=256m,mode=1777", "--workdir", "/tmp",
            "--mount", f"type=bind,source={workspace.resolve()},target=/submission,readonly",
            "--mount", f"type=bind,source={grader.resolve()},target=/grader,readonly",
            image, "sh", "-c", command]


def bounded_run(argv: list[str], container_name: str, timeout: int) -> tuple[int, str, bool]:
    process = subprocess.Popen(argv, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    output = bytearray()
    truncated = False

    def consume():
        nonlocal truncated
        while chunk := process.stdout.read(8192):
            available = max(0, MAX_LOG - len(output))
            output.extend(chunk[:available])
            truncated |= len(chunk) > available

    reader = threading.Thread(target=consume, daemon=True)
    reader.start()
    try:
        status = process.wait(timeout=timeout)
    except subprocess.TimeoutExpired:
        status = 124
    finally:
        # Killing the Docker CLI alone does not stop its container.
        try:
            subprocess.run(["docker", "rm", "-f", container_name], capture_output=True, timeout=15)
        finally:
            if process.poll() is None:
                process.kill()
                process.wait(timeout=5)
            reader.join(timeout=5)
            process.stdout.close()
    return status, output.decode("utf-8", errors="replace"), truncated


def grade(run_dir: Path, image: str, report_path: Path) -> dict:
    if report_path.exists():
        raise ValueError("report exists; use a fresh output path")
    manifest = json.loads((run_dir / "manifest.json").read_text())
    if manifest.get("cohort_sha256") != cohort_hash():
        raise ValueError("cohort changed since preparation; cannot compare this run")
    if [task["id"] for task in manifest["tasks"]] != [task["id"] for task in cohort()["tasks"]]:
        raise ValueError("task cohort differs or is incomplete")
    image_id = subprocess.check_output(["docker", "image", "inspect", "--format", "{{.Id}}", image], text=True).strip()
    results = []
    for task in cohort()["tasks"]:
        workspace = run_dir / task["id"] / "workspace"
        started = time.monotonic()
        errors = validate_submission(workspace, task)
        status, output, truncated = 1, "\n".join(errors), False
        if not errors:
            name = "meldra-grade-" + uuid.uuid4().hex
            status, output, truncated = bounded_run(grader_argv(image_id, workspace, ROOT / "graders" / task["id"], task["language"], name), name, 120)
        results.append({"id": task["id"], "passed": status == 0, "exit_code": status,
                        "duration_seconds": time.monotonic() - started, "grader_output": output,
                        "output_truncated": truncated, "input_tokens": None, "output_tokens": None,
                        "cost_usd": None})
    report = {"schema_version": 1, "suite": cohort()["suite"], "cohort_sha256": cohort_hash(),
              "image_id": image_id, "kind": "external_grading", "model_quality_evidence": False,
              "note": "Attach independently recorded agent/model/budget usage before claiming live quality.",
              "tasks": len(results), "passed": sum(result["passed"] for result in results), "results": results}
    report_path.parent.mkdir(parents=True, exist_ok=True)
    with report_path.open("x") as stream:
        json.dump(report, stream, indent=2)
        stream.write("\n")
    return report


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    prepare_parser = commands.add_parser("prepare")
    prepare_parser.add_argument("--output", type=Path, required=True)
    plan = commands.add_parser("plan")
    for flag in ("model", "provider", "input-price", "output-price", "max-cost"):
        plan.add_argument("--" + flag, required=True)
    plan.add_argument("--token-budget", type=int, required=True)
    plan.add_argument("--max-output-tokens", type=int, required=True)
    grader = commands.add_parser("grade")
    grader.add_argument("--run-dir", type=Path, required=True)
    grader.add_argument("--image", required=True)
    grader.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    try:
        if args.command == "prepare":
            result = prepare(args.output)
        elif args.command == "plan":
            result = budget_plan(args.model, args.provider, args.input_price, args.output_price,
                                 args.token_budget, args.max_output_tokens, args.max_cost)
        else:
            result = grade(args.run_dir, args.image, args.output)
        print(json.dumps(result, indent=2))
        return int(args.command == "grade" and result["passed"] != result["tasks"])
    except (ValueError, OSError, subprocess.SubprocessError) as exc:
        parser.exit(2, f"error: {exc}\n")


if __name__ == "__main__":
    raise SystemExit(main())
