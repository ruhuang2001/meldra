#!/usr/bin/env python3
"""Reproducible, credential-free runtime measurements and matched comparisons.

Uses only the Python standard library. Model quality is reported separately from
scripted runtime correctness; this command never starts a live model evaluation.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import math
import os
import platform
import re
import signal
import statistics
import subprocess
import sys
import tempfile
from contextlib import suppress
from datetime import datetime, timezone
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
SCHEMA = 1


def command_output(*args: str) -> str:
    result = subprocess.run(args, cwd=ROOT, capture_output=True, text=True, check=True, timeout=30)
    return result.stdout.strip()


def metadata() -> dict:
    tracked = command_output("git", "ls-files", "-co", "--exclude-standard").splitlines()
    source = hashlib.sha256()
    for name in sorted(set(tracked)):
        path = ROOT / name
        if path.is_file() and (path.suffix in {".go", ".py"} or name in {"go.mod", "go.sum", "Makefile"}):
            source.update(name.encode() + b"\0" + path.read_bytes() + b"\0")
    cpu = platform.processor()
    if platform.system() == "Darwin":
        cpu = command_output("sysctl", "-n", "machdep.cpu.brand_string")
    elif Path("/proc/cpuinfo").is_file():
        for line in Path("/proc/cpuinfo").read_text().splitlines():
            if line.startswith("model name"):
                cpu = line.split(":", 1)[1].strip()
                break
    return {
        "revision": command_output("git", "rev-parse", "HEAD"),
        "dirty": bool(command_output("git", "status", "--porcelain")),
        "source_sha256": source.hexdigest(),
        "go_version": command_output("go", "version"),
        "os": platform.system(), "os_release": platform.release(), "arch": platform.machine(), "cpu": cpu,
        "logical_cpus": os.cpu_count(),
        "created_at": datetime.now(timezone.utc).isoformat(),
    }


def summarize(values: list[float]) -> dict:
    if not values or any(not math.isfinite(v) or v < 0 for v in values):
        raise ValueError("metric samples must be nonempty, finite and nonnegative")
    ordered = sorted(values)
    return {"samples": values, "count": len(values), "median": statistics.median(values),
            "min": ordered[0], "max": ordered[-1], "p95": ordered[math.ceil(.95 * len(values)) - 1]}


def parse_micro(events: list[dict], repeats: int) -> dict:
    samples: dict[str, dict[str, list[float]]] = {}
    # test2json may flush a benchmark name before the timing columns arrive.
    # Reassemble the output stream for each package before splitting lines.
    streams: dict[str, list[str]] = {}
    for event in events:
        streams.setdefault(event.get("Package", ""), []).append(event.get("Output", ""))
    for package, fragments in streams.items():
        for line in "".join(fragments).splitlines():
            fields = line.split()
            if len(fields) < 4 or not fields[0].startswith("Benchmark") or not fields[1].isdigit():
                continue
            if int(fields[1]) <= 0 or (len(fields) - 2) % 2:
                raise ValueError("invalid benchmark measurement")
            name = re.sub(r"-\d+$", "", fields[0])
            units = samples.setdefault(f"{package}/{name}", {})
            for i in range(2, len(fields), 2):
                units.setdefault(fields[i + 1], []).append(float(fields[i]))
    if not samples:
        raise ValueError("no benchmark measurements; check the benchmark filter and log")
    results = {}
    for name, metrics in sorted(samples.items()):
        if "ns/op" not in metrics or any(len(values) != repeats for values in metrics.values()):
            raise ValueError(f"{name}: incomplete or duplicate repetitions")
        results[name] = {unit: summarize(values) for unit, values in sorted(metrics.items())}
    return results


def parse_scenarios(events: list[dict], cases: list[str], repeats: int) -> dict:
    expected = set(cases)
    if not expected or len(expected) != len(cases):
        raise ValueError("scenario manifest must contain unique cases")
    runs: dict[str, list[dict]] = {name: [] for name in cases}
    active: dict[str, dict] = {}
    for event in events:
        name = event.get("Test")
        if name not in expected:
            continue
        action = event.get("Action")
        if action == "run":
            if name in active:
                raise ValueError(f"{name}: previous attempt did not finish")
            active[name] = {}
        elif action == "output" and "EVAL_METRICS " in event.get("Output", ""):
            if name not in active:
                raise ValueError(f"{name}: metrics outside an active attempt")
            metrics = json.loads(event["Output"].split("EVAL_METRICS ", 1)[1])
            for value in metrics.values():
                if not isinstance(value, (int, float)) or not math.isfinite(value) or value < 0:
                    raise ValueError("invalid scenario metric")
            active[name].update(metrics)
        elif action in {"pass", "fail", "skip"}:
            if name not in active:
                raise ValueError(f"{name}: outcome without start")
            metrics = active.pop(name)
            measured_turn = "duration_ms" in metrics
            metrics.setdefault("duration_ms", event.get("Elapsed", 0) * 1000)
            runs[name].append({"status": action, "timing_scope": "turn" if measured_turn else "test", **metrics})
    if active or any(len(attempts) != repeats for attempts in runs.values()):
        raise ValueError("missing/unfinished scenario or incorrect attempt count; no score generated")
    return {name: {"attempts": attempts, "passed": sum(a["status"] == "pass" for a in attempts),
                   "duration_ms": summarize([a["duration_ms"] for a in attempts])}
            for name, attempts in sorted(runs.items())}


def read_events(path: Path) -> list[dict]:
    events = []
    for line in path.read_text().splitlines():
        try:
            event = json.loads(line)
        except json.JSONDecodeError:
            continue  # Go toolchain diagnostics remain available in the raw log.
        if isinstance(event, dict):
            events.append(event)
    return events


def write_report(path: Path, report: dict) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.NamedTemporaryFile(mode="w", dir=path.parent, delete=False) as output:
        temporary = Path(output.name)
        try:
            json.dump(report, output, indent=2, allow_nan=False)
            output.write("\n")
            output.flush()
            os.fsync(output.fileno())
        except BaseException:
            temporary.unlink(missing_ok=True)
            raise
    try:
        os.replace(temporary, path)
    finally:
        temporary.unlink(missing_ok=True)


def execute_go(command: list[str], stream, timeout: int) -> int:
    # A timeout must terminate test binaries too, not just their go parent.
    process = subprocess.Popen(command, cwd=ROOT, stdout=stream, stderr=subprocess.STDOUT,
                               start_new_session=os.name == "posix")
    try:
        return process.wait(timeout=timeout)
    except (subprocess.TimeoutExpired, KeyboardInterrupt):
        if os.name == "posix":
            with suppress(ProcessLookupError):
                os.killpg(process.pid, signal.SIGKILL)
        else:
            process.kill()
        process.wait()
        raise


def run_measurements(args: argparse.Namespace) -> int:
    output = args.output.resolve()
    if output.exists():
        raise ValueError(f"report already exists: {output}; choose a new output path")
    meta = metadata()
    meta.update({"repeats": args.count, "cpu_limit": args.cpu})
    command = ["go", "test", "-json", f"-count={args.count}", f"-cpu={args.cpu}", "-timeout=10m"]
    manifest = None
    if args.command == "micro":
        command += ["-run=^$", f"-bench={args.bench}", "-benchmem", f"-benchtime={args.benchtime}", "./..."]
        meta.update({"benchtime": args.benchtime, "benchmark_filter": args.bench})
        suite = "runtime-micro-v1"
    else:
        manifest = json.loads((ROOT / "benchmarks/suites/offline.json").read_text())
        parents = sorted({name.split("/", 1)[0] for name in manifest["cases"]})
        command += ["-run=^(" + "|".join(re.escape(name) for name in parents) + ")$", manifest["package"]]
        suite = manifest["name"]
        meta["manifest_sha256"] = hashlib.sha256(json.dumps(manifest, sort_keys=True).encode()).hexdigest()
    output.parent.mkdir(parents=True, exist_ok=True)
    log = output.with_suffix(".log")
    with log.open("x") as stream:
        returncode = execute_go(command, stream, args.timeout)
    events = read_events(log)
    report = {"schema_version": SCHEMA, "kind": args.command, "suite": suite, "metadata": meta,
              "completed": returncode == 0, "execution_exit_code": returncode, "command": command, "raw_log": log.name, "raw_log_sha256": hashlib.sha256(log.read_bytes()).hexdigest(),
              "live_model": False}
    if args.command == "micro":
        if returncode:
            raise ValueError(f"benchmark execution failed; see {log}")
        report["benchmarks"] = parse_micro(events, args.count)
        report["summary"] = {"benchmarks": len(report["benchmarks"]), "samples_per_benchmark": args.count}
    else:
        report["cases"] = parse_scenarios(events, manifest["cases"], args.count)
        passed = sum(case["passed"] for case in report["cases"].values())
        has_failure = any(a["status"] == "fail" for case in report["cases"].values() for a in case["attempts"])
        if returncode not in (0, 1) or (returncode and not has_failure):
            raise ValueError(f"evaluation infrastructure failed; see {log}")
        report["completed"] = True
        total = len(report["cases"]) * args.count
        report["summary"] = {"cases": len(report["cases"]), "attempts": total, "passed": passed,
                             "pass_rate": passed / total, "model_quality_score": None}
    write_report(output, report)
    print(json.dumps(report["summary"], indent=2))
    print(f"Report: {output}\nRaw log: {log}")
    return 0 if returncode == 0 and (args.command == "micro" or report["summary"]["pass_rate"] == 1) else 1


def comparison(before: dict, after: dict, allow_environment_change: bool = False) -> dict:
    if before.get("schema_version") != SCHEMA or after.get("schema_version") != SCHEMA:
        raise ValueError("unsupported report schema")
    if before.get("kind") != after.get("kind") or before.get("suite") != after.get("suite"):
        raise ValueError("cannot compare different suites or report kinds")
    if not before.get("completed") or not after.get("completed"):
        raise ValueError("cannot compare incomplete runs")
    warnings = []
    for field in ("os", "os_release", "arch", "cpu", "go_version", "cpu_limit", "benchtime"):
        if before["metadata"].get(field) != after["metadata"].get(field):
            if not allow_environment_change:
                raise ValueError(f"environment differs: {field}; rerun under matching conditions")
            warnings.append(f"environment differs: {field}; timing changes are not attributable to code alone")
    result = {"kind": before["kind"], "suite": before["suite"],
              "revision_before": before["metadata"].get("revision"), "revision_after": after["metadata"].get("revision"),
              "source_before": before["metadata"].get("source_sha256"), "source_after": after["metadata"].get("source_sha256"),
              "warnings": warnings, "changes": []}
    if before["kind"] == "micro":
        left, right = before["benchmarks"], after["benchmarks"]
        if left.keys() != right.keys():
            raise ValueError("benchmark sets differ; partial comparisons would hide missing cases")
        for name in sorted(left):
            if left[name].keys() != right[name].keys():
                raise ValueError(f"metric sets differ: {name}")
            for unit in left[name]:
                a, b = left[name][unit]["median"], right[name][unit]["median"]
                result["changes"].append({"name": name, "unit": unit, "before": a, "after": b,
                    "delta_percent": (b / a - 1) * 100 if a else None,
                    "samples_before": left[name][unit]["count"], "samples_after": right[name][unit]["count"]})
    elif before["kind"] == "scenarios":
        if before["metadata"].get("manifest_sha256") != after["metadata"].get("manifest_sha256"):
            raise ValueError("scenario manifest changed")
        if before["cases"].keys() != after["cases"].keys():
            raise ValueError("scenario sets differ")
        for name, case in before["cases"].items():
            new = after["cases"][name]
            result["changes"].append({"name": name,
                "pass_rate_before": case["passed"] / len(case["attempts"]),
                "pass_rate_after": new["passed"] / len(new["attempts"]),
                "duration_ms_before": case["duration_ms"]["median"], "duration_ms_after": new["duration_ms"]["median"]})
    else:
        raise ValueError("unknown report kind")
    return result


def compare_quality(before: dict, after: dict) -> dict:
    """Compare existing sanity-report baselines without mixing runtime scores in."""
    for field in ("schema_version", "harness", "tier"):
        if before.get(field) != after.get(field):
            raise ValueError(f"quality baseline differs in {field}")
    if before.get("schema_version") != 1 or before.get("harness") != "SanityHarness":
        raise ValueError("expected SanityHarness baseline schema 1")
    def tasks(report):
        indexed = {}
        for task in report["results"]:
            key = task["language"] + "/" + task["task"]
            if key in indexed or task["attempts"] < 1 or task["duration_ms"] < 0:
                raise ValueError("invalid or duplicate quality task")
            indexed[key] = task
        if not indexed or len(indexed) != report["tasks"]:
            raise ValueError("incomplete quality baseline")
        return indexed
    left, right = tasks(before), tasks(after)
    if left.keys() != right.keys():
        raise ValueError("quality task sets differ")
    if any(left[name]["attempts"] != right[name]["attempts"] for name in left):
        raise ValueError("attempt budgets differ; success rates would not be comparable")
    return {"kind": "quality", "harness": before["harness"], "tasks": len(left),
            "meldra_before": before.get("meldra_version", "unknown"), "meldra_after": after.get("meldra_version", "unknown"),
            "model_before": before["model"], "model_after": after["model"],
            "provider_before": before["provider"], "provider_after": after["provider"],
            "pass_rate_before": sum(t["status"] == "pass" for t in left.values()) / len(left),
            "pass_rate_after": sum(t["status"] == "pass" for t in right.values()) / len(right),
            "regressed": [n for n in left if left[n]["status"] == "pass" and right[n]["status"] != "pass"],
            "improved": [n for n in left if left[n]["status"] != "pass" and right[n]["status"] == "pass"],
            "duration_ms_before": sum(t["duration_ms"] for t in left.values()),
            "duration_ms_after": sum(t["duration_ms"] for t in right.values()),
            "estimated_cost_before": before.get("estimated_cost", "unknown"),
            "estimated_cost_after": after.get("estimated_cost", "unknown"),
            "note": "Recorded cost is caller-supplied; duration is summed task time, not parallel wall time. No token usage is inferred."}


def positive(value: str) -> int:
    result = int(value)
    if result <= 0:
        raise argparse.ArgumentTypeError("must be positive")
    return result


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    for name in ("micro", "scenarios"):
        command = sub.add_parser(name)
        command.add_argument("--output", type=Path, required=True)
        command.add_argument("--count", type=positive, default=5 if name == "micro" else 3)
        command.add_argument("--cpu", type=positive, default=1)
        command.add_argument("--timeout", type=positive, default=900)
        if name == "micro":
            command.add_argument("--benchtime", default="200ms")
            command.add_argument("--bench", default=".")
    for name in ("compare", "compare-quality"):
        command = sub.add_parser(name)
        command.add_argument("before", type=Path)
        command.add_argument("after", type=Path)
        command.add_argument("--output", type=Path)
        if name == "compare":
            command.add_argument("--allow-environment-change", action="store_true")
    args = parser.parse_args(argv)
    try:
        if args.command in {"micro", "scenarios"}:
            return run_measurements(args)
        before = json.loads(args.before.read_text())
        after = json.loads(args.after.read_text())
        result = compare_quality(before, after) if args.command == "compare-quality" else comparison(before, after, args.allow_environment_change)
        if args.output:
            if args.output.exists():
                raise ValueError("comparison output already exists; choose a new path")
            write_report(args.output, result)
            print(f"Comparison: {args.output}")
        else:
            print(json.dumps(result, indent=2, allow_nan=False))
        return 0
    except (ValueError, KeyError, OSError, subprocess.SubprocessError) as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
