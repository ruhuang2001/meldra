import argparse
from decimal import localcontext
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import Mock, patch

import execute as runner


IMAGE = 'sha256:' + 'a' * 64
KEY = 'private-upstream-key-for-offline-test'


class FakeGateway:
    instances = []

    def __init__(self, config, **kwargs):
        self.config = config
        self.kwargs = kwargs
        self.address = ('0.0.0.0', 12345)
        self.closed = False
        self.__class__.instances.append(self)

    def __enter__(self):
        return self

    def __exit__(self, *args):
        self.closed = True

    def report(self):
        return {'requests': 1, 'reserved_tokens': 80, 'reserved_cost_usd': '0.001',
                'reported_input_tokens': 16, 'reported_output_tokens': 8,
                'usage_complete': True, 'reservation_integrity': True, 'closed': self.closed,
                'transport_mode': 'https', 'requests_detail': [
                    {'status': 'completed', 'generation_reservation_usd': '0.001'}]}


class ExecutionTests(unittest.TestCase):
    def setUp(self):
        FakeGateway.instances = []

    def make_args(self, directory):
        root = Path(directory)
        plan = runner.cohort_runner.budget_plan('exact-model', 'https://provider.example/v1',
                                              '2', '10', 1000, 64, '1', '0', 10)
        (root / 'plan.json').write_text(json.dumps(plan))
        (root / 'meldra').write_bytes(b'\x7fELFfake-linux-binary')
        return argparse.Namespace(execute_live=True, approved_plan=root / 'plan.json',
                                  api_key_env='EXPLICIT_EVAL_KEY', binary=root / 'meldra',
                                  image='trusted-eval-image', run_dir=root / 'attempt',
                                  output=root / 'report.json', timeout=300)

    def preflight(self, argv, **kwargs):
        if argv[:3] == ['go', 'version', '-m']:
            return '\tbuild\tGOOS=linux\n\tbuild\tGOARCH=arm64\n\tbuild\tvcs.revision=abc123\n\tbuild\tvcs.modified=false\n'
        self.assertEqual(argv[:3], ['docker', 'image', 'inspect'])
        return IMAGE + '\n'

    def grade(self, run_dir, image, output):
        manifest = json.loads((run_dir / 'manifest.json').read_text())
        self.assertEqual(len(manifest['tasks']), 5)
        self.assertTrue(all(task['status'] not in ('running', 'not_started') for task in manifest['tasks']))
        self.assertEqual(image, IMAGE)
        self.assertTrue(all(g.closed for g in FakeGateway.instances))
        return {'results': [{'id': task['id'], 'passed': True, 'exit_code': 0}
                            for task in runner.cohort_runner.cohort()['tasks']]}

    def test_plan_is_exact_frozen_cohort_and_budget_and_unknown_fields_not_forwarded(self):
        with tempfile.TemporaryDirectory() as tmp:
            args = self.make_args(tmp)
            plan, digest = runner.load_plan(args.approved_plan)
            self.assertEqual(len(digest), 64)
            self.assertEqual(plan['model'], 'exact-model')
            self.assertEqual(plan['max_requests'], 10)
            self.assertEqual(runner.config_for_plan(plan, '1').timeout_seconds, 60)
            raw = json.loads(args.approved_plan.read_text())
            raw['secret_unknown'] = KEY
            args.approved_plan.write_text(json.dumps(raw))
            self.assertNotIn('secret_unknown', runner.load_plan(args.approved_plan)[0])
            for field, value in [('cohort_sha256', 'old'), ('schema_version', 1),
                                 ('conditional_cost_upper_bound_usd', '0'), ('model', 123)]:
                with self.subTest(field=field):
                    changed = dict(raw, **{field: value})
                    args.approved_plan.write_text(json.dumps(changed))
                    with self.assertRaises(ValueError):
                        runner.load_plan(args.approved_plan)

    def test_explicit_opt_in_and_explicit_credential_required_before_any_docker(self):
        with tempfile.TemporaryDirectory() as tmp, patch.object(runner.subprocess, 'check_output') as command:
            args = self.make_args(tmp)
            args.execute_live = False
            with self.assertRaisesRegex(ValueError, 'execute-live'):
                runner.run_evaluation(args)
            args.execute_live = True
            with patch.dict(os.environ, {'OPENAI_API_KEY': KEY}, clear=True):
                with self.assertRaisesRegex(ValueError, 'explicitly named'):
                    runner.run_evaluation(args)
            command.assert_not_called()

    def test_docker_isolates_writable_fixture_and_readonly_binary_not_host_config(self):
        argv = runner.agent_argv(IMAGE, Path('/tmp/task/workspace'), Path('/tmp/pinned-binary'),
                                 'attempt-one', 1234, 'bounded-nonce', 'model', 'prompt', 'marker')
        self.assertEqual(argv[argv.index('--network') + 1], 'bridge')
        self.assertNotEqual(argv[argv.index('--user') + 1].split(':')[0], '0')
        self.assertIn('--read-only', argv)
        self.assertIn('no-new-privileges', argv)
        self.assertIn('host.docker.internal:host-gateway', argv)
        mounts = [argv[index + 1] for index, item in enumerate(argv) if item == '--mount']
        self.assertEqual(len(mounts), 2)
        self.assertTrue(mounts[0].endswith('target=/workspace'))
        self.assertTrue(mounts[1].endswith('target=/opt/meldra,readonly'))
        for forbidden in ('docker.sock', '/grader', str(Path.home()), KEY):
            self.assertNotIn(forbidden, ' '.join(argv))
        self.assertEqual(argv[-8:-6], ['python3', '-c'])
        with self.assertRaises(ValueError):
            runner.agent_argv(IMAGE, Path('/tmp/comma,bad'), Path('/tmp/bin'), 'n', 1, 'n', 'm', 'p', 'q')

    def test_container_bootstrap_creates_private_config_and_only_gateway_credentials(self):
        # Execute the trusted wrapper with a tiny authored stand-in. This checks
        # the config contract without Docker, a model call or generated code.
        with tempfile.TemporaryDirectory() as tmp:
            binary = Path(tmp) / 'fake-meldra'
            binary.write_text('#!' + sys.executable + '\n'
                              'import os, pathlib\n'
                              'config=pathlib.Path(os.environ["MELDRA_HOME"])/"config.toml"\n'
                              'assert config.stat().st_mode & 0o777 == 0o600\n'
                              'assert config.parent.stat().st_mode & 0o777 == 0o700\n'
                              'assert os.environ["OPENAI_API_KEY"] == "test-only-nonce"\n'
                              'assert os.environ["OPENAI_BASE_URL"].startswith("http://127.0.0.1:")\n'
                              'assert config.read_text() == "allow_insecure_base_url = true\\n"\n')
            binary.chmod(0o755)
            script = runner.CONTAINER_SCRIPT.replace('/tmp/meldra', str(Path(tmp) / 'config'))
            script = script.replace('/opt/meldra', str(binary))
            result = subprocess.run([sys.executable, '-c', script, '9', 'test-only-nonce',
                                     'offline-model', 'offline prompt', 'metrics:'],
                                    capture_output=True, timeout=5)
            self.assertEqual(result.returncode, 0, result.stderr.decode())
            self.assertIn('metrics:', result.stdout.decode())

    def test_failed_attempt_kept_in_denominator_logs_private_budget_shared_and_no_rerun(self):
        with tempfile.TemporaryDirectory() as tmp, patch.dict(os.environ, {'EXPLICIT_EVAL_KEY': KEY}):
            args = self.make_args(tmp)
            calls = []

            def attempt(argv, name, timeout):
                self.assertEqual(timeout, 300)
                self.assertNotIn(KEY, repr(argv))
                calls.append(argv)
                self.assertTrue(all(g.closed for g in FakeGateway.instances[:-1]))
                marker, nonce = argv[-1], argv[-4]
                payload = KEY + ' ' + nonce + '\n' + marker + '{"tool_calls":2,"tool_statuses":{"succeeded":2}}\n'
                return (1 if len(calls) == 1 else 0), payload.encode(), False

            with patch.object(runner.subprocess, 'check_output', side_effect=self.preflight), \
                 patch.object(runner, 'docker_attempt', side_effect=attempt), \
                 patch.object(runner.cohort_runner, 'grade', side_effect=self.grade):
                result = runner.run_evaluation(args, gateway_factory=FakeGateway)
                self.assertEqual((result['tasks'], result['passed']), (5, 4))
                self.assertFalse(result['model_quality_evidence'])
                self.assertFalse(result['live_model'])
                self.assertEqual(result['kind'], 'offline_execution_test')
                self.assertEqual(result['reserved_cost_usd'], '0.005')
                self.assertIsNone(result['reported_cost_usd'])
                self.assertEqual([str(g.config.max_cost_usd) for g in FakeGateway.instances],
                                 ['1', '0.999', '0.998', '0.997', '0.996'])
                self.assertEqual(len(set(g.kwargs['bearer_nonce'] for g in FakeGateway.instances)), 5)
                for entry in result['results']:
                    log = args.run_dir / entry['log']
                    self.assertEqual(log.stat().st_mode & 0o777, 0o600)
                    self.assertNotIn(KEY, log.read_text())
                    self.assertNotIn(FakeGateway.instances[0].kwargs['bearer_nonce'], log.read_text())
                    self.assertEqual(entry['tool_calls'], 2)
                self.assertEqual(args.output.stat().st_mode & 0o777, 0o600)
                self.assertNotIn(KEY, args.output.read_text())
                self.assertEqual(result['binary_revision'], 'abc123')
                with self.assertRaisesRegex(ValueError, 'exists'):
                    runner.run_evaluation(args, gateway_factory=FakeGateway)
                self.assertEqual(len(calls), 5)

    def test_global_reservations_ignore_low_callers_decimal_precision(self):
        with tempfile.TemporaryDirectory() as tmp, patch.dict(os.environ, {'EXPLICIT_EVAL_KEY': KEY}):
            args = self.make_args(tmp)
            with localcontext() as context:
                context.prec = 1
                with patch.object(runner.subprocess, 'check_output', side_effect=self.preflight), \
                     patch.object(runner, 'docker_attempt', return_value=(0, b'', False)), \
                     patch.object(runner.cohort_runner, 'grade', side_effect=self.grade):
                    result = runner.run_evaluation(args, gateway_factory=FakeGateway)
                self.assertEqual(context.prec, 1)
            self.assertEqual([str(g.config.max_cost_usd) for g in FakeGateway.instances],
                             ['1', '0.999', '0.998', '0.997', '0.996'])
            self.assertEqual(result['reserved_cost_usd'], '0.005')

    def test_interruption_records_five_outcomes_and_closes_gateway_before_grade(self):
        with tempfile.TemporaryDirectory() as tmp, patch.dict(os.environ, {'EXPLICIT_EVAL_KEY': KEY}):
            args = self.make_args(tmp)
            with patch.object(runner.subprocess, 'check_output', side_effect=self.preflight), \
                 patch.object(runner, 'docker_attempt', side_effect=[(0, b'first', False), runner.DockerInterrupted]), \
                 patch.object(runner.cohort_runner, 'grade', side_effect=self.grade):
                result = runner.run_evaluation(args, gateway_factory=FakeGateway)
            self.assertEqual(result['tasks'], 5)
            self.assertEqual(result['passed'], 1)
            self.assertTrue(result['interrupted'])
            self.assertEqual([entry['status'] for entry in result['results']],
                             ['completed', 'interrupted'] + ['not_attempted_interrupted'] * 3)
            self.assertTrue(all(g.closed for g in FakeGateway.instances))
            self.assertTrue(result['results'][1]['cleanup_confirmed'])

    def test_unconfirmed_cleanup_stops_cohort_and_forbids_grading_mutable_workspaces(self):
        with tempfile.TemporaryDirectory() as tmp, patch.dict(os.environ, {'EXPLICIT_EVAL_KEY': KEY}):
            args = self.make_args(tmp)
            with patch.object(runner.subprocess, 'check_output', side_effect=self.preflight), \
                 patch.object(runner, 'docker_attempt', side_effect=runner.CleanupUnconfirmed), \
                 patch.object(runner.cohort_runner, 'grade') as grade:
                result = runner.run_evaluation(args, gateway_factory=FakeGateway)
            grade.assert_not_called()
            self.assertFalse(result['cleanup_confirmed'])
            self.assertEqual(result['tasks'], 5)
            self.assertEqual(result['passed'], 0)
            self.assertEqual(len(FakeGateway.instances), 1)
            self.assertEqual(result['results'][1]['status'], 'not_attempted_cleanup_unconfirmed')

    def test_grader_infrastructure_failure_still_publishes_complete_failure_report(self):
        with tempfile.TemporaryDirectory() as tmp, patch.dict(os.environ, {'EXPLICIT_EVAL_KEY': KEY}):
            args = self.make_args(tmp)
            with patch.object(runner.subprocess, 'check_output', side_effect=self.preflight), \
                 patch.object(runner, 'docker_attempt', return_value=(0, b'', False)), \
                 patch.object(runner.cohort_runner, 'grade', side_effect=OSError):
                result = runner.run_evaluation(args, gateway_factory=FakeGateway)
            self.assertEqual((result['tasks'], result['passed']), (5, 0))
            self.assertTrue(all(t['grading']['status'] == 'grading_unavailable' for t in result['results']))

    def test_production_factory_cannot_turn_injected_transport_into_model_quality(self):
        class InjectedGateway(FakeGateway):
            def report(self):
                return {**super().report(), 'transport_mode': 'injected'}

        with tempfile.TemporaryDirectory() as tmp, patch.dict(os.environ, {'EXPLICIT_EVAL_KEY': KEY}):
            args = self.make_args(tmp)
            with patch.object(runner.subprocess, 'check_output', side_effect=self.preflight), \
                 patch.object(runner, 'docker_attempt', return_value=(0, b'', False)), \
                 patch.object(runner.cohort_runner, 'grade', side_effect=self.grade), \
                 patch.object(runner.gate, 'BudgetGateway', InjectedGateway):
                result = runner.run_evaluation(args)
            self.assertFalse(result['model_quality_evidence'])

    def test_unavailable_gateway_accounting_reserves_remaining_cap_and_stops(self):
        class BrokenAccounting(FakeGateway):
            def report(self):
                raise ValueError('unknown')

        with tempfile.TemporaryDirectory() as tmp, patch.dict(os.environ, {'EXPLICIT_EVAL_KEY': KEY}):
            args = self.make_args(tmp)
            with patch.object(runner.subprocess, 'check_output', side_effect=self.preflight), \
                 patch.object(runner, 'docker_attempt', return_value=(0, b'', False)), \
                 patch.object(runner.cohort_runner, 'grade', side_effect=self.grade):
                result = runner.run_evaluation(args, gateway_factory=BrokenAccounting)
            self.assertFalse(result['reservation_integrity'])
            self.assertEqual(result['reserved_cost_usd'], '1')
            self.assertEqual(result['results'][1]['status'], 'not_attempted_budget_integrity')

    def test_docker_drain_cap_timeout_environment_and_cleanup_order(self):
        process = Mock()
        process.stdout = io.BytesIO(b'x' * (runner.MAX_LOG + 1024))
        process.wait.side_effect = [subprocess.TimeoutExpired('docker', 1), 137]
        process.poll.return_value = None
        with patch.dict(os.environ, {'EXPLICIT_EVAL_KEY': KEY, 'OPENAI_API_KEY': KEY}), \
             patch.object(runner.subprocess, 'Popen', return_value=process) as start, \
             patch.object(runner.subprocess, 'run') as cleanup:
            status, output, truncated = runner.docker_attempt(['docker', 'run'], 'attempt', 1)
        self.assertEqual(status, 124)
        self.assertLessEqual(len(output), runner.MAX_LOG + 4097)
        self.assertGreaterEqual(len(output), runner.MAX_LOG)
        self.assertTrue(truncated)
        self.assertNotIn('OPENAI_API_KEY', start.call_args.kwargs['env'])
        self.assertNotIn('EXPLICIT_EVAL_KEY', start.call_args.kwargs['env'])
        self.assertEqual(cleanup.call_args.args[0], ['docker', 'rm', '-f', 'attempt'])
        self.assertTrue(cleanup.call_args.kwargs['check'])
        process.kill.assert_called_once()

    def test_large_agent_log_keeps_trailing_tool_metrics(self):
        process = Mock()
        marker = 'MELDRA_EVAL_random:'
        process.stdout = io.BytesIO(b'x' * (runner.MAX_LOG + 2048) + b'\n' +
                                    marker.encode() + b'{"tool_calls":1,"tool_statuses":{"succeeded":1}}\n')
        process.wait.return_value = 0
        process.poll.return_value = 0
        with patch.object(runner.subprocess, 'Popen', return_value=process), \
             patch.object(runner.subprocess, 'run'):
            _, output, truncated = runner.docker_attempt(['docker', 'run'], 'attempt', 1)
        log, metrics = runner.parse_metrics(output.decode(), marker)
        self.assertTrue(truncated)
        self.assertEqual(metrics['tool_calls'], 1)
        self.assertNotIn(marker, log)

    def test_docker_failed_removal_is_fatal_but_still_kills_client(self):
        process = Mock()
        process.stdout = io.BytesIO(b'')
        process.wait.return_value = 0
        process.poll.return_value = None
        for error in (subprocess.TimeoutExpired('docker', 15), subprocess.CalledProcessError(1, 'docker')):
            with self.subTest(error=error), \
                 patch.object(runner.subprocess, 'Popen', return_value=process), \
                 patch.object(runner.subprocess, 'run', side_effect=error):
                process.stdout = io.BytesIO(b'')
                with self.assertRaises(runner.CleanupUnconfirmed):
                    runner.docker_attempt(['docker', 'run'], 'attempt', 1)
        self.assertEqual(process.kill.call_count, 2)

    def test_docker_interruption_only_reports_clean_after_removal(self):
        process = Mock()
        process.stdout = io.BytesIO(b'partial output')
        process.wait.side_effect = [KeyboardInterrupt, 137]
        process.poll.return_value = None
        with patch.object(runner.subprocess, 'Popen', return_value=process), \
             patch.object(runner.subprocess, 'run') as cleanup:
            with self.assertRaises(runner.DockerInterrupted):
                runner.docker_attempt(['docker', 'run'], 'attempt', 1)
        self.assertTrue(cleanup.call_args.kwargs['check'])
        process.kill.assert_called_once()

    def test_term_and_terminal_hangup_install_and_restore_cleanup_signals(self):
        argv = ['execute.py', '--approved-plan', '/tmp/plan', '--api-key-env', 'KEY',
                '--binary', '/tmp/bin', '--image', 'image', '--run-dir', '/tmp/run', '--output', '/tmp/report']
        seen = []

        def register(number, handler):
            seen.append((number, handler))
            return runner.signal.SIG_DFL

        with patch.object(runner.sys, 'argv', argv), \
             patch.object(runner.signal, 'signal', side_effect=register), \
             patch.object(runner, 'run_evaluation', side_effect=ValueError), \
             patch.object(runner.sys, 'stderr', io.StringIO()):
            with self.assertRaises(SystemExit) as exit_info:
                runner.main()
        self.assertEqual(exit_info.exception.code, 2)
        expected = [runner.signal.SIGTERM]
        if hasattr(runner.signal, 'SIGHUP'):
            expected.append(runner.signal.SIGHUP)
        self.assertEqual([number for number, _ in seen], expected * 2)
        for _, handler in seen[:len(expected)]:
            with self.assertRaises(KeyboardInterrupt):
                handler()
        self.assertTrue(all(handler == runner.signal.SIG_DFL for _, handler in seen[len(expected):]))


if __name__ == '__main__':
    unittest.main()
