import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location("live_cohort", Path(__file__).with_name("run.py"))
runner = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(runner)


REFERENCE_FIXES = {
    "go-clamp": {"clamp.go": "package clamp\nfunc Clamp(value, low, high int) int { return min(high, max(low, value)) }\n"},
    "go-chunks": {
        "size.go": "package chunks\nfunc NormalizeChunkSize(size int) int { return max(1, size) }\n",
        "chunks.go": "package chunks\nfunc Split(values []int, size int) [][]int { var result [][]int; size=NormalizeChunkSize(size); for start:=0; start<len(values); start+=size { result=append(result,values[start:min(start+size,len(values))]) }; return result }\n",
    },
    "python-slug": {"slug.py": "import re\ndef slugify(text):\n    return re.sub('[^a-z0-9]+', '-', text.lower()).strip('-')\n"},
    "python-ledger": {
        "money.py": "import re\ndef parse_cents(value):\n    if re.fullmatch(r'[+-]?[0-9]+(?:\\.[0-9]{1,2})?', value) is None:\n        raise ValueError(value)\n    sign = -1 if value.startswith('-') else 1\n    whole, _, fraction = value.lstrip('+-').partition('.')\n    return sign * (int(whole) * 100 + int(fraction.ljust(2, '0')))\n",
        "ledger.py": "from money import parse_cents\ndef total_cents(records):\n    return sum(parse_cents(r['amount']) for r in records if not r.get('void', False))\n",
    },
    "python-config": {"config.py": "from copy import deepcopy\ndef merge_config(defaults, overrides):\n    result = deepcopy(defaults)\n    for key, value in overrides.items():\n        result[key] = merge_config(result[key], value) if isinstance(result.get(key), dict) and isinstance(value, dict) else deepcopy(value)\n    return result\n"},
}


class CohortTests(unittest.TestCase):
    def test_prepare_keeps_graders_separate_and_refuses_overwrite(self):
        with tempfile.TemporaryDirectory() as tmp:
            output = Path(tmp) / "run"
            manifest = runner.prepare(output)
            self.assertEqual(len(manifest["tasks"]), 5)
            self.assertFalse(manifest["live_model"])
            for task in runner.cohort()["tasks"]:
                workspace = output / task["id"] / "workspace"
                self.assertFalse(list(workspace.glob("*test*")))
                self.assertEqual(runner.validate_submission(workspace, task), [])
            with self.assertRaises(FileExistsError):
                runner.prepare(output)

    def test_budget_fails_closed_and_never_claims_enforcement(self):
        result = runner.budget_plan("configured-model", "configured-provider", "2", "10", 10000, 1000, "5")
        self.assertEqual(result["conditional_cost_upper_bound_usd"], "0.5")
        self.assertFalse(result["budget_enforced"])
        self.assertFalse(result["live_execution_enabled"])
        for bad in ("unknown", "NaN", "Infinity", "0", "-1"):
            with self.subTest(bad=bad), self.assertRaises(ValueError):
                runner.budget_plan("model", "provider", bad, "10", 10000, 1000, "5")
        with self.assertRaises(ValueError):
            runner.budget_plan("model", "provider", "2", "10", 1000000, 1000, "5")
        with self.assertRaises(ValueError):
            runner.budget_plan("model", "provider", "2", "10", 100, 1000, "5")

    def test_docker_grader_has_no_network_host_config_or_socket(self):
        args = runner.grader_argv("sha256:image", Path("/tmp/work"), Path("/tmp/grader"), "go", "grade-one")
        self.assertEqual(args[args.index("--network") + 1], "none")
        self.assertIn("--read-only", args)
        self.assertIn("no-new-privileges", args)
        self.assertIn("--cap-drop", args)
        mounts = [args[i + 1] for i, arg in enumerate(args) if arg == "--mount"]
        self.assertEqual(len(mounts), 2)
        self.assertTrue(all(mount.endswith(",readonly") for mount in mounts))
        self.assertNotIn("docker.sock", " ".join(args))
        self.assertNotIn("OPENAI_API_KEY", " ".join(args))
        self.assertNotIn("--yes", args)

    def test_changed_support_files_and_symlinks_are_rejected(self):
        with tempfile.TemporaryDirectory() as tmp:
            output = Path(tmp) / "run"
            runner.prepare(output)
            task = runner.cohort()["tasks"][0]
            workspace = output / task["id"] / "workspace"
            (workspace / "go.mod").write_text("module changed\n")
            self.assertTrue(any("support file" in error for error in runner.validate_submission(workspace, task)))
            (workspace / "clamp.go").unlink()
            (workspace / "clamp.go").symlink_to("/etc/passwd")
            self.assertTrue(any("symlink" in error for error in runner.validate_submission(workspace, task)))

    def test_grader_failure_remains_in_denominator(self):
        with tempfile.TemporaryDirectory() as tmp:
            output = Path(tmp) / "run"
            runner.prepare(output)
            report = Path(tmp) / "report.json"
            with patch.object(runner.subprocess, "check_output", return_value="sha256:fixed\n"), \
                 patch.object(runner, "bounded_run", side_effect=[(1, "failed", False)] + [(0, "ok", False)] * 4):
                result = runner.grade(output, "image", report)
            self.assertEqual((result["passed"], result["tasks"]), (4, 5))
            self.assertFalse(result["model_quality_evidence"])
            self.assertIsNone(result["results"][0]["cost_usd"])
            self.assertEqual(json.loads(report.read_text())["results"][0]["exit_code"], 1)
            with self.assertRaises(ValueError):
                runner.grade(output, "image", report)

    def test_broken_fixtures_fail_and_known_repairs_pass(self):
        # Only this test's authored fixtures and reference fixes run locally.
        # Model-generated submissions always use grade() and Docker isolation.
        environment = {**os.environ, "GOTOOLCHAIN": "local", "GOPROXY": "off", "PYTHONDONTWRITEBYTECODE": "1"}
        goroot = subprocess.check_output(["go", "env", "GOROOT"], cwd=runner.ROOT.parents[1], text=True).strip()
        for task in runner.cohort()["tasks"]:
            with self.subTest(task=task["id"]), tempfile.TemporaryDirectory() as tmp:
                workspace = Path(tmp)
                for name, data in {**runner.templates(task, "fixtures"), **runner.templates(task, "graders")}.items():
                    (workspace / name).write_bytes(data)
                command = [str(Path(goroot) / "bin" / "go"), "test", "-count=1", "-timeout=15s", "./..."] if task["language"] == "go" else ["python3", "-m", "unittest", "discover", "-p", "test_hidden.py"]
                broken = subprocess.run(command, cwd=workspace, env=environment, capture_output=True, timeout=60)
                self.assertNotEqual(broken.returncode, 0, broken.stdout.decode())
                for name, contents in REFERENCE_FIXES[task["id"]].items():
                    (workspace / name).write_text(contents)
                fixed = subprocess.run(command, cwd=workspace, env=environment, capture_output=True, timeout=60)
                self.assertEqual(fixed.returncode, 0, fixed.stdout.decode() + fixed.stderr.decode())


if __name__ == "__main__":
    unittest.main()
