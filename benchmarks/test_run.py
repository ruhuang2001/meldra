import argparse
import copy
import importlib.util
import io
import json
from pathlib import Path
import tempfile
import subprocess
import unittest
from unittest import mock

SPEC = importlib.util.spec_from_file_location("benchmark_runner", Path(__file__).with_name("run.py"))
runner = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(runner)


class ReportsTest(unittest.TestCase):
    def event(self, **kwargs):
        return {"Package": "meldra/internal/app", **kwargs}

    def micro(self):
        return runner.parse_micro([
            self.event(Output="BenchmarkAgentTurn/tools_1-1 100 10 ns/op 20 B/op 2 allocs/op\n"),
            self.event(Output="BenchmarkAgentTurn/tools_1-1 200 30 ns/op 40 B/op 4 allocs/op\n"),
        ], 2)

    def report(self):
        return {"schema_version": 1, "kind": "micro", "suite": "runtime-micro-v1", "completed": True,
                "metadata": {"os": "test", "arch": "test", "cpu": "test", "cpu_limit": 1, "go_version": "test", "benchtime": "200ms"},
                "benchmarks": self.micro()}

    def test_micro_preserves_raw_samples_and_units(self):
        case = self.micro()["meldra/internal/app/BenchmarkAgentTurn/tools_1"]
        self.assertEqual(case["ns/op"]["samples"], [10, 30])
        self.assertEqual(case["ns/op"]["median"], 20)
        self.assertEqual(case["allocs/op"]["max"], 4)

    def test_micro_rejects_empty_incomplete_nonfinite(self):
        for events, count in [([], 1), ([self.event(Output="BenchmarkX-1 1 5 ns/op\n")], 2),
                              ([self.event(Output="BenchmarkX-1 1 NaN ns/op\n")], 1)]:
            with self.assertRaises(ValueError):
                runner.parse_micro(events, count)

    def test_scenarios_count_failures_and_skips(self):
        events = [self.event(Action="run", Test="TestX"), self.event(Action="output", Test="TestX", Output='x.go:1: EVAL_METRICS {"duration_ms":1.5,"tool_calls":2}\n'),
                  self.event(Action="pass", Test="TestX", Elapsed=.1), self.event(Action="run", Test="TestX"),
                  self.event(Action="fail", Test="TestX", Elapsed=.2), self.event(Action="run", Test="TestY"),
                  self.event(Action="skip", Test="TestY", Elapsed=0), self.event(Action="run", Test="TestY"),
                  self.event(Action="pass", Test="TestY", Elapsed=.1)]
        result = runner.parse_scenarios(events, ["TestX", "TestY"], 2)
        self.assertEqual(result["TestX"]["passed"], 1)
        self.assertEqual(result["TestY"]["passed"], 1)
        self.assertEqual(result["TestX"]["attempts"][0]["timing_scope"], "turn")
        self.assertEqual(result["TestX"]["attempts"][1]["duration_ms"], 200)

    def test_scenarios_reject_missing_unfinished_duplicate(self):
        cases = [([], ["TestX"], 1), ([self.event(Action="run", Test="TestX")], ["TestX"], 1),
                 ([], ["TestX", "TestX"], 1), ([self.event(Action="pass", Test="TestX")], ["TestX"], 1)]
        for events, names, count in cases:
            with self.assertRaises(ValueError):
                runner.parse_scenarios(events, names, count)

    def test_comparison_checks_environment_cases_completion(self):
        before = self.report()
        for change in (lambda r: r["metadata"].update(cpu="other"), lambda r: r.update(completed=False),
                       lambda r: r["benchmarks"].clear(), lambda r: r.update(kind="scenarios")):
            after = copy.deepcopy(before)
            change(after)
            with self.assertRaises(ValueError):
                runner.comparison(before, after)
        after = copy.deepcopy(before)
        after["metadata"]["cpu"] = "other"
        self.assertTrue(runner.comparison(before, after, True)["warnings"])

    def test_comparison_handles_zero_without_infinity(self):
        before = self.report()
        key = next(iter(before["benchmarks"]))
        before["benchmarks"][key]["allocs/op"]["median"] = 0
        result = runner.comparison(before, self.report())
        json.dumps(result, allow_nan=False)
        self.assertIsNone(next(r for r in result["changes"] if r["unit"] == "allocs/op")["delta_percent"])

    def quality(self):
        return {"schema_version": 1, "harness": "SanityHarness", "tier": "core", "model": "m", "provider": "p", "tasks": 2,
                "results": [{"language": "go", "task": "a", "status": "pass", "duration_ms": 10, "attempts": 1},
                            {"language": "python", "task": "b", "status": "fail", "duration_ms": 20, "attempts": 1}]}

    def test_quality_identifies_regressions_without_inventing_cost(self):
        before, after = self.quality(), self.quality()
        after["results"][0]["status"] = "fail"
        result = runner.compare_quality(before, after)
        self.assertEqual(result["regressed"], ["go/a"])
        self.assertEqual(result["pass_rate_before"], .5)
        self.assertEqual(result["estimated_cost_before"], "unknown")

    def test_quality_rejects_changed_task_or_attempt_budget(self):
        for field, value in [("task", "other"), ("attempts", 2)]:
            after = self.quality()
            after["results"][0][field] = value
            with self.assertRaises(ValueError):
                runner.compare_quality(self.quality(), after)
        after = self.quality()
        after["results"][1] = after["results"][0]
        with self.assertRaises(ValueError):
            runner.compare_quality(self.quality(), after)

    def test_atomic_report_does_not_corrupt_previous_on_encoding_error(self):
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder) / "result.json"
            runner.write_report(path, {"ok": True})
            with self.assertRaises(ValueError):
                runner.write_report(path, {"bad": float("nan")})
            self.assertEqual(json.loads(path.read_text()), {"ok": True})
            self.assertEqual(len(list(Path(folder).iterdir())), 1)

    def test_failed_scenario_produces_comparable_report_and_nonzero_exit(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "benchmarks/suites").mkdir(parents=True)
            (root / "benchmarks/suites/offline.json").write_text(json.dumps({"cases": ["TestX"], "name": "test", "package": "./app"}))
            output = root / "report.json"
            args = argparse.Namespace(output=output, count=1, cpu=1, command="scenarios", timeout=10)
            def execute(command, stream, timeout):
                for event in [self.event(Action="run", Test="TestX"), self.event(Action="fail", Test="TestX", Elapsed=.1)]:
                    stream.write(json.dumps(event) + "\n")
                return 1
            with mock.patch.object(runner, "ROOT", root), mock.patch.object(runner, "metadata", return_value={}), mock.patch.object(runner, "execute_go", side_effect=execute), mock.patch("sys.stdout", new_callable=io.StringIO):
                self.assertEqual(runner.run_measurements(args), 1)
            report = json.loads(output.read_text())
            self.assertTrue(report["completed"])
            self.assertEqual(report["summary"]["pass_rate"], 0)
            self.assertEqual(runner.comparison(report, report)["changes"][0]["pass_rate_after"], 0)

    def test_timeout_terminates_and_reaps_test_process_group(self):
        process = mock.Mock()
        process.pid = 12345
        process.wait.side_effect = [subprocess.TimeoutExpired("go", 1), 0]
        with mock.patch.object(runner.subprocess, "Popen", return_value=process), mock.patch.object(runner.os, "killpg") as kill:
            with self.assertRaises(subprocess.TimeoutExpired):
                runner.execute_go(["go", "test"], io.StringIO(), 1)
            if runner.os.name == "posix":
                kill.assert_called_once_with(12345, runner.signal.SIGKILL)
            else:
                process.kill.assert_called_once()
        self.assertEqual(process.wait.call_count, 2)

    def test_cli_help_does_not_run_benchmarks(self):
        with mock.patch.object(runner, "run_measurements") as run, mock.patch("sys.stdout", new_callable=io.StringIO):
            with self.assertRaises(SystemExit) as raised:
                runner.main(["--help"])
            self.assertEqual(raised.exception.code, 0)
            run.assert_not_called()


if __name__ == "__main__":
    unittest.main()
