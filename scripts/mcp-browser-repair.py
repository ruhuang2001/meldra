#!/usr/bin/env python3
"""Opt-in autonomous repair with the configured live model and real Chrome MCP.

No scripted model responses. The model only sees a disposable workspace copy.
A separate browser checks the same scenarios before and after; credentials and
provider reasoning are never written to evidence. This incurs paid inference.
"""
import argparse
from contextlib import contextmanager
import difflib
import hashlib
import json
import os
from pathlib import Path
import re
import select
import shutil
import signal
import subprocess
import sys
import time
import tomllib
import urllib.request

if not __debug__:
    raise SystemExit('Verification requires assertions; unset PYTHONOPTIMIZE and do not use -O.')

ROOT = Path(__file__).resolve().parents[1]
FIXTURE = ROOT / 'scripts/fixtures/browser-repair'
PROMPT = '''The Fieldwork workshop demo's Reserve seats action fails, and the phone layout is unusable. Repair the frontend, preserving workshop choices, pricing, required fields and seat limits.

Use the connected Chrome tools to reproduce both problems before editing. After repair, operate the real form again, inspect desktop and phone screenshots, and check console and network errors. Use actual form controls, not a simulated reservation. This is a disposable local demo: use only its loopback server, no external sites. README.md describes the API and development lifecycle.

Start the app as a managed process. Before editing files, use its documented clean shutdown and wait for exit code 0, then restart it to retest. Leave no processes running when finished. Change only app.js and style.css; do not change the server, add dependencies, or commit. Explain the observed failure and the verification results.'''


def dump(path, value):
    path.write_text(json.dumps(value, indent=2, ensure_ascii=False) + '\n')


def run(command, cwd, env, output, label, timeout):
    with (output / (label + '.stdout.txt')).open('w') as stdout, (output / (label + '.stderr.txt')).open('w') as stderr:
        process = subprocess.Popen(command, cwd=cwd, env=env, stdin=subprocess.DEVNULL,
                                   stdout=stdout, stderr=stderr, start_new_session=True)
        try:
            code = process.wait(timeout=timeout)
        except BaseException:
            os.killpg(process.pid, signal.SIGINT)
            try:
                process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                os.killpg(process.pid, signal.SIGKILL)
                process.wait()
            raise
    assert code == 0, f'{label} exited {code}; inspect {output}/{label}.stderr.txt'
    return (output / (label + '.stdout.txt')).read_text()


@contextmanager
def app(workspace, env, output, label):
    with (output / (label + '-server.log')).open('w') as log:
        process = subprocess.Popen([sys.executable, '-B', 'server.py'], cwd=workspace, env=env,
                                   stdout=subprocess.PIPE, stderr=log, text=True)
        try:
            assert select.select([process.stdout], [], [], 10)[0], 'server did not become ready'
            ready = process.stdout.readline()
            match = re.fullmatch(r'READY (http://127\.0\.0\.1:\d+)\n', ready)
            assert match, ready
            url = match[1]
            log.write(ready)
            yield url
        finally:
            if process.poll() is None:
                try:
                    if 'url' in locals():
                        with urllib.request.urlopen(urllib.request.Request(url + '/__dev/stop', data=b'', method='POST'), timeout=5) as response:
                            assert response.status == 200
                    process.wait(timeout=10)
                finally:
                    if process.poll() is None:
                        process.kill()
                        process.wait()
            log.write(process.stdout.read())
            assert process.returncode == 0, 'fixture server did not exit cleanly'


def audit(binary, workspace, env, output):
    tasks = json.loads(run([str(binary), 'tasks', '--json'], workspace, env, output, 'tasks', 30))
    assert len(tasks) == 1, tasks
    task_id = tasks[0]['id']
    record = json.loads(run([str(binary), 'task', 'show', task_id, '--json'], workspace, env, output, 'task', 30))
    run([str(binary), 'task', 'events', task_id, '--json'], workspace, env, output, 'events', 30)
    dump(output / 'task-record.json', record)
    calls = record['tool_calls']
    images = []
    for i, call in enumerate(calls):
        for ref in call['result'].get('artifacts', []):
            data = (Path(env['MELDRA_HOME']) / 'tasks/artifacts' / ref['sha256']).read_bytes()
            assert hashlib.sha256(data).hexdigest() == ref['sha256']
            if ref['name'].startswith('mcp-image-'):
                name = f'agent-{i:03d}-' + ref['name']
                (output / name).write_bytes(data)
                images.append({'call_index': i, 'file': name, 'sha256': ref['sha256']})
    edits = [i for i, call in enumerate(calls) if call['name'] in ('edit_file', 'apply_patch') and call['status'] == 'succeeded']
    checks = {'task_completed': record['task']['status'] == 'completed',
              'runs_succeeded': all(item['status'] == 'succeeded' for item in record['runs']),
              'no_unknown_outcomes': all(call['status'] not in ('unknown', 'running', 'planned') for call in calls),
              'edited_frontend': bool(edits),
              'screenshots_before_and_after': bool(edits and any(image['call_index'] < min(edits) for image in images) and any(image['call_index'] > max(edits) for image in images)),
              'clean_server_restarts': len(record['processes']) >= 2 and all(item['state'] == 'exited' and item.get('exit_code') == 0 and item['effects'] == 'known' for item in record['processes'])}
    for label, start, end in [('before', 0, min(edits) if edits else 0), ('after', max(edits) + 1 if edits else 0, len(calls))]:
        names = {call['name'].removeprefix('mcp__chrome__') for call in calls[start:end] if call['status'] == 'succeeded'}
        checks[label + '_form_interaction'] = bool(names & {'fill', 'fill_form', 'type_text'}) and 'click' in names
        checks[label + '_console_checked'] = bool(names & {'list_console_messages', 'get_console_message'})
        checks[label + '_network_checked'] = bool(names & {'list_network_requests', 'get_network_request'})
    evidence = {'checks': checks, 'images': images, 'tool_count': len(calls),
                'failed_tools': [{'name': call['name'], 'error': call['result'].get('error')} for call in calls if call['status'] == 'failed']}
    dump(output / 'agent-checks.json', evidence)
    return evidence


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', type=Path, required=True)
    parser.add_argument('--server-script', type=Path, required=True)
    parser.add_argument('--node', default=shutil.which('node'))
    parser.add_argument('--chrome', required=True, help='Installed Chrome executable')
    parser.add_argument('--puppeteer-module', type=Path, help='Module exporting puppeteer; defaults to the tested Chrome MCP bundle')
    parser.add_argument('--live-config', type=Path, required=True, help='Private Meldra home; opts in to paid model inference')
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--timeout', type=int, default=900, help='Wall-clock seconds for one autonomous run')
    args = parser.parse_args()
    binary, output, server = args.binary.resolve(), args.output.resolve(), args.server_script.resolve()
    module = (args.puppeteer_module or server.parent.parent / 'third_party/index.js').resolve()
    assert module.is_file() and binary.is_file() and server.is_file()
    config = tomllib.loads((args.live_config / 'config.toml').read_text())
    key = os.environ.get('OPENAI_API_KEY', '')
    credentials = args.live_config / 'credentials.env'
    if not key and credentials.exists():
        for line in credentials.read_text().splitlines():
            if line.startswith('OPENAI_API_KEY='):
                key = line.partition('=')[2].strip().strip('\"').strip("'")
    assert key, 'missing configured model key'
    endpoint = config.get('base_url', 'https://api.openai.com/v1')
    assert endpoint.startswith('https://'), 'live provider must use HTTPS'
    output.mkdir(parents=True, mode=0o700, exist_ok=False)
    workspace, home, state, tmp = [output / name for name in ('workspace', 'home', 'state', 'tmp')]
    for path in (home, state, tmp):
        path.mkdir(mode=0o700)
    shutil.copytree(FIXTURE, workspace, ignore=shutil.ignore_patterns('__pycache__'))
    env = {'PATH': os.environ.get('PATH', '/usr/bin:/bin'), 'HOME': str(home), 'MELDRA_HOME': str(state),
           'TMPDIR': str(tmp), 'XDG_CACHE_HOME': str(home / '.cache'), 'TERM': 'dumb', 'NO_COLOR': '1',
           'LANG': 'C.UTF-8', 'MELDRA_DISABLE_UPDATE_CHECK': '1', 'PYTHONDONTWRITEBYTECODE': '1'}
    (state / 'config.toml').write_text('model = ' + json.dumps(config['model']) + '\nbase_url = ' + json.dumps(endpoint) + '\n')
    (state / 'config.toml').chmod(0o600)
    dump(state / 'mcp.json', {'mcpServers': {'chrome': {
        'command': str(Path(args.node).resolve()),
        'args': [str(server), '--headless', '--isolated', '--no-usage-statistics', '--no-performance-crux',
                 '--viewport=1280x1000', '--screenshot-format=png', '--no-file-navigations',
                 '--executable-path=' + str(Path(args.chrome).resolve())],
        'allow_during_process': ['list_pages', 'new_page', 'select_page', 'navigate_page', 'close_page',
                                 'resize_page', 'take_snapshot', 'take_screenshot', 'fill', 'fill_form',
                                 'click', 'press_key', 'type_text', 'wait_for', 'evaluate_script',
                                 'list_console_messages', 'get_console_message',
                                 'list_network_requests', 'get_network_request'],
        'startup_timeout_sec': 60, 'tool_timeout_sec': 60}}})
    (state / 'mcp.json').chmod(0o600)
    (output / 'prompt.txt').write_text(PROMPT + '\n')
    started = time.monotonic()
    report = {'passed': False, 'model': config['model'], 'binary_sha256': hashlib.sha256(binary.read_bytes()).hexdigest(),
              'server_script': str(server), 'fixture_sha256': {path.name: hashlib.sha256(path.read_bytes()).hexdigest() for path in FIXTURE.iterdir() if path.is_file()}}
    try:
        for label in ('before', 'after'):
            if label == 'after':
                print('Running live Meldra repair; no scripted tool choices.', flush=True)
                run([str(binary), '--workspace', str(workspace), '--prompt', PROMPT, '--auto-approve', '--output-format', 'stream-json'],
                    workspace, dict(env, OPENAI_API_KEY=key), output, 'agent', args.timeout)
            print('Independent browser checks: ' + label, flush=True)
            with app(workspace, env, output, label) as url:
                run([args.node, str(ROOT / 'scripts/browser-repair-assert.mjs'), str(module), args.chrome, url,
                     str(output / label), 'broken' if label == 'before' else 'fixed'], workspace, env, output, label, 120)
    finally:
        if (output / 'agent.stdout.txt').exists():
            try:
                report['agent'] = audit(binary, workspace, env, output)
            except Exception as error:
                report['audit_error'] = str(error)
        try:
            expected = {path.name: path for path in FIXTURE.iterdir() if path.is_file()}
            actual = {str(path.relative_to(workspace)): path for path in workspace.rglob('*')}
            report['unexpected_entries'] = sorted(set(actual) - set(expected))
            report['missing_files'] = sorted(set(expected) - set(actual))
            report['changed_files'] = []
            diff = []
            for name, path in expected.items():
                modified = workspace / name
                if modified.is_symlink() or not modified.is_file():
                    report.setdefault('invalid_files', []).append(name)
                elif modified.read_bytes() != path.read_bytes():
                    report['changed_files'].append(name)
                    diff.extend(difflib.unified_diff(path.read_text().splitlines(True), modified.read_text(errors='replace').splitlines(True), fromfile='before/' + name, tofile='after/' + name))
            (output / 'repair.diff').write_text(''.join(diff))
        except Exception as error:
            report['diff_error'] = str(error)
        report['elapsed_seconds'] = round(time.monotonic() - started, 1)
        dump(output / 'report.json', report)
    assert not report.get('audit_error') and not report.get('diff_error'), report
    assert not report['unexpected_entries'] and not report['missing_files'] and not report.get('invalid_files'), report
    assert set(report['changed_files']) == {'app.js', 'style.css'}, report['changed_files']
    assert all(report['agent']['checks'].values()), report['agent']['checks']
    report['passed'] = True
    dump(output / 'report.json', report)
    print(json.dumps(report, indent=2))


if __name__ == '__main__':
    main()
