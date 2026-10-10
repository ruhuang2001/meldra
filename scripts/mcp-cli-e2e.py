#!/usr/bin/env python3
"""MCP catalog CLI E2E against real binaries and local protocol/provider fixtures.

Failure cases specified before implementation:
- invalid/duplicate arguments, unknown/disabled server must fail before spawn;
- explicit catalog selection must never launch another configured server;
- resources/prompts listing and retrieval must work without model credentials;
- pagination, template URIs, binary resource data and prompt roles must survive;
- selected prompt text enters --run only after confirmation, as user input;
- denied --run makes no model request; non-text prompt --run fails explicitly;
- catalog calls must leave inspectable task/artifact evidence.
"""
import argparse
import hashlib
import http.server
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import threading
import traceback


spec = importlib.util.spec_from_file_location('mcp_e2e', Path(__file__).with_name('mcp-e2e.py'))
fixture = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fixture)


def serve(log):
    fixture.record(log, {'started_pid': os.getpid(), 'cwd': os.getcwd()})
    for line in sys.stdin:
        request = json.loads(line)
        reply = fixture.mcp_reply(request, log)
        method = request.get('method')
        if method == 'initialize':
            reply['result']['capabilities'] = {'resources': {}, 'prompts': {}}
        elif method == 'prompts/list':
            reply['result']['nextCursor'] = 'next-prompts'
        elif method == 'prompts/get':
            if request['params']['name'] == 'image':
                reply['result'] = {'messages': [{'role': 'user', 'content': {'type': 'image', 'data': 'eA==', 'mimeType': 'image/png'}}]}
            else:
                reply['result']['messages'].append({'role': 'assistant', 'content': {'type': 'text', 'text': 'Assistant role is quoted context'}})
        if reply is not None:
            print(json.dumps(reply), flush=True)


def run_case(binary, output, name):
    directory = output / name
    directory.mkdir()
    home, workspace = directory / 'home', directory / 'workspace'
    home.mkdir(mode=0o700)
    workspace.mkdir()
    wire = directory / 'selected.jsonl'
    unrelated = directory / 'unrelated.jsonl'
    command = {'command': sys.executable, 'args': [str(Path(__file__).resolve()), '--fixture', str(wire)]}
    config = {'mcpServers': {'selected': command, 'unrelated': dict(command, args=[str(Path(__file__).resolve()), '--fixture', str(unrelated)])}}
    if name == 'disabled':
        command['disabled'] = True
    fixture.save(home / 'mcp.json', config)
    (home / 'mcp.json').chmod(0o600)
    env = {key: value for key, value in os.environ.items() if not key.startswith(('MELDRA_', 'OPENAI_'))}
    env.update(MELDRA_HOME=str(home), TERM='dumb')
    model = http.server.ThreadingHTTPServer(('127.0.0.1', 0), fixture.Handler)
    model.daemon_threads = True
    model.directory = directory
    model.requests, model.errors, model.advertised, model.authorization = [], [], [], []
    model.steps = []
    model.response_index = 0
    threading.Thread(target=model.serve_forever, daemon=True).start()
    base = 'http://127.0.0.1:' + str(model.server_port) + '/v1'
    (home / 'config.toml').write_text('model="fixture"\nbase_url="' + base + '"\nallow_insecure_base_url=true\n')
    (home / 'config.toml').chmod(0o600)
    calls = {
        'resources': ['resources', 'selected'],
        'pagination': ['resources', 'selected', 'second'],
        'templates': ['templates', 'selected'],
        'read': ['read', 'selected', 'fixture://custom'],
        'prompts': ['prompts', 'selected', 'second'],
        'prompt': ['prompt', 'selected', 'review', 'focus=security=review'],
        'image': ['prompt', 'selected', 'image', 'focus=image'],
        'run': ['prompt', 'selected', 'review', 'focus=security', '--run'],
        'run-auto': ['prompt', 'selected', 'review', 'focus=security', '--run', '--auto-approve'],
        'run-deny': ['prompt', 'selected', 'review', 'focus=security', '--run'],
        'run-image': ['prompt', 'selected', 'image', 'focus=image', '--run', '--auto-approve'],
        'disabled': ['resources', 'selected'],
        'unknown': ['resources', 'missing'],
        'invalid-pair': ['prompt', 'selected', 'review', 'not-a-pair'],
        'duplicate-pair': ['prompt', 'selected', 'review', 'focus=a', 'focus=b'],
        'invalid-read': ['read', 'selected'],
        'invalid-run': ['resources', 'selected', '--run'],
        'invalid-flag': ['resources', 'selected', '--something'],
    }
    args = [str(binary), 'mcp'] + calls[name]
    if name != 'resources':
        args += ['--workspace', str(workspace)]
    if name in ('run', 'run-auto'):
        env['OPENAI_API_KEY'] = 'fixture-model-key'
    no_spawn = name in ('disabled', 'unknown', 'invalid-pair', 'duplicate-pair', 'invalid-read', 'invalid-run', 'invalid-flag')
    try:
        result = subprocess.run(args, input='y\n' if name == 'run' else 'n\n' if name == 'run-deny' else '', capture_output=True, text=True, env=env, cwd=workspace, timeout=30)
        (directory / 'stdout.txt').write_text(result.stdout)
        (directory / 'stderr.txt').write_text(result.stderr)
        assert not unrelated.exists(), 'unselected MCP process was launched'
        assert not model.errors, model.errors
        if no_spawn:
            assert result.returncode != 0 and not wire.exists(), 'invalid invocation spawned a server'
            return {'name': name, 'passed': True}
        assert wire.exists(), 'selected MCP was not launched'
        if name == 'run-image':
            assert result.returncode != 0 and 'text prompt' in result.stderr, result.stderr
        else:
            assert result.returncode == 0, result.stderr
        messages = fixture.messages(wire)
        assert all(row.get('cwd', str(workspace)) == str(workspace) for row in messages), messages
        if name in ('run', 'run-auto'):
            assert model.requests, 'selected prompt never reached model'
            for request in model.requests:
                assert 'prompt-content-evidence: security' in json.dumps(request['input']), request
                assert 'MCP message role:' in json.dumps(request['input']), request
                assert 'prompt-content-evidence' not in request.get('instructions', ''), 'prompt elevated to system instructions'
        else:
            assert not model.requests, 'display/declined prompt invoked the model'
        if name == 'run-deny':
            assert 'Declined' in result.stdout, result.stdout
        elif name not in ('run', 'run-auto', 'run-image'):
            data = json.loads(result.stdout)
            if name == 'resources':
                assert data['nextCursor'] == 'second', data
            elif name == 'pagination':
                assert data['resources'][0]['uri'] == 'fixture://second', data
            elif name == 'templates':
                assert data['resourceTemplates'][0]['uriTemplate'] == 'fixture://{name}', data
            elif name == 'read':
                assert data['contents'][1]['blob'] == 'YmluYXJ5', data
            elif name == 'prompts':
                assert data['nextCursor'] == 'next-prompts' and any(row.get('params', {}).get('cursor') == 'second' for row in messages), data
            elif name == 'prompt':
                assert data['messages'][0]['content']['text'].endswith('security=review') and data['messages'][1]['role'] == 'assistant', data
            elif name == 'image':
                assert data['messages'][0]['content']['type'] == 'image', data
        listing = subprocess.run([str(binary), 'tasks', '--json'], capture_output=True, text=True, env=env, timeout=10)
        tasks = json.loads(listing.stdout)
        assert tasks, 'catalog command left no task evidence'
        details = []
        for task in tasks:
            show = subprocess.run([str(binary), 'task', 'show', task['id'], '--json'], capture_output=True, text=True, env=env, timeout=10)
            details.append(json.loads(show.stdout))
        fixture.save(directory / 'tasks.json', details)
        assert any(call['result'].get('artifacts') for detail in details for call in detail.get('tool_calls') or []), 'no catalog artifact'
        return {'name': name, 'passed': True}
    finally:
        model.shutdown()
        model.server_close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', type=Path)
    parser.add_argument('--output', type=Path)
    parser.add_argument('--fixture', type=Path)
    parser.add_argument('--case')
    args = parser.parse_args()
    if args.fixture:
        serve(args.fixture)
        return
    binary, output = args.binary.resolve(), args.output.resolve()
    output.mkdir(parents=True)
    checks = []
    for name in ('resources', 'pagination', 'templates', 'read', 'prompts', 'prompt', 'image', 'run', 'run-auto', 'run-deny', 'run-image', 'disabled', 'unknown', 'invalid-pair', 'duplicate-pair', 'invalid-read', 'invalid-run', 'invalid-flag'):
        if args.case and name != args.case:
            continue
        try:
            check = run_case(binary, output, name)
        except Exception:
            check = {'name': name, 'passed': False, 'error': traceback.format_exc()}
        checks.append(check)
        fixture.save(output / 'report.json', {'binary_sha256': hashlib.sha256(binary.read_bytes()).hexdigest(), 'scenarios': checks})
        print(name + ': ' + ('PASS' if check['passed'] else 'FAIL'), flush=True)
    fixture.save(output / 'checksums.json', {str(p.relative_to(output)): hashlib.sha256(p.read_bytes()).hexdigest() for p in sorted(output.rglob('*')) if p.is_file()})
    assert checks and all(check['passed'] for check in checks), 'MCP CLI checks failed: ' + str(output / 'report.json')


if __name__ == '__main__':
    main()
