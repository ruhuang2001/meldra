#!/usr/bin/env python3
"""Run the fixed cohort in Docker through an explicitly priced budget gateway.

--execute-live is an execution opt-in, not a substitute for human approval of
the saved plan. Credentials are read only from the explicitly named variable.
There is no credential/config discovery and no host agent execution.
"""
from __future__ import annotations

import argparse
from datetime import datetime, timezone
from decimal import Decimal
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import signal
import subprocess
import sys
import threading
import time
import uuid

import budget
import gate
import run as cohort_runner

MAX_LOG = 64 * 1024


class CleanupUnconfirmed(RuntimeError):
    """Docker did not confirm removal; the workspace must not be graded."""


class DockerInterrupted(KeyboardInterrupt):
    """The caller interrupted, and Docker cleanup completed successfully."""

# This code is trusted runner code passed as a Python argument, never shell
# interpolation. Only the task workspace and pinned Linux binary are mounted.
# The loopback relay preserves Meldra's HTTP-only-on-loopback validation.
CONTAINER_SCRIPT = r'''
import http.client, http.server, json, os, pathlib, sqlite3, subprocess, sys, threading
port, nonce, model, prompt, marker = sys.argv[1:]
class Relay(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args): pass
    def do_POST(self):
        conn = None
        try:
            size = int(self.headers.get('Content-Length', '-1'))
            if self.path != '/v1/responses' or not 0 <= size <= 4 * 1024 * 1024:
                self.send_error(400); return
            conn = http.client.HTTPConnection('host.docker.internal', int(port), timeout=125)
            conn.request('POST', self.path, body=self.rfile.read(size), headers={
                'Authorization': 'Bearer ' + nonce, 'Content-Type': 'application/json'})
            response = conn.getresponse()
            body = response.read(8 * 1024 * 1024 + 1)
            if len(body) > 8 * 1024 * 1024: raise ValueError('response too large')
            self.send_response(response.status)
            self.send_header('Content-Type', response.getheader('Content-Type', 'application/json'))
            self.send_header('Content-Length', str(len(body))); self.end_headers()
            self.wfile.write(body)
        except (OSError, ValueError, http.client.HTTPException):
            try: self.send_error(502, 'budget gateway unavailable')
            except OSError: pass
        finally:
            if conn is not None: conn.close()
server = http.server.HTTPServer(('127.0.0.1', 0), Relay)
threading.Thread(target=server.serve_forever, daemon=True).start()
home = pathlib.Path('/tmp/meldra'); home.mkdir(mode=0o700)
config = home / 'config.toml'
config.write_text('allow_insecure_base_url = true\n')
config.chmod(0o600)
environment = dict(os.environ, HOME='/tmp/home', MELDRA_HOME=str(home),
    OPENAI_API_KEY=nonce, OPENAI_MODEL=model,
    OPENAI_BASE_URL='http://127.0.0.1:%d/v1' % server.server_address[1])
status = subprocess.run(['/opt/meldra', '--auto-approve', '--workspace', '/workspace', '--prompt', prompt],
    stdin=subprocess.DEVNULL, env=environment).returncode
# Agent-owned records are useful diagnostics, not an independent success oracle.
metrics = {'tool_calls': None, 'tool_statuses': None}
try:
    db = sqlite3.connect('file:/tmp/meldra/tasks/tasks.db?mode=ro', uri=True, timeout=1)
    metrics['tool_calls'] = db.execute('SELECT COUNT(*) FROM tool_calls').fetchone()[0]
    metrics['tool_statuses'] = dict(db.execute('SELECT status, COUNT(*) FROM tool_calls GROUP BY status'))
    db.close()
except sqlite3.Error: pass
print('\n' + marker + json.dumps(metrics), flush=True)
server.shutdown()
sys.exit(status if status >= 0 else 128 - status)
'''


def private_json(path: Path, value: dict, *, replace: bool = False) -> None:
    """Atomic private JSON; never replace a user's report by default."""
    if not replace and path.exists():
        raise FileExistsError('output exists; choose a fresh path')
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(path.name + '.' + secrets.token_hex(8) + '.tmp')
    try:
        with temporary.open('x', encoding='utf-8') as stream:
            os.chmod(temporary, 0o600)
            json.dump(value, stream, indent=2)
            stream.write('\n')
        if replace:
            os.replace(temporary, path)
        else:
            # link() makes exclusive publication atomic against concurrent runs.
            os.link(temporary, path)
    finally:
        temporary.unlink(missing_ok=True)


@budget.exact
def load_plan(path: Path) -> tuple[dict, str]:
    raw = path.read_bytes()
    if len(raw) > MAX_LOG:
        raise ValueError('plan is oversized')
    plan = json.loads(raw)
    if not isinstance(plan, dict) or plan.get('schema_version') != 2:
        raise ValueError('use a schema version 2 plan with explicit token-counting fees')
    fields = ('model', 'provider', 'input_price_per_million', 'output_price_per_million',
              'total_tokens_per_task', 'max_output_tokens_per_response', 'max_cost_usd',
              'count_price_per_request', 'max_requests')
    if any(name not in plan for name in fields):
        raise ValueError('saved plan is missing budget fields')
    try:
        validated = cohort_runner.budget_plan(*(plan[name] for name in fields))
    except (TypeError, AttributeError) as exc:
        raise ValueError('saved plan has invalid field types') from exc
    for name in (*fields, 'suite', 'cohort_sha256', 'tasks', 'attempts_per_task',
                 'conditional_cost_upper_bound_usd'):
        if plan.get(name) != validated[name]:
            raise ValueError('saved plan differs from the current cohort or validated budget')
    # Construction also enforces gateway-specific caps and provider semantics.
    config_for_plan(plan, plan['max_cost_usd'])
    return validated, hashlib.sha256(raw).hexdigest()


def config_for_plan(plan: dict, remaining: str) -> gate.Config:
    return gate.Config(model=plan['model'], provider_url=plan['provider'],
                       input_price_per_million=plan['input_price_per_million'],
                       output_price_per_million=plan['output_price_per_million'],
                       count_price_per_request=plan['count_price_per_request'],
                       total_tokens=plan['total_tokens_per_task'],
                       max_output_tokens=plan['max_output_tokens_per_response'],
                       max_cost_usd=remaining, max_requests=plan['max_requests'],
                       timeout_seconds=60)


def binary_metadata(binary: Path) -> dict:
    if binary.is_symlink() or not binary.is_file() or binary.stat().st_size > 128 * 1024 * 1024:
        raise ValueError('provide a regular Linux Meldra binary no larger than 128 MiB')
    with binary.open('rb') as stream:
        if stream.read(4) != b'\x7fELF':
            raise ValueError('agent binary must be a Linux ELF executable')
    info = subprocess.check_output(['go', 'version', '-m', str(binary)], text=True, timeout=30)
    build = {}
    for line in info.splitlines():
        if match := re.match(r'\s*build\s+(\S+)=(.*)$', line):
            build[match[1]] = match[2]
    if build.get('GOOS') != 'linux':
        raise ValueError('agent Go build metadata must specify GOOS=linux')
    return {'binary_sha256': hashlib.sha256(binary.read_bytes()).hexdigest(),
            'binary_revision': build.get('vcs.revision'),
            'binary_modified': build.get('vcs.modified'),
            'binary_goarch': build.get('GOARCH'), 'binary_goos': 'linux'}


def agent_argv(image_id: str, workspace: Path, binary: Path, name: str,
               port: int, nonce: str, model: str, prompt: str, marker: str) -> list[str]:
    for path in (workspace.resolve(), binary.resolve()):
        if ',' in str(path) or '\n' in str(path):
            raise ValueError('Docker mount paths cannot contain commas or newlines')
    uid = os.getuid() or 65534
    gid = os.getgid() or 65534
    return ['docker', 'run', '--name', name, '--network', 'bridge',
            '--add-host', 'host.docker.internal:host-gateway', '--read-only',
            '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges',
            '--pids-limit', '128', '--memory', '1g', '--cpus', '1',
            '--user', f'{uid}:{gid}', '--tmpfs', '/tmp:rw,exec,nosuid,size=256m,mode=1777',
            '--workdir', '/workspace', '--env', 'GOTOOLCHAIN=local', '--env', 'GOPROXY=off',
            '--env', 'GOCACHE=/tmp/go-cache', '--env', 'PYTHONDONTWRITEBYTECODE=1',
            '--mount', f'type=bind,source={workspace.resolve()},target=/workspace',
            '--mount', f'type=bind,source={binary.resolve()},target=/opt/meldra,readonly',
            image_id, 'python3', '-c', CONTAINER_SCRIPT, str(port), nonce, model, prompt, marker]


def safe_text(value: str, credentials: tuple[str, ...]) -> str:
    for secret in credentials:
        if secret:
            value = value.replace(secret, '[REDACTED]')
    # Strip terminal controls so even explicitly opened logs cannot issue escapes.
    return ''.join(c for c in value if c in '\n\r\t' or ord(c) >= 32 and ord(c) != 127)


def docker_attempt(argv: list[str], name: str, timeout: int) -> tuple[int, bytes, bool]:
    """Drain continuously, retain at most 64 KiB, always remove the container."""
    process = None
    output = bytearray()
    tail = bytearray()
    truncated = False
    reader = None
    interrupted = False
    # Docker connection settings are needed on Desktop/OrbStack and remote
    # daemons. Model credentials and unrelated caller environment are omitted.
    environment = {key: value for key, value in os.environ.items()
                   if key in {'PATH', 'HOME', 'DOCKER_HOST', 'DOCKER_CONTEXT', 'DOCKER_CONFIG',
                              'DOCKER_TLS_VERIFY', 'DOCKER_CERT_PATH', 'SSH_AUTH_SOCK', 'SYSTEMROOT'}}
    try:
        process = subprocess.Popen(argv, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                                   stderr=subprocess.STDOUT, env=environment)

        def consume():
            nonlocal truncated
            while chunk := process.stdout.read(8192):
                available = max(0, MAX_LOG - len(output))
                output.extend(chunk[:available])
                truncated |= len(chunk) > available
                tail.extend(chunk)
                del tail[:-4096]

        reader = threading.Thread(target=consume, daemon=True)
        reader.start()
        try:
            status = process.wait(timeout=timeout)
        except subprocess.TimeoutExpired:
            status = 124
        except KeyboardInterrupt:
            interrupted = True
    finally:
        # rm -f is essential: terminating the client alone leaves agent code live.
        try:
            if process is not None:
                try:
                    subprocess.run(['docker', 'rm', '-f', name], stdout=subprocess.DEVNULL,
                                   stderr=subprocess.DEVNULL, timeout=15, check=True, env=environment)
                except (OSError, subprocess.SubprocessError) as exc:
                    raise CleanupUnconfirmed('Docker removal was not confirmed') from exc
        finally:
            if process is not None:
                if process.poll() is None:
                    process.kill()
                    process.wait(timeout=5)
                if reader is not None:
                    reader.join(timeout=5)
                process.stdout.close()
    if interrupted:
        raise DockerInterrupted()
    # Preserve the wrapper's final diagnostic marker even when earlier model
    # output exhausts the log cap. Published agent.log remains <=64 KiB.
    return status, bytes(output) + (b'\n' + bytes(tail) if truncated else b''), truncated


def parse_metrics(output: str, marker: str) -> tuple[str, dict]:
    metrics = {'tool_calls': None, 'tool_statuses': None}
    lines = []
    for line in output.splitlines(keepends=True):
        if line.startswith(marker):
            try:
                candidate = json.loads(line[len(marker):])
                count = candidate.get('tool_calls')
                statuses = candidate.get('tool_statuses')
                if (type(count) is int and count >= 0 and isinstance(statuses, dict)
                        and all(type(v) is int and v >= 0 for v in statuses.values())
                        and sum(statuses.values()) == count):
                    metrics = {'tool_calls': count, 'tool_statuses': statuses}
            except (ValueError, AttributeError):
                pass
        else:
            lines.append(line)
    return ''.join(lines), metrics


@budget.exact
def run_evaluation(args, *, gateway_factory=None) -> dict:
    if not args.execute_live:
        raise ValueError('live execution requires --execute-live and external approval of the saved plan')
    if not 1 <= args.timeout <= 3600:
        raise ValueError('per-task timeout must be between 1 and 3600 seconds')
    if not re.fullmatch(r'[A-Za-z_][A-Za-z0-9_]*', args.api_key_env):
        raise ValueError('name the explicit upstream API key environment variable')
    api_key = os.environ.get(args.api_key_env, '')
    if not api_key:
        raise ValueError('the explicitly named API key variable is empty')
    if not 16 <= len(api_key) <= 4096 or any(ord(c) < 33 or ord(c) > 126 for c in api_key):
        raise ValueError('the upstream key must be printable and at least 16 characters')
    plan, plan_hash = load_plan(args.approved_plan)
    if args.run_dir.exists() or args.output.exists():
        raise ValueError('run directory/report exists; each cohort permits one fresh attempt only')
    if args.output.resolve().is_relative_to(args.run_dir.resolve()):
        raise ValueError('keep final report outside task run directory')
    binary = args.binary.resolve()
    metadata = binary_metadata(binary)
    image_id = subprocess.check_output(['docker', 'image', 'inspect', '--format', '{{.Id}}', args.image],
                                       text=True, timeout=30).strip()
    if not re.fullmatch(r'sha256:[a-f0-9]{64}', image_id):
        raise ValueError('Docker image inspection did not return an immutable image ID')
    manifest = cohort_runner.prepare(args.run_dir)
    snapshot = args.run_dir / 'agent-binary'
    shutil.copyfile(binary, snapshot)
    snapshot.chmod(0o555)
    if hashlib.sha256(snapshot.read_bytes()).hexdigest() != metadata['binary_sha256']:
        raise ValueError('binary changed while preparing the run')
    if os.getuid() == 0:
        for task in cohort_runner.cohort()['tasks']:
            workspace = args.run_dir / task['id'] / 'workspace'
            for path in [workspace, *workspace.rglob('*')]:
                os.chown(path, 65534, 65534)
    injected = gateway_factory is not None
    factory = gateway_factory or gate.BudgetGateway
    manifest.update(metadata, schema_version=2, kind='live_execution', live_model=not injected,
                    model=plan['model'], provider=plan['provider'], approved_plan_sha256=plan_hash,
                    image_id=image_id, budget_plan=plan,
                    network_isolation='Docker bridge; no domain egress allowlist',
                    credential_isolation='upstream key host-only; per-attempt bounded gateway nonce',
                    live_started_at=datetime.now(timezone.utc).isoformat())
    for task in manifest['tasks']:
        task.update(attempt_id=uuid.uuid4().hex, status='not_started', duration_seconds=0,
                    exit_code=None, gateway=None, tool_calls=None, tool_statuses=None,
                    tool_metrics_source='agent-owned SQLite diagnostic; not grading evidence',
                    reported_input_tokens=None, reported_output_tokens=None,
                    reported_cost_usd=None, reserved_cost_usd='0')
    manifest_path = args.run_dir / 'manifest.json'
    private_json(manifest_path, manifest, replace=True)
    reserved = Decimal(0)
    interrupted = False
    fatal_reservation_error = False
    cleanup_unconfirmed = False
    for task, attempt in zip(cohort_runner.cohort()['tasks'], manifest['tasks']):
        if interrupted or fatal_reservation_error or cleanup_unconfirmed or reserved >= Decimal(plan['max_cost_usd']):
            attempt['status'] = ('not_attempted_interrupted' if interrupted else
                                 'not_attempted_cleanup_unconfirmed' if cleanup_unconfirmed else
                                 'not_attempted_budget_integrity' if fatal_reservation_error else
                                 'not_attempted_budget_exhausted')
            private_json(manifest_path, manifest, replace=True)
            continue
        nonce = secrets.token_urlsafe(48)
        marker = 'MELDRA_EVAL_' + secrets.token_hex(16) + ':'
        gateway = None
        started = time.monotonic()
        attempt['status'] = 'running'
        private_json(manifest_path, manifest, replace=True)
        output, truncated, status = b'', False, None
        try:
            config = config_for_plan(plan, str(Decimal(plan['max_cost_usd']) - reserved))
            gateway = factory(config, upstream_key=api_key, bearer_nonce=nonce, bind_host='0.0.0.0')
            with gateway:
                name = 'meldra-live-' + attempt['attempt_id']
                attempt['container_name'] = name
                attempt['cleanup_confirmed'] = False
                private_json(manifest_path, manifest, replace=True)
                command = agent_argv(image_id, args.run_dir / task['id'] / 'workspace', snapshot,
                                     name, gateway.address[1], nonce, plan['model'],
                                     (args.run_dir / task['id'] / 'prompt.txt').read_text(), marker)
                status, output, truncated = docker_attempt(command, name, args.timeout)
                attempt['cleanup_confirmed'] = True
            attempt['status'] = 'completed' if status == 0 else 'timed_out' if status == 124 else 'failed'
        except CleanupUnconfirmed:
            cleanup_unconfirmed = True
            attempt['status'] = 'cleanup_unconfirmed'
        except DockerInterrupted:
            interrupted = True
            attempt['status'] = 'interrupted'
            attempt['cleanup_confirmed'] = True
        except KeyboardInterrupt:
            interrupted = True
            attempt['status'] = 'interrupted'
        except (OSError, ValueError, RuntimeError, subprocess.SubprocessError):
            # Exception messages may contain request bodies or credentials. The
            # bounded log and key-free gateway status supply operational detail.
            attempt['status'] = 'runner_error'
        finally:
            attempt['duration_seconds'] = time.monotonic() - started
            attempt['exit_code'] = status
            if gateway is not None:
                try:
                    usage = gateway.report()
                    charge = Decimal(usage['reserved_cost_usd'])
                    if not charge.is_finite() or charge < 0:
                        raise ValueError('invalid reservation')
                    attempt['gateway'] = usage
                    attempt['reported_input_tokens'] = usage['reported_input_tokens']
                    attempt['reported_output_tokens'] = usage['reported_output_tokens']
                    fatal_reservation_error |= not usage['reservation_integrity']
                except (ValueError, KeyError, TypeError, RuntimeError):
                    # Unknown accounting consumes the entire remaining allowance;
                    # subsequent attempts cannot obtain fresh charge capacity.
                    charge = Decimal(plan['max_cost_usd']) - reserved
                    fatal_reservation_error = True
                    attempt['reservation_estimate'] = 'remaining allowance; gateway accounting unavailable'
                reserved += charge
                attempt['reserved_cost_usd'] = str(charge)
                fatal_reservation_error |= reserved > Decimal(plan['max_cost_usd'])
            log, metrics = parse_metrics(output.decode('utf-8', errors='replace'), marker)
            attempt.update(metrics)
            log_bytes = safe_text(log, (api_key, nonce)).encode('utf-8')[:MAX_LOG]
            log_path = args.run_dir / task['id'] / 'agent.log'
            with log_path.open('xb') as stream:
                os.chmod(log_path, 0o600)
                stream.write(log_bytes)
            attempt['output_truncated'] = truncated or len(log_bytes) >= MAX_LOG
            attempt['log'] = str(log_path.relative_to(args.run_dir))
            private_json(manifest_path, manifest, replace=True)
    # Every attempt, including failures and not-started tasks, is durable before
    # any grader runs. No hidden tests ever enter the agent containers.
    try:
        if cleanup_unconfirmed:
            raise ValueError('cannot grade workspaces with unconfirmed execution cleanup')
        grades = cohort_runner.grade(args.run_dir, image_id, args.run_dir / 'grades.json')
    except (OSError, ValueError, subprocess.SubprocessError, KeyboardInterrupt):
        # A Docker/grader infrastructure failure is not a missing denominator.
        grades = {'results': [{'id': entry['id'], 'passed': False, 'exit_code': None,
                              'status': 'grading_unavailable'} for entry in manifest['tasks']]}
    for attempt, result in zip(manifest['tasks'], grades['results']):
        attempt['grading'] = result
        attempt['passed'] = result['passed'] and attempt['status'] == 'completed'
    observed = [entry['gateway'] for entry in manifest['tasks']]
    has_response = any(item and any(record.get('status') in {'completed', 'incomplete', 'failed', 'cancelled'}
                                    for record in item['requests_detail']) for item in observed)
    all_https = all(item.get('transport_mode') == 'https' for item in observed if item)
    report = {**manifest, 'kind': 'live_quality' if not injected else 'offline_execution_test',
              'model_quality_evidence': not injected and all_https and has_response and not fatal_reservation_error,
              'all_tasks_attempted': all(entry['gateway'] is not None for entry in manifest['tasks']),
              'tasks': len(manifest['tasks']), 'passed': sum(t['passed'] for t in manifest['tasks']),
              'results': manifest['tasks'], 'reserved_cost_usd': str(reserved),
              'reported_cost_usd': None, 'interrupted': interrupted,
              'cleanup_confirmed': not cleanup_unconfirmed,
              'reservation_integrity': not fatal_reservation_error,
              'finished_at': datetime.now(timezone.utc).isoformat()}
    private_json(args.output, report)
    return report


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--execute-live', action='store_true')
    parser.add_argument('--approved-plan', type=Path, required=True)
    parser.add_argument('--api-key-env', required=True)
    parser.add_argument('--binary', type=Path, required=True)
    parser.add_argument('--image', required=True)
    parser.add_argument('--run-dir', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--timeout', type=int, default=600)
    args = parser.parse_args()
    def stop(*_):
        raise KeyboardInterrupt()

    stop_signals = [signal.SIGTERM]
    if hasattr(signal, 'SIGHUP'):
        stop_signals.append(signal.SIGHUP)
    old_handlers = {number: signal.signal(number, stop) for number in stop_signals}
    try:
        report = run_evaluation(args)
        print(json.dumps({'report': str(args.output), 'tasks': report['tasks'], 'passed': report['passed'],
                          'reserved_cost_usd': report['reserved_cost_usd'],
                          'model_quality_evidence': report['model_quality_evidence']}, indent=2))
        return int(report['passed'] != report['tasks'] or not report['model_quality_evidence'])
    except (ValueError, OSError, subprocess.SubprocessError):
        parser.exit(2, 'error: evaluation setup failed; verify plan, explicit key, Linux binary and Docker image\n')
    except KeyboardInterrupt:
        parser.exit(130, 'evaluation interrupted; saved attempts remain in the run manifest\n')
    finally:
        for number, handler in old_handlers.items():
            signal.signal(number, handler)


if __name__ == '__main__':
    sys.exit(main())
