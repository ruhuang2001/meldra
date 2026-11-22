#!/usr/bin/env python3
"""Test verification scripts without running the Go suites."""
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest

if not __debug__:
    raise SystemExit('Verification requires assertions; unset PYTHONOPTIMIZE and do not use -O.')

ROOT = Path(__file__).resolve().parents[1]


class VerificationScriptsTest(unittest.TestCase):
    def test_verification_scripts_run_outside_release_source(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve()
            verification = root / 'verification'
            verification.mkdir()
            shutil.copy2(ROOT / 'Makefile', verification / 'Makefile')
            shutil.copytree(ROOT / 'scripts', verification / 'scripts', ignore=shutil.ignore_patterns('__pycache__'))
            source = root / 'old-tag'
            source.mkdir()
            result = subprocess.run([sys.executable, '-B', str(verification / 'scripts/check-scripts-test.py'),
                                     'VerificationScriptsTest.test_optimized_verification_fails_closed'],
                                    cwd=source, capture_output=True, text=True, timeout=30)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            result = subprocess.run(['make', '-n', '-f', str(verification / 'Makefile'), 'check',
                                     'SCRIPT_TEST=' + str(verification / 'scripts/check-scripts-test.py'),
                                     'CHECK_SCRIPT=' + str(verification / 'scripts/mcp-check.py'),
                                     'VERSION=fixture'],
                                    cwd=source, capture_output=True, text=True, timeout=15)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            self.assertIn('python3 -B "' + str(verification / 'scripts/check-scripts-test.py') + '"', result.stdout)
            self.assertIn('--source-root "' + str(source), result.stdout)
            self.assertEqual(list(source.iterdir()), [])

    def test_optimized_verification_fails_closed(self):
        scripts = sorted((ROOT / 'scripts').glob('*e2e.py'))
        scripts += [ROOT / 'scripts/mcp-check.py']
        for script in scripts:
            for flags, optimize in [([], '1'), (['-O'], '')]:
                with self.subTest(script=script.name, flags=flags):
                    result = subprocess.run([sys.executable, '-B', *flags, str(script), '--help'],
                                            env=dict(os.environ, PYTHONOPTIMIZE=optimize),
                                            capture_output=True, text=True, timeout=15)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn('Verification requires assertions', result.stderr)
        result = subprocess.run([sys.executable, '-B', str(ROOT / 'scripts/mcp-check.py'), '--self-check'],
                                env=dict(os.environ, PYTHONOPTIMIZE=''), capture_output=True, text=True, timeout=15)
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_staged_failure_and_timeout_retain_evidence(self):
        for timeout in [False, True]:
            with self.subTest(timeout=timeout), tempfile.TemporaryDirectory() as directory:
                root = Path(directory).resolve()
                home = root / 'home'
                home.mkdir()
                env = dict(os.environ, HOME=str(home), GIT_CONFIG_NOSYSTEM='1',
                           MELDRA_LOCAL_CI_TIMEOUT='1' if timeout else '10')
                for key in ('MAKEFLAGS', 'MFLAGS', 'CHECK_DIR', 'COVERAGE_FILE'):
                    env.pop(key, None)
                subprocess.run(['git', 'init', '-q', str(root)], check=True, env=env)
                commands = ['mkdir -p "$(CHECK_DIR)"',
                            'pwd > "$(CHECK_DIR)/source.txt"',
                            'printf staged-evidence > "$(CHECK_DIR)/failure.log"',
                            'printf coverage-evidence > "$(COVERAGE_FILE)"',
                            'sleep 30' if timeout else 'false']
                lines = ['CHECK_DIR := .discarded', 'COVERAGE_FILE := discarded.out', 'check:']
                lines += [chr(9) + command for command in commands]
                (root / 'Makefile').write_text(chr(10).join(lines) + chr(10))
                subprocess.run(['git', '-C', str(root), 'add', 'Makefile'], check=True, env=env)
                (root / 'Makefile').write_text('check:' + chr(10) + chr(9) + 'false' + chr(10))
                result = subprocess.run([sys.executable, '-B', str(ROOT / 'scripts/check-local-ci.py'), '--staged'],
                                        cwd=root, env=env, capture_output=True, text=True, timeout=20)
                self.assertEqual(result.returncode, 124 if timeout else 2, result.stdout + result.stderr)
                evidence, = (root / '.artifacts/checks').glob('local-ci-*')
                self.assertEqual((evidence / 'failure.log').read_text(), 'staged-evidence')
                self.assertEqual((evidence / 'coverage.out').read_text(), 'coverage-evidence')
                self.assertFalse(Path((evidence / 'source.txt').read_text().strip()).exists())
                self.assertIn(str(evidence), result.stdout)


if __name__ == '__main__':
    unittest.main()
