#!/usr/bin/env python3
"""Offline real-binary cross-feature checks for the plan integration tree."""
import argparse
import hashlib
import importlib.util
import json
import re
import subprocess
from pathlib import Path
import time
import traceback

ROOT = Path(__file__).resolve().parents[1]


def module(name, filename):
    spec = importlib.util.spec_from_file_location(name, ROOT / 'scripts' / filename)
    value = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(value)
    return value


cap = module('integration_capability', 'capability-e2e.py')


def events(result, status='succeeded'):
    parsed = [json.loads(line) for line in result.stdout.splitlines()]
    assert parsed and parsed[-1]['kind'] == 'session_end', result.stdout
    assert parsed[-1]['data']['status'] == status, parsed[-1]
    assert [entry['seq'] for entry in parsed] == list(range(1, len(parsed) + 1))
    assert all(entry['schema_version'] == 1 for entry in parsed)
    for key in ('offline-fixture-not-a-credential', web.KEY, 'PRIVATE_REASONING_CANARY'):
        assert key not in result.stdout and key not in result.stderr
    assert 'reasoning' not in result.stdout.lower()
    for line in result.stderr.splitlines():
        try:
            json.loads(line)
        except ValueError:
            continue
        raise AssertionError('JSON written to stderr: ' + line)
    return parsed


def command_stream(binary, output):
    with Case(binary, output, 'command-stream', [{}, {}, {}]) as case:
        commands.command(case, 'review', 'WHOLE=[$ARGUMENTS] FIRST=[$1] SECOND=[$2]')
        commands.command(case, 'literal', '/review $ARGUMENTS')
        commands.command(case, 'mode', '/plan')
        commands.command(case, 'plan', 'BUILTIN_SHADOW_FORBIDDEN')
        for index, (prompt, expected) in enumerate([
            ('/review $2  $ARGUMENTS', 'WHOLE=[$2  $ARGUMENTS] FIRST=[$2] SECOND=[$ARGUMENTS]'),
            ('/literal retained', '/review retained'),
            ('/mode', '/plan'),
        ]):
            result = case.chat(prompt, flags=['--output-format', 'stream-json'], label='command-' + str(index))
            transcript = events(result)
            assert commands.user_texts(case.requests[-1])[-1] == expected, case.requests[-1]
            assert any(entry['kind'] == 'notice' and 'built-in command /plan' in entry['data']['text'] for entry in transcript)
            assert transcript[0]['data']['mode'] == 'build'
        records = case.audit()
        assert len(records) == 3 and all(record['task']['status'] == 'completed' for record in records)
        case.complete()


def command_image_policy(binary, output):
    with Case(binary, output, 'command-image-policy', [
        {'calls': [('read_file', {'path': 'note.txt'}), cap.edit('forbidden.txt', 'NO')]}, {}
    ]) as case:
        data = images.png()
        write(case.workspace / 'shot.png', data)
        write(case.workspace / 'note.txt', 'INTEGRATED_TEXT_MARKER')
        commands.command(case, 'inspect', 'Inspect @shot.png and @note.txt. Focus: $ARGUMENTS')
        result = case.chat('/inspect $1', flags=['--output-format', 'stream-json', '--mode', 'plan', '--auto-approve'])
        transcript = events(result)
        assert len(case.requests) == 2
        for request in case.requests:
            images.assert_image(request, data)
            assert not request.get('previous_response_id')
        assert 'Focus: $1' in json.dumps(case.requests[0])
        assert 'function_call_output' in json.dumps(case.requests[1])
        assert not (case.workspace / 'forbidden.txt').exists()
        assert any(entry['kind'] == 'tool_finished' and entry['data']['name'] == 'edit_file' and entry['data']['status'] == 'declined' for entry in transcript)
        records = images.audit_images(case, data)
        assert any(entry['kind'] == 'tool.policy_denied' for entry in records[0]['events'])
        case.complete()


def web_approval(binary, output, origin):
    with Case(binary, output, 'web-approval', [
        {'calls': [('web_fetch', {'url': 'https://source.example/article?a=1&b=2'})]}, {'text': 'WEB_RESUME_COMPLETE'}
    ]) as case:
        origin.attach(case, hosted=True)
        before = len(origin.calls)
        result = case.chat('Fetch the public reference.', flags=['--output-format', 'stream-json'], success=False)
        assert result.returncode == 3
        transcript = events(result, 'waiting_approval')
        approval = next(entry for entry in transcript if entry['kind'] == 'approval_required')
        assert approval['data']['operation'] == 'external_read'
        assert 'DeepSeek' in approval['data']['detail']
        assert len(origin.calls) == before, 'network dispatched before stream approval'
        records = case.audit()
        assert records[0]['task']['status'] == 'waiting_approval'
        assert records[0]['tool_calls'][0]['effect'] == 'external_read'
        wrong = case.run(['task', 'resume', approval['task_id'], '--approve-detail-hash', '0' * 64], success=False, label='wrong-hash')
        assert wrong.returncode == 1 and len(origin.calls) == before
        events(wrong, 'failed')
        resumed = case.run(['task', 'resume', approval['task_id'], '--approve-detail-hash', approval['data']['detail_hash']], label='exact-hash')
        events(resumed)
        assert len(origin.calls) == before + 1
        assert origin.calls[-1]['authorized_with_independent_key'] is True
        assert web.KEY not in json.dumps(case.requests)
        records = case.audit()
        assert records[0]['task']['status'] == 'completed'
        completed = [call for call in records[0]['tool_calls'] if call['status'] == 'succeeded']
        assert len(completed) == 1 and completed[0]['effect'] == 'external_read'
        assert any(a['decision'] == 'approved' and a['detail'].get('approved_detail_hash') == approval['data']['detail_hash'] for a in records[0]['approvals'])
        for artifact in (case.state / 'tasks/artifacts').iterdir():
            assert web.KEY.encode() not in artifact.read_bytes()
        assert web.KEY not in json.dumps(records)
        case.complete()


def native_binary(output):
    source = (ROOT / 'internal/app/agent.go').read_text()
    pattern = r'func isCustomBaseURL\(baseURL string\) bool \{.*?\n\}'
    replacement, count = re.subn(pattern, 'func isCustomBaseURL(baseURL string) bool { return false }', source, count=1, flags=re.S)
    assert count == 1
    path = write(output / 'native_agent_fixture.go', replacement)
    subprocess.run(['gofmt', '-w', str(path)], check=True)
    overlay = output / 'native-overlay.json'
    dump(overlay, {'Replace': {str(ROOT / 'internal/app/agent.go'): str(path)}})
    binary = output / 'meldra-native-fixture'
    result = subprocess.run(['go', 'build', '-race', '-overlay', str(overlay), '-o', str(binary), '.'], cwd=ROOT, capture_output=True, text=True)
    write(output / 'native-fixture-build.txt', result.stdout + result.stderr)
    assert result.returncode == 0, result.stderr
    dump(output / 'native-fixture-provenance.json', {'binary_sha256': hashlib.sha256(binary.read_bytes()).hexdigest(), 'source_sha256': hashlib.sha256(source.encode()).hexdigest(), 'overlay': str(overlay), 'boundary': 'Only app isCustomBaseURL classification forced false. Provider transport and Responses implementation unchanged; configured endpoint is loopback.'})
    return binary


def native_image(binary, output):
    data = images.png()
    with Case(binary, output, 'native-image', [{'calls': [('read_file', {'path': 'note.txt'})]}, {}, {}]) as case:
        case.env['GORACE'] = 'atexit_sleep_ms=0'
        write(case.workspace / 'shot.png', data)
        write(case.workspace / 'note.txt', 'NATIVE_TOOL_RESULT')
        result = case.chat('Describe @shot.png', flags=['--output-format', 'stream-json'])
        events(result)
        images.assert_image(case.requests[0], data)
        assert case.requests[1].get('previous_response_id') == 'resp_1', case.requests[1]
        assert not images.images(case.requests[1]), 'native continuation duplicated image payload'
        assert 'function_call_output' in json.dumps(case.requests[1])
        records = images.audit_images(case, data)
        (case.workspace / 'shot.png').unlink()
        resumed = case.run(['resume', records[0]['task']['id'], '--prompt', 'Describe the saved image.', '--output-format', 'stream-json'], label='resume')
        events(resumed)
        images.assert_image(case.requests[-1], data)
        assert not case.requests[-1].get('previous_response_id')
        case.complete()


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--binary', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    global commands, images, web, Case, write, dump
    if not (ROOT / 'scripts/web-e2e.py').exists() or not (ROOT / 'internal/app/jsonstream.go').exists():
        raise SystemExit('This manual suite requires the commands, images, web tools and stream-json features in the same checkout.')
    commands = module('integration_commands', 'commands-e2e.py')
    images = module('integration_images', 'images-e2e.py')
    web = module('integration_web', 'web-e2e.py')
    Case, write, dump = images.Case, cap.write, cap.dump
    binary, output = args.binary.resolve(), args.output.resolve()
    output.mkdir(parents=True, exist_ok=False)
    report = {'passed': False, 'live_model': False, 'binary_sha256': hashlib.sha256(binary.read_bytes()).hexdigest(), 'scenarios': []}
    origin = None
    try:
        fixture = web.fixture_binary(output)
        native = native_binary(output)
        origin = web.Origin(output)
        scenarios = {'command-stream': lambda: command_stream(binary, output),
                     'command-image-policy': lambda: command_image_policy(binary, output),
                     'web-approval': lambda: web_approval(fixture, output, origin),
                     'native-image': lambda: native_image(native, output)}
        for name, scenario in scenarios.items():
            result, started = {'name': name, 'passed': False}, time.monotonic()
            try:
                scenario()
                result['passed'] = True
            except Exception:
                result['error'] = traceback.format_exc()
            result['elapsed_seconds'] = round(time.monotonic() - started, 3)
            report['scenarios'].append(result)
            dump(output / 'report.json', report)
            print(('PASS ' if result['passed'] else 'FAIL ') + name, flush=True)
            if not result['passed']:
                print(result['error'], flush=True)
    finally:
        if origin:
            origin.close()
        files = [*ROOT.glob('internal/**/*.go'), ROOT / 'go.mod', ROOT / 'go.sum', Path(__file__).resolve()]
        report['source_sha256'] = {str(path.relative_to(ROOT)): hashlib.sha256(path.read_bytes()).hexdigest() for path in sorted(files)}
        report['passed'] = len(report['scenarios']) == 4 and all(result['passed'] for result in report['scenarios'])
        dump(output / 'report.json', report)
        dump(output / 'checksums.json', {str(path.relative_to(output)): hashlib.sha256(path.read_bytes()).hexdigest() for path in sorted(output.rglob('*')) if path.is_file()})
    raise SystemExit(0 if report['passed'] else 1)


if __name__ == '__main__':
    main()
