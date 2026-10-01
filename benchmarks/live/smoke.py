#!/usr/bin/env python3
"""Exercise Docker + real Meldra + budget gate + hidden graders without an API.

The upstream is scripted with authored reference repairs. This verifies harness
plumbing, not model quality. It does not read credentials or access a provider.
"""
from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import tempfile
import shutil
from types import SimpleNamespace

import execute
import gate
import run as cohort_runner
from test_run import REFERENCE_FIXES


def smoke(binary: Path, image: str, report: Path) -> dict:
    with tempfile.TemporaryDirectory(prefix="meldra-live-smoke-") as temporary:
        root = Path(temporary)
        plan = cohort_runner.budget_plan("offline-scripted", "https://offline.invalid/v1",
                                         "2", "10", 10000, 1000, "5", "0", 10)
        plan_path = root / "plan.json"
        plan_path.write_text(json.dumps(plan))
        tasks = iter(cohort_runner.cohort()["tasks"])

        def gateway_factory(config, **kwargs):
            task = next(tasks)
            requests = 0

            def transport(path, payload, max_bytes, deadline):
                nonlocal requests
                request = json.loads(payload)
                if path == "/responses/input_tokens":
                    return 200, "application/json", b'{"input_tokens":200}'
                if path != "/responses":
                    raise AssertionError("unexpected provider route")
                requests += 1
                if request["max_output_tokens"] != 1000 or request["include"] != ["reasoning.encrypted_content"]:
                    raise AssertionError("generation cap or stateless reasoning missing")
                if requests == 1:
                    originals = cohort_runner.templates(task, "fixtures")
                    changes = [{"path": name, "old_str": originals[name].decode(), "new_str": text}
                               for name, text in REFERENCE_FIXES[task["id"]].items()]
                    output = [
                        {"type": "reasoning", "id": "reason-one", "summary": [], "encrypted_content": "fixture-encrypted"},
                        {"type": "message", "id": "commentary", "role": "assistant", "status": "completed", "phase": "commentary",
                         "content": [{"type": "output_text", "text": "Applying fixture repair.", "annotations": []}]},
                        {"type": "function_call", "id": "tool-one", "call_id": "fixture-edit", "name": "apply_patch",
                         "status": "completed", "arguments": json.dumps({"patch": None, "changes": changes})},
                    ]
                else:
                    if requests != 2 or not isinstance(request["input"], list):
                        raise AssertionError("unexpected inference sequence")
                    if not any(item.get("encrypted_content") == "fixture-encrypted" for item in request["input"]):
                        raise AssertionError("encrypted reasoning lost on replay")
                    if not any(item.get("type") == "function_call_output" and "Applied successfully" in item.get("output", "") for item in request["input"]):
                        raise AssertionError("real tool execution did not succeed")
                    output = [{"type": "message", "id": "answer", "role": "assistant", "status": "completed", "phase": "final_answer",
                               "content": [{"type": "output_text", "text": "Fixture repair complete.", "annotations": []}]}]
                response = {"id": "fixture-response-" + str(requests), "status": "completed", "output": output,
                            "usage": {"input_tokens": 200, "output_tokens": 30, "total_tokens": 230}}
                event = {"type": "response.completed", "response": response}
                data = b"event: response.completed\ndata: " + json.dumps(event).encode() + b"\n\n"
                if len(data) > max_bytes:
                    raise AssertionError("fixture exceeds response bound")
                return 200, "text/event-stream", data

            return gate.BudgetGateway(config, _transport=transport, **kwargs)

        variable = "MELDRA_OFFLINE_SMOKE_KEY"
        prior = os.environ.get(variable)
        os.environ[variable] = "offline-fixture-key-no-provider-access"
        try:
            result = execute.run_evaluation(SimpleNamespace(execute_live=True, approved_plan=plan_path,
                api_key_env=variable, binary=binary, image=image, run_dir=root / "attempts", output=report, timeout=60),
                gateway_factory=gateway_factory)
            if result["model_quality_evidence"] or result["live_model"]:
                raise AssertionError("scripted provider was mislabeled as live model evidence")
            if result["passed"] != 5:
                diagnostics=report.with_suffix(".diagnostics")
                diagnostics.mkdir(exist_ok=False,mode=0o700)
                failures=[]
                for item in result["results"]:
                    log=root/"attempts"/item["id"]/"agent.log"
                    shutil.copyfile(log,diagnostics/(item["id"]+".log"))
                    failures.append({"id":item["id"],"status":item["status"],"log":log.read_text()[:2000]})
                raise AssertionError("offline Docker pipeline failed: " + json.dumps(failures))
            return result
        finally:
            if prior is None:
                os.environ.pop(variable, None)
            else:
                os.environ[variable] = prior


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--image", required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    result = smoke(args.binary, args.image, args.output.resolve())
    print(json.dumps({"passed": result["passed"], "tasks": result["tasks"], "model_quality_evidence": False,
                      "report": str(args.output)}, indent=2))


if __name__ == "__main__":
    main()
