#!/usr/bin/env python3
"""Offline CLI regression scenarios for the SWE-bench adapter.

Failure scenarios recorded before implementation:
1. CLI generation drops --auto-approve or discards a successful empty patch.
2. A transient checkout error becomes a prediction, lacks attempt evidence, or
   cannot be retried with the same run ID.
3. A failed agent's partial diff becomes a completed prediction or blocks retry.
4. A corrupt JSONL tail hides a valid prefix, breaks appended records, or an
   otherwise valid final record without a newline merges with the next record.
5. Timeout kills the agent before SIGTERM cleanup can stop a separately grouped
   child, loses output, or records a timeout as a completed prediction.
6. A failed batch still invokes local evaluation or hosted submission.
7. Nonpositive/nonfinite timeout and termination-grace inputs reach execution.
8. An agent that ignores SIGTERM leaves its separately grouped child running
   after the grace period instead of escalating and retaining timeout evidence.
9. A truncated attempts journal tail or missing final newline corrupts the next
   outcome, drops a valid prior attempt, or loses the exact damaged evidence.
10. A legacy failed prediction is mistaken for completion instead of migrating
    its error and partial patch into attempts and retrying the instance.

Run with a fresh --output directory; fixtures, CLI logs, and report.json remain
available for inspection and reproduction. No network, models, or unit tests.
"""

import argparse
import json
import os
from pathlib import Path
import shlex
import signal
import subprocess
import sys
import time
import traceback

ROOT = Path(__file__).resolve().parents[1]
PATCH = 'diff --git a/example.txt b/example.txt\n+fixture change\n'

DATASETS = '''
import json
import os
from pathlib import Path

class Dataset(list):
    def select(self, indices):
        return Dataset(self[index] for index in indices)

def load_dataset(name, split):
    return Dataset(json.loads(Path(os.environ['ADAPTER_FIXTURE_TASKS']).read_text()))
'''

FIXTURE = r'''
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import time

state = Path(os.environ['ADAPTER_FIXTURE_STATE'])
role, args = sys.argv[1], sys.argv[2:]

def event(kind, **fields):
    record = {'kind': kind, **fields}
    descriptor = os.open(state / 'events.jsonl', os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o600)
    try:
        os.write(descriptor, (json.dumps(record) + '\n').encode())
    finally:
        os.close(descriptor)

def task_id(workspace):
    return (workspace / '.fixture-task').read_text()

def behavior(instance):
    return json.loads((state / 'behaviors.json').read_text())[instance]

def first_failure(instance, label):
    marker = state / (instance + '.' + label)
    if marker.exists():
        return False
    marker.touch()
    return True

if role == 'git':
    workspace = Path.cwd()
    if args[0] == 'clone':
        workspace = Path(args[-1])
        workspace.mkdir(parents=True, exist_ok=True)
        instance = args[-2].rsplit('/', 1)[-1].removesuffix('.git')
        (workspace / '.fixture-task').write_text(instance)
        event('clone', instance_id=instance, args=args)
    else:
        instance = task_id(workspace)
        event('git', instance_id=instance, args=args)
        mode = behavior(instance)
        if args[0] == 'checkout' and mode == 'checkout_once' and first_failure(instance, 'checkout-failed'):
            print('fixture transient checkout failure', file=sys.stderr)
            sys.exit(17)
        if args[0] == 'diff':
            patch = workspace / '.fixture-patch'
            sys.stdout.write(patch.read_text() if patch.exists() else '')
elif role == 'meldra':
    workspace = Path(args[args.index('--workspace') + 1])
    instance = task_id(workspace)
    mode = behavior(instance)
    event('agent', instance_id=instance, args=args, pid=os.getpid(), pgid=os.getpgrp())
    assert '--auto-approve' in args, 'automatic approval missing'
    assert args[args.index('--mode') + 1] == 'build'
    assert args[args.index('--permissions') + 1] == 'workspace-edit'
    assert sys.stdin.read() == '', 'adapter stdin must be closed'
    print('fixture stdout before completion', flush=True)
    print('fixture stderr before completion', file=sys.stderr, flush=True)
    if mode in ('timeout', 'stubborn'):
        child = subprocess.Popen([sys.executable, '-c', 'import time; time.sleep(120)'],
                                 start_new_session=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        (state / (instance + '.child.json')).write_text(json.dumps({'pid': child.pid, 'pgid': child.pid}))
        event('child', instance_id=instance, pid=child.pid, pgid=child.pid, parent_pgid=os.getpgrp())
        def stop(_signum, _frame):
            event('sigterm', instance_id=instance)
            if mode == 'stubborn':
                print('fixture refuses graceful cleanup', flush=True)
                return
            os.killpg(child.pid, signal.SIGTERM)
            child.wait(timeout=3)
            time.sleep(0.15)
            print('fixture graceful cleanup complete', flush=True)
            print('fixture cleanup stderr', file=sys.stderr, flush=True)
            (state / (instance + '.cleaned')).write_text('child reaped\n')
            sys.exit(0)
        signal.signal(signal.SIGTERM, stop)
        print('fixture timeout child ready', flush=True)
        while True:
            time.sleep(1)
    if mode != 'empty':
        (workspace / '.fixture-patch').write_text('diff --git a/example.txt b/example.txt\n+fixture change\n')
    if mode == 'agent_once' and first_failure(instance, 'agent-failed'):
        print('fixture agent failed with partial patch', file=sys.stderr)
        sys.exit(23)
    if mode == 'agent_always':
        sys.exit(24)
elif role in ('submit', 'evaluate'):
    event(role, args=args)
else:
    raise AssertionError('unexpected fixture role: ' + role)
'''


def write(path, content):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(content)


def dump(path, value):
    write(path, json.dumps(value, indent=2) + '\n')


def read_jsonl(path):
    if not path.exists():
        return []
    data = path.read_bytes()
    assert not data or data.endswith(b'\n'), f'{path} lacks final newline'
    return [json.loads(line) for line in data.decode().splitlines() if line.strip()]


def alive(pid):
    try:
        os.kill(pid, 0)
        return True
    except ProcessLookupError:
        return False


class Case:
    def __init__(self, output, name, adapter, behaviors):
        self.name, self.adapter = name, adapter
        self.root = output / name
        self.root.mkdir()
        self.state = self.root / 'fixture-state'
        self.state.mkdir()
        self.bin = self.root / 'bin'
        self.modules = self.root / 'modules'
        self.home = self.root / 'home'
        self.home.mkdir()
        self.run_id = 'fixture-' + name
        self.run = self.root / 'benchmarks' / 'swe-bench' / self.run_id
        self.invocations = []
        dump(self.state / 'behaviors.json', behaviors)
        tasks = [{'instance_id': instance, 'repo': 'fixture/' + instance,
                  'base_commit': 'fixture-base', 'problem_statement': 'Fix ' + instance}
                 for instance in behaviors]
        dump(self.state / 'tasks.json', tasks)
        write(self.modules / 'datasets.py', DATASETS)
        write(self.root / 'fixture.py', FIXTURE)
        for executable, role in [('git', 'git'), ('meldra', 'meldra'), ('sb-cli', 'submit')]:
            path = self.bin / executable
            write(path, '#!' + sys.executable + '\nimport runpy, sys\nsys.argv = [' +
                  repr(str(self.root / 'fixture.py')) + ', ' + repr(role) +
                  '] + sys.argv[1:]\nrunpy.run_path(sys.argv[0], run_name="__main__")\n')
            path.chmod(0o700)
        for package in ['swebench', 'swebench/harness']:
            write(self.modules / package / '__init__.py', '')
        write(self.modules / 'swebench/harness/run_evaluation.py',
              'import runpy, sys\nsys.argv = [' + repr(str(self.root / 'fixture.py')) +
              ', "evaluate"] + sys.argv[1:]\nrunpy.run_path(sys.argv[0], run_name="__main__")\n')
        self.env = dict(os.environ, HOME=str(self.home), XDG_CONFIG_HOME=str(self.home / '.config'),
                        PATH=str(self.bin) + os.pathsep + os.environ.get('PATH', ''),
                        PYTHONPATH=str(self.modules), PYTHONDONTWRITEBYTECODE='1',
                        HF_DATASETS_OFFLINE='1', HF_HUB_OFFLINE='1',
                        ADAPTER_FIXTURE_STATE=str(self.state),
                        ADAPTER_FIXTURE_TASKS=str(self.state / 'tasks.json'))

    def invoke(self, *extra):
        command = [sys.executable, str(self.adapter), '--binary', str(self.bin / 'meldra'),
                   '--run-id', self.run_id, '--max-tasks', '100', '--agent-workers', '1', *extra]
        number = len(self.invocations) + 1
        record = {'command': command, 'cwd': str(self.root)}
        dump(self.root / f'invocation-{number}.json', record)
        started = time.monotonic()
        process = subprocess.run(command, cwd=self.root, env=self.env, stdin=subprocess.DEVNULL,
                                 capture_output=True, text=True, timeout=25)
        write(self.root / f'invocation-{number}.stdout.log', process.stdout)
        write(self.root / f'invocation-{number}.stderr.log', process.stderr)
        record.update(returncode=process.returncode, seconds=round(time.monotonic() - started, 3))
        self.invocations.append(record)
        dump(self.root / f'invocation-{number}.json', record)
        return process

    def events(self, kind=None):
        events = read_jsonl(self.state / 'events.jsonl')
        return [item for item in events if kind is None or item['kind'] == kind]

    def predictions(self):
        return read_jsonl(self.run / 'predictions.jsonl')

    def attempts(self):
        return read_jsonl(self.run / 'attempts.jsonl')

    def seed(self, content):
        write(self.run / 'predictions.jsonl', content)

    def cleanup(self):
        for path in self.state.glob('*.child.json'):
            child = json.loads(path.read_text())
            if alive(child['pid']):
                try:
                    os.killpg(child['pgid'], signal.SIGKILL)
                except ProcessLookupError:
                    pass


def prediction(instance):
    return {'instance_id': instance, 'model_name_or_path': 'meldra', 'model_patch': '',
            'agent_error': '', 'agent_seconds': 0}


def check_success(case):
    process = case.invoke()
    assert process.returncode == 0, process.stderr
    predictions = {item['instance_id']: item for item in case.predictions()}
    assert set(predictions) == {'empty', 'changed'}, predictions
    assert predictions['empty']['model_patch'] == '', predictions
    assert predictions['changed']['model_patch'] == PATCH, predictions
    agents = case.events('agent')
    assert len(agents) == 2 and all('--auto-approve' in event['args'] for event in agents), agents
    assert case.invoke().returncode == 0
    assert len(case.events('agent')) == 2, 'successful instances regenerated on resume'


def check_retry(case):
    assert case.invoke().returncode != 0, 'failed attempt exited successfully'
    assert case.predictions() == [], 'failed attempt submitted a prediction'
    attempts = case.attempts()
    assert attempts and attempts[-1]['instance_id'] == 'retry', attempts
    assert attempts[-1].get('agent_error'), 'failed attempt lacks an explicit error'
    mode = json.loads((case.state / 'behaviors.json').read_text())['retry']
    if mode == 'agent_once':
        assert attempts[-1]['model_patch'] == PATCH, 'failed partial patch evidence was lost'
    else:
        assert 'checkout' in attempts[-1]['agent_error'], 'checkout error was not identified'
    dump(case.root / 'attempts-after-failure.json', attempts)
    assert case.invoke().returncode == 0, 'same run ID did not retry successfully'
    predictions = case.predictions()
    assert len(predictions) == 1 and predictions[0]['instance_id'] == 'retry', predictions
    assert predictions[0]['model_patch'] == PATCH, predictions
    assert len(case.events('clone')) == 2, 'failed instance was not retried'
    assert len(case.attempts()) > len(attempts), 'retry did not append attempt evidence'


def check_recovery(case, broken_tail):
    prefix = json.dumps(prediction('retained'))
    case.seed(prefix + ('\n{"instance_id": "retry", "model_patch":' if broken_tail else ''))
    assert case.invoke().returncode == 0
    records = case.predictions()
    assert records[0] == prediction('retained'), 'valid prefix changed during recovery'
    assert [record['instance_id'] for record in records] == ['retained', 'retry'], records
    assert [event['instance_id'] for event in case.events('agent')] == ['retry']
    assert case.invoke().returncode == 0
    assert len(case.events('agent')) == 1, 'repaired predictions could not resume'


def check_timeout(case):
    # Allow interpreter startup under concurrent builds before testing shutdown.
    process = case.invoke('--agent-timeout', '5', '--termination-grace', '2')
    assert process.returncode != 0, 'timeout exited successfully'
    children = case.events('child')
    assert len(children) == 1, 'timeout fixture did not start its child'
    child = children[0]
    assert child['pgid'] != child['parent_pgid'], child
    assert (case.state / 'timeout.cleaned').exists(), 'SIGTERM cleanup did not finish'
    assert case.events('sigterm'), 'agent never received SIGTERM'
    assert not alive(child['pid']), 'child survived timeout cleanup'
    assert case.predictions() == [], 'timeout submitted a prediction'
    assert 'timeout' in json.dumps(case.attempts()).lower(), 'timeout evidence missing'
    logs = list((case.run / 'agent-logs').glob('*.log'))
    content = '\n'.join(path.read_text() for path in logs)
    for marker in ['fixture stdout before completion', 'fixture stderr before completion',
                   'fixture graceful cleanup complete', 'fixture cleanup stderr']:
        assert marker in content, f'agent log lost {marker!r}'


def check_attempt_recovery(case, broken_tail):
    retained = dict(prediction('previous'), agent_error='prior fixture failure')
    original = json.dumps(retained).encode()
    if broken_tail:
        original += b'\n{"instance_id": "interrupted", "agent_error": "\xff'
    path = case.run / 'attempts.jsonl'
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(original)
    assert case.invoke().returncode == 0, 'journal repair blocked successful generation'
    attempts = case.attempts()
    assert len(attempts) == 2 and attempts[0] == retained, 'valid journal prefix changed or lost'
    assert attempts[1]['instance_id'] == 'retry' and not attempts[1]['agent_error'], attempts
    assert attempts[1]['model_patch'] == PATCH, 'new outcome lost after journal recovery'
    backups = list(case.run.glob('attempts-recovery-*'))
    if broken_tail:
        assert any(backup.read_bytes() == original for backup in backups), 'exact prior journal was not preserved'
    assert [record['instance_id'] for record in case.predictions()] == ['retry']
    assert case.invoke().returncode == 0
    assert case.attempts() == attempts, 'resume duplicated a completed attempt'


def check_legacy_failure(case):
    failed = dict(prediction('retry'), model_patch=PATCH, agent_error='meldra exit 23')
    original = json.dumps(failed) + '\n'
    case.seed(original)
    assert case.invoke().returncode == 0, 'legacy failed prediction was not retried'
    records = case.predictions()
    assert len(records) == 1 and records[0]['instance_id'] == 'retry', records
    assert not records[0]['agent_error'] and records[0]['model_patch'] == PATCH, records
    assert len(case.events('agent')) == 1, 'legacy failed instance was treated as completed'
    attempts = case.attempts()
    assert len(attempts) == 2 and attempts[0] == failed, 'legacy failure evidence was lost'
    assert not attempts[1]['agent_error'], attempts
    assert any(path.read_text() == original for path in case.run.glob('predictions-recovery-*')), \
        'legacy predictions backup missing'


def check_escalation(case):
    process = case.invoke('--agent-timeout', '5', '--termination-grace', '0.4')
    assert process.returncode != 0, 'stubborn timeout exited successfully'
    children, parents = case.events('child'), case.events('agent')
    assert len(children) == len(parents) == 1, 'timeout fixtures did not start'
    assert children[0]['pgid'] != parents[0]['pgid'], 'child shares agent process group'
    assert case.events('sigterm'), 'agent never received graceful shutdown signal'
    assert not alive(parents[0]['pid']), 'stubborn parent survived escalation'
    child_pid = children[0]['pid']
    deadline = time.monotonic() + 3
    while alive(child_pid) and time.monotonic() < deadline:
        time.sleep(0.05)
    status = subprocess.run(['ps', '-p', str(child_pid), '-o', 'stat='],
                            capture_output=True, text=True, timeout=5)
    dump(case.root / 'child-after-escalation.json',
         {'pid': child_pid, 'status': status.stdout.strip(), 'returncode': status.returncode})
    # A dead orphan can briefly await OS reaping; it must not remain executable.
    assert not status.stdout.strip() or status.stdout.strip().startswith('Z'), 'child survived escalation'
    assert case.predictions() == [], 'stubborn timeout submitted a prediction'
    assert 'timeout' in json.dumps(case.attempts()).lower(), 'timeout evidence missing'
    content = '\n'.join(path.read_text() for path in (case.run / 'agent-logs').glob('*.log'))
    assert 'fixture refuses graceful cleanup' in content, 'escalation lost agent output'


def check_gate(case, option):
    assert case.invoke(option).returncode != 0, 'mixed batch failure exited successfully'
    assert [row['instance_id'] for row in case.predictions()] == ['empty']
    assert not case.events('evaluate') and not case.events('submit'), 'failed batch reached evaluation'
    assert any(item['instance_id'] == 'failed' for item in case.attempts()), 'failure evidence missing'


def check_arguments(case):
    for option in ['--agent-timeout', '--termination-grace']:
        for value in ['0', '-1', 'nan', 'inf']:
            result = case.invoke(option + '=' + value)
            assert result.returncode != 0, f'{option} accepted {value}'
    assert not case.events(), 'invalid arguments reached fixture execution'


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--adapter', type=Path, default=ROOT / 'scripts/swe-bench-lite.py')
    parser.add_argument('--output', type=Path, required=True, help='Fresh artifact directory')
    args = parser.parse_args()
    output, adapter = args.output.resolve(), args.adapter.resolve()
    if not adapter.is_file():
        parser.error(f'adapter not found: {adapter}')
    output.mkdir(parents=True, exist_ok=False)
    report = {'adapter': str(adapter), 'python': sys.executable, 'scenarios': [],
              'reproduce': shlex.join([sys.executable, str(Path(__file__).resolve()),
                                       '--adapter', str(adapter), '--output', str(output) + '-rerun'])}
    dump(output / 'report.json', report)
    scenarios = [
        ('successful-patches', {'empty': 'empty', 'changed': 'success'}, check_success),
        ('checkout-retry', {'retry': 'checkout_once'}, check_retry),
        ('partial-agent-retry', {'retry': 'agent_once'}, check_retry),
        ('corrupt-tail-recovery', {'retained': 'empty', 'retry': 'success'},
         lambda case: check_recovery(case, True)),
        ('missing-newline-recovery', {'retained': 'empty', 'retry': 'success'},
         lambda case: check_recovery(case, False)),
        ('attempts-corrupt-tail', {'retry': 'success'},
         lambda case: check_attempt_recovery(case, True)),
        ('attempts-missing-newline', {'retry': 'success'},
         lambda case: check_attempt_recovery(case, False)),
        ('legacy-failed-prediction', {'retry': 'success'}, check_legacy_failure),
        ('graceful-timeout', {'timeout': 'timeout'}, check_timeout),
        ('stubborn-timeout', {'stubborn': 'stubborn'}, check_escalation),
        ('evaluation-gate', {'empty': 'empty', 'failed': 'agent_always'},
         lambda case: check_gate(case, '--evaluate')),
        ('submission-gate', {'empty': 'empty', 'failed': 'agent_always'},
         lambda case: check_gate(case, '--submit')),
        ('invalid-timeout-arguments', {'empty': 'empty'}, check_arguments),
    ]
    for name, behaviors, verify in scenarios:
        case = Case(output, name, adapter, behaviors)
        result = {'name': name, 'passed': False, 'artifacts': str(case.root)}
        try:
            verify(case)
            result['passed'] = True
        except Exception:
            result['error'] = traceback.format_exc()
            write(case.root / 'failure.log', result['error'])
        finally:
            case.cleanup()
            result['invocations'] = case.invocations
            report['scenarios'].append(result)
            report['passed'] = all(item['passed'] for item in report['scenarios'])
            dump(output / 'report.json', report)
        print(('PASS' if result['passed'] else 'FAIL') + ' ' + name, flush=True)
    print('Report: ' + str(output / 'report.json'))
    return 0 if report['passed'] else 1


if __name__ == '__main__':
    sys.exit(main())
