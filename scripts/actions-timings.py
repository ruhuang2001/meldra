#!/usr/bin/env python3
"""Report GitHub Actions timing evidence (Python standard library and gh only).

Failure cases considered before implementation:
- gh missing, unauthenticated, or API failing: exit nonzero with the command error.
- Missing/malformed offline input or missing jobs: fail rather than invent evidence.
- Paginated jobs: fetch every page and reject incomplete offline job collections.
- Missing executed-step timestamps stay unknown; explicitly skipped steps take zero time.
- Negative/overlapping intervals: flag anomalies; never silently call them test time.
- Dependency waiting and reruns: queue is only a run-created-to-job-start estimate,
  includes needs waiting (and time before a rerun), not pure runner provisioning.
- Mixed outcomes/workflows: medians use only completed successful runs, grouped by
  workflow; job rows preserve all outcomes and attempts returned by the latest API.
- Setup classification ambiguity: disclose name-based rules and every step; keep
  post-job cleanup, artifact handling, and unattributed job time separate.
- Output failure: exit nonzero; raw inputs remain available for repeatable analysis.

Examples:
  python3 scripts/actions-timings.py --limit 50 --output .artifacts/actions-timings
  python3 scripts/actions-timings.py --runs before-runs.json --jobs before-jobs.json \
      --output .artifacts/ci-optimization/before-report
"""

import argparse
import csv
from datetime import datetime
import json
from pathlib import Path
import re
import statistics
import subprocess
import sys


METHOD = """All times are seconds. Queue estimate = workflow run created_at to job
started_at; it includes dependency/needs waiting and any time before a rerun, so
it is not a measurement of runner provisioning. Setup sums steps named Set up job,
Create job directory, checkout, setup, cache restore/save, dependency download,
or tool installation. Cleanup includes Post steps and Complete job. Artifact
upload/download/retention is overhead. Execution sums other steps, including
builds, checks, releases and scans; it is not exclusively test execution. Rules
are name-based heuristics; per-step evidence below makes them reviewable.
Unattributed time = job wall time minus all measured step durations, reported as
overhead only when all step durations are known and nonoverlapping. Missing or
invalid timestamps remain unknown. Explicitly skipped steps take zero execution
time, even without timestamps. Workflow wall time uses the latest job completion
minus run creation,
not updated_at (which can change later). Workflow medians include only completed
successful runs with complete job timing evidence. Phase medians per workflow
are per-run sums of job phases (runner-seconds; queue sums are waiting-seconds),
not critical-path wall time. Job medians group exact job names within workflows
and include only successful jobs with complete timing evidence.
Workflow grouping uses workflowDatabaseId; legacy offline input without IDs falls
back to display names and cannot distinguish identical names.
Raw jobs are the latest attempt returned by GitHub, with run_attempt preserved.
"""


def gh(*args):
    result = subprocess.run(["gh", *args], capture_output=True, text=True, check=True)
    return json.loads(result.stdout)


def read_json(path):
    return json.loads(path.read_text())


def write_json(path, data):
    path.write_text(json.dumps(data, indent=2, ensure_ascii=False) + "\n")


def timestamp(value):
    if not value:
        return None
    try:
        result = datetime.fromisoformat(value.replace("Z", "+00:00"))
        return result if result.tzinfo else None
    except (ValueError, TypeError, AttributeError):
        return None


def duration(start, end):
    start, end = timestamp(start), timestamp(end)
    if start is None or end is None or end < start:
        return None
    return (end - start).total_seconds()


def phase(name):
    name = name.lower()
    if name.startswith("post ") or name in ("complete job", "cleanup", "clean up job"):
        return "cleanup"
    if re.search(r"(?:upload|download).*?(?:artifact|archive)|retain .*evidence", name):
        return "overhead"
    if (name in ("set up job", "create job directory")
            or re.search(r"actions/(checkout|setup-|cache)|^(checkout|check out|set up|setup|install|restore .*cache|save .*cache|download dependencies)\b", name)):
        return "setup"
    return "execution"


def total(values):
    return None if any(v is None for v in values) else sum(values)


PHASES = ("queue", "setup", "execution", "cleanup", "overhead")


def workflow_key(run):
    identity = run.get("workflowDatabaseId")
    return str(identity) if identity is not None else "legacy-name:" + run["workflowName"]


def analyze_job(run, job):
    steps = [{"number": s.get("number"), "name": s["name"],
              "phase": phase(s["name"]), "conclusion": s.get("conclusion"),
              "started_at": s.get("started_at"), "completed_at": s.get("completed_at"),
              "seconds": 0.0 if s.get("conclusion") == "skipped" else
              duration(s.get("started_at"), s.get("completed_at"))}
             for s in job.get("steps", [])]
    wall = duration(job.get("started_at"), job.get("completed_at"))
    measured = total([s["seconds"] for s in steps]) if steps else None
    notes = []
    intervals = sorted((timestamp(s["started_at"]), timestamp(s["completed_at"]))
                       for s in steps if s["seconds"] is not None and s["seconds"] > 0)
    overlaps = any(a[1] > b[0] for a, b in zip(intervals, intervals[1:]))
    bounds = (timestamp(job.get("started_at")), timestamp(job.get("completed_at")))
    outside = any(bounds[0] and a < bounds[0] or bounds[1] and b > bounds[1]
                  for a, b in intervals)
    if overlaps or outside:
        notes.append("Overlapping steps or steps outside job bounds; overhead is unknown.")
    residual = (wall - measured if wall is not None and measured is not None
                and not overlaps and not outside and wall >= measured else None)
    if residual is None:
        notes.append("Unattributed time cannot be reliably measured.")
    row = {"run_id": run["databaseId"], "workflow": run["workflowName"],
           "workflow_id": run.get("workflowDatabaseId"), "workflow_key": workflow_key(run),
           "run_conclusion": run.get("conclusion"), "job_id": job["id"],
           "job": job["name"], "attempt": job.get("run_attempt"),
           "conclusion": job.get("conclusion"), "status": job.get("status"),
           "url": job.get("html_url"), "wall_seconds": wall,
           "queue_seconds": duration(run.get("createdAt"), job.get("started_at")),
           "unattributed_seconds": residual, "notes": notes, "steps": steps}
    for kind in PHASES[1:]:
        row[kind + "_seconds"] = total([s["seconds"] for s in steps if s["phase"] == kind]) if steps else None
    row["artifact_seconds"] = row["overhead_seconds"]
    row["overhead_seconds"] = total([row["artifact_seconds"], residual])
    return row


def median(values):
    known = [v for v in values if v is not None]
    return statistics.median(known) if known else None


def report(runs, jobs):
    rows, workflows = [], {}
    for run in runs:
        run_id = str(run["databaseId"])
        collection = jobs[run_id]
        if len(collection["jobs"]) != collection["total_count"]:
            raise ValueError(f"Incomplete jobs for run {run_id}")
        current = [analyze_job(run, job) for job in collection["jobs"]]
        rows.extend(current)
        successful = run.get("status") == "completed" and run.get("conclusion") == "success"
        group = workflows.setdefault(workflow_key(run), {"name": run["workflowName"], "workflow_id": run.get("workflowDatabaseId"), "runs": 0, "successful_runs": 0, "samples": []})
        group["runs"] += 1
        if not successful:
            continue
        group["successful_runs"] += 1
        ends = [timestamp(job.get("completed_at")) for job in collection["jobs"]]
        wall = (duration(run.get("createdAt"), max(ends).isoformat())
                if ends and all(ends) else None)
        sample = {"run_id": run["databaseId"], "wall_seconds": wall}
        for kind in PHASES:
            sample[kind + "_seconds"] = total([r[kind + "_seconds"] for r in current]) if current else None
        if all(value is not None for value in sample.values()):
            group["samples"].append(sample)
    for group in workflows.values():
        group["complete_timing_samples"] = len(group["samples"])
        group["medians_seconds"] = {key: median([s[key + "_seconds"] for s in group["samples"]])
                                     for key in ("wall", *PHASES)}
    job_groups = {}
    success_ids = {r["databaseId"] for r in runs if r.get("status") == "completed" and r.get("conclusion") == "success"}
    for row in rows:
        if (row["run_id"] in success_ids and row["conclusion"] == "success"
                and all(row[key + "_seconds"] is not None for key in ("wall", *PHASES))):
            job_groups.setdefault((row["workflow_key"], row["job"]), []).append(row)
    summaries = [{"workflow": items[0]["workflow"], "workflow_id": items[0]["workflow_id"], "job": name, "samples": len(items),
                  "medians_seconds": {key: median([r[key + "_seconds"] for r in items]) for key in ("wall", *PHASES)}}
                 for (workflow, name), items in sorted(job_groups.items())]
    return {"methodology": METHOD, "workflows": workflows, "job_medians": summaries, "jobs": rows}


def cell(value):
    if value is None:
        return "unknown"
    if isinstance(value, float):
        return f"{value:.1f}"
    return str(value).replace("|", "\\|").replace("\n", " ")


def markdown(data):
    lines = ["# GitHub Actions timing report", "", METHOD, "", "## Successful workflow medians", "",
             "| Workflow | All runs | Successful | Complete samples | Wall | Queue sum | Setup sum | Execution sum | Cleanup sum | Overhead sum |",
             "| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |"]
    for name, group in sorted(data["workflows"].items()):
        values = [group["name"] + " [" + name + "]", group["runs"], group["successful_runs"], group["complete_timing_samples"]]
        values += [group["medians_seconds"][key] for key in ("wall", *PHASES)]
        lines.append("| " + " | ".join(map(cell, values)) + " |")
    lines += ["", "## Successful job medians", "", "| Workflow | Job | Samples | Wall | Queue | Setup | Execution | Cleanup | Overhead |",
              "| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |"]
    for group in data["job_medians"]:
        values = [group["workflow"] + " [" + str(group["workflow_id"]) + "]", group["job"], group["samples"]]
        values += [group["medians_seconds"][key] for key in ("wall", *PHASES)]
        lines.append("| " + " | ".join(map(cell, values)) + " |")
    lines += ["", "## Per-job evidence", ""]
    for row in data["jobs"]:
        lines += [f"### {cell(row['workflow'])}: {cell(row['job'])} ({row['job_id']})", "",
                  f"Run {row['run_id']}, attempt {row['attempt']}, conclusion {row['conclusion']}. {row['url'] or ''}", "",
                  "; ".join(f"{key}: {cell(row[key + '_seconds'])} s" for key in ("wall", *PHASES)) + ".",
                  f"Artifact handling: {cell(row['artifact_seconds'])} s; unattributed: {cell(row['unattributed_seconds'])} s.",
                  *row["notes"], "", "| Step | Phase | Outcome | Seconds |", "| --- | --- | --- | ---: |"]
        lines += ["| " + " | ".join(cell(s[key]) for key in ("name", "phase", "conclusion", "seconds")) + " |" for s in row["steps"]]
        lines.append("")
    return "\n".join(lines) + "\n"


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--repo", help="owner/repo; defaults to gh repo view")
    parser.add_argument("--limit", type=int, default=50, help="number of recent runs to fetch (default: 50)")
    parser.add_argument("--runs", type=Path, help="offline gh run list JSON")
    parser.add_argument("--jobs", type=Path, help="offline JSON mapping run IDs to jobs API objects")
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    if bool(args.runs) != bool(args.jobs):
        parser.error("--runs and --jobs must be supplied together")
    if args.limit < 1:
        parser.error("--limit must be positive")
    args.output.mkdir(parents=True, exist_ok=True)
    if args.runs:
        runs, jobs = read_json(args.runs), read_json(args.jobs)
    else:
        repo = args.repo or gh("repo", "view", "--json", "nameWithOwner")["nameWithOwner"]
        fields = "databaseId,workflowDatabaseId,workflowName,createdAt,startedAt,updatedAt,status,conclusion,event,headBranch,headSha,url"
        runs = gh("run", "list", "--repo", repo, "--limit", str(args.limit), "--json", fields)
        write_json(args.output / "runs.json", runs)
        jobs = {}
        for run in runs:
            pages = gh("api", "--paginate", "--slurp", f"repos/{repo}/actions/runs/{run['databaseId']}/jobs?per_page=100&filter=latest")
            jobs[str(run["databaseId"])] = {"total_count": pages[0]["total_count"], "jobs": [job for page in pages for job in page["jobs"]]}
            write_json(args.output / "jobs.json", jobs)
    write_json(args.output / "runs.json", runs)
    write_json(args.output / "jobs.json", jobs)
    data = report(runs, jobs)
    write_json(args.output / "report.json", data)
    (args.output / "report.md").write_text(markdown(data))
    fields = ["run_id", "workflow", "workflow_id", "run_conclusion", "job_id", "job", "attempt", "status", "conclusion", "wall_seconds",
              *[kind + "_seconds" for kind in PHASES], "artifact_seconds", "unattributed_seconds", "url"]
    with (args.output / "jobs.csv").open("w", newline="") as stream:
        writer = csv.DictWriter(stream, fieldnames=fields, extrasaction="ignore")
        writer.writeheader()
        writer.writerows(data["jobs"])
    print(f"Reported {len(runs)} runs and {len(data['jobs'])} jobs in {args.output}")


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, KeyError, TypeError, subprocess.CalledProcessError) as exc:
        detail = exc.stderr.strip() if isinstance(exc, subprocess.CalledProcessError) else str(exc)
        print(f"actions-timings: {detail}", file=sys.stderr)
        sys.exit(1)
