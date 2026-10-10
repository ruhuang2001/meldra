#!/usr/bin/env python3
"""Regression: unanswered form, URL, or sampling approval must time out with stdin open.
The process must exit, persist unknown, and leak no reader or MCP process.
"""
import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import select
import subprocess
import sys
import time


def record_pid(wire):
    wire.with_suffix('.pid').write_text(str(os.getpid()))


def check_fixture_exit(wire):
    pid = int(wire.with_suffix('.pid').read_text())
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return
    raise AssertionError(f'MCP fixture {pid} survived Meldra exit')


def cancel_fixture(wire, url=False):
    record_pid(wire)
    def send(value):
        print(json.dumps(dict(jsonrpc='2.0', **value)), flush=True)

    def receive():
        value = json.loads(sys.stdin.readline())
        with wire.open('a') as log:
            log.write(json.dumps(value) + '\n')
        return value

    while True:
        request = receive()
        if 'id' not in request:
            continue
        method = request.get('method')
        if method == 'server/discover':
            send({'id': request['id'], 'error': {'code': -32601, 'message': 'legacy fixture'}})
            continue
        result = {}
        if method == 'initialize':
            result = {'protocolVersion': '2025-11-25', 'serverInfo': {'name': 'cancel-fixture', 'version': '1'}, 'capabilities': {'tools': {}}}
        elif method == 'tools/list':
            result = {'tools': [{'name': 'interact', 'inputSchema': {'type': 'object', 'properties': {}}}]}
        elif method == 'tools/call':
            params = {'mode': 'form', 'message': 'cancel-first-form', 'requestedSchema': {'type': 'object', 'properties': {'color': {'type': 'string'}}, 'required': ['color']}}
            first_params = {'mode': 'url', 'message': 'cancel-first-form', 'url': 'https://example.com/fixture', 'elicitationId': 'first-url'} if url else params
            send({'id': 'first', 'method': 'elicitation/create', 'params': first_params})
            deadline = time.monotonic() + 10
            while not wire.with_suffix('.ready').exists():
                assert time.monotonic() < deadline, 'first form was not displayed'
                time.sleep(0.01)
            send({'method': 'notifications/cancelled', 'params': {'requestId': 'first', 'reason': 'fixture superseded form'}})
            first = receive()
            assert first['id'] == 'first', first
            params['message'] = 'accept-second-form'
            send({'id': 'second', 'method': 'elicitation/create', 'params': params})
            second = receive()
            assert second['id'] == 'second' and second['result']['action'] == 'accept' and second['result']['content']['color'] == 'blue', second
            result = {'content': [{'type': 'text', 'text': 'second-form-accepted'}]}
        send({'id': request['id'], 'result': result})


def cancel_then_continue(binary, output, fixture):
    directory, home, workspace, server, env = fixture.setup(output, 'cancel-then-continue')
    process = None
    transcript = bytearray()
    try:
        wire = directory / 'wire.jsonl'
        fixture.set_config(home, {'command': sys.executable, 'args': [str(Path(__file__).resolve()), '--cancel-fixture', str(wire)], 'tool_timeout_sec': 20})
        process = subprocess.Popen([str(binary), '--workspace', str(workspace), '--prompt', 'request interaction', '--auto-approve'], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=env)
        for marker in (b'cancel-first-form', b'accept-second-form'):
            deadline = time.monotonic() + 12
            while marker not in transcript:
                assert time.monotonic() < deadline, ('missing prompt', marker, transcript.decode())
                if select.select([process.stdout], [], [], 0.2)[0]:
                    chunk = os.read(process.stdout.fileno(), 65536)
                    assert chunk, ('unexpected EOF', transcript.decode())
                    transcript.extend(chunk)
            if marker == b'cancel-first-form':
                wire.with_suffix('.ready').touch()
        stdout, stderr = process.communicate(b'{"color":"blue"}\nfollowup-after-cancellation\n', timeout=15)
        transcript.extend(stdout)
        transcript.extend(stderr)
        assert process.returncode == 0 and b'WARNING: DATA RACE' not in transcript, (process.returncode, transcript.decode())
        check_fixture_exit(wire)
        fixture.write(directory / 'provider-requests.json', server.provider_requests)
        assert any('followup-after-cancellation' in json.dumps(request.get('input')) for request in server.provider_requests), server.provider_requests
        tasks = json.loads(subprocess.check_output([str(binary), 'tasks', '--json'], env=env, text=True, timeout=10))
        detail = json.loads(subprocess.check_output([str(binary), 'task', 'show', tasks[0]['id'], '--json'], env=env, text=True, timeout=10))
        fixture.write(directory / 'task.json', detail)
        assert len(detail['tool_calls']) == 1 and detail['tool_calls'][0]['status'] == 'succeeded', detail
        return {'name': 'cancel-then-continue', 'passed': True}
    finally:
        if process is not None and process.poll() is None:
            process.kill()
            stdout, stderr = process.communicate()
            transcript.extend(stdout + stderr)
        (directory / 'transcript.txt').write_bytes(transcript)
        server.shutdown()
        server.server_close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    binary, output = args.binary.resolve(), args.output.resolve()
    output.mkdir()
    fixture_path = Path(__file__).with_name('mcp-auth-interaction-e2e.py').resolve()
    spec = importlib.util.spec_from_file_location('interaction_fixture', fixture_path)
    fixture = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(fixture)
    reports = []
    for mode in ('form-accept', 'url-accept', 'sampling-accept'):
        directory, home, workspace, server, env = fixture.setup(output, mode)
        process = None
        try:
            fixture.set_config(home, {'command': sys.executable, 'args': [str(Path(__file__).resolve()), '--tracked-fixture', str(directory / 'wire.jsonl'), mode], 'sampling': mode.startswith('sampling'), 'tool_timeout_sec': 1})
            started = time.monotonic()
            process = subprocess.Popen([str(binary), '--workspace', str(workspace), '--prompt', 'request interaction', '--auto-approve'], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, env=env)
            # Leave stdin open deliberately: communicate(input='') would hide the bug.
            process.wait(timeout=8)
            stdout, stderr = process.communicate()
            (directory / 'transcript.txt').write_text(stdout + stderr)
            assert process.returncode != 0 and 'unknown outcome' in stderr, (process.returncode, stderr)
            assert 'WARNING: DATA RACE' not in stderr
            check_fixture_exit(directory / 'wire.jsonl')
            tasks = json.loads(subprocess.check_output([str(binary), 'tasks', '--json'], env=env, text=True, timeout=10))
            detail = json.loads(subprocess.check_output([str(binary), 'task', 'show', tasks[0]['id'], '--json'], env=env, text=True, timeout=10))
            fixture.write(directory / 'task.json', detail)
            assert len(detail['tool_calls']) == 1 and detail['tool_calls'][0]['status'] == 'unknown', detail
            reports.append({'name': mode, 'passed': True, 'elapsed_seconds': round(time.monotonic() - started, 2)})
            fixture.write(output / 'report.json', reports)
        finally:
            if process is not None and process.poll() is None:
                process.kill()
                stdout, stderr = process.communicate()
                (directory / 'transcript.txt').write_text(stdout + stderr)
            server.shutdown()
            server.server_close()
    reports.append(cancel_then_continue(binary, output, fixture))
    reports.append(cancel_tui_approval(binary, output, fixture))
    reports.extend(stdin_sources(binary, output, fixture))
    fixture.write(output / 'report.json', reports)
    fixture.write(output / 'checksums.json', {str(p.relative_to(output)): hashlib.sha256(p.read_bytes()).hexdigest() for p in output.rglob('*') if p.is_file() and 'home' not in p.relative_to(output).parts})
    print(json.dumps(reports, indent=2))


def cancel_tui_approval(binary, output, fixture):
    directory, home, workspace, server, env = fixture.setup(output, 'cancel-tui-approval')
    wire = directory / 'wire.jsonl'
    fixture.set_config(home, {'command': sys.executable, 'args': [str(Path(__file__).resolve()), '--cancel-url-fixture', str(wire)], 'tool_timeout_sec': 20})
    master, slave = fixture.pty.openpty()
    fixture.fcntl.ioctl(slave, fixture.termios.TIOCSWINSZ, fixture.struct.pack('HHHH', 45, 150, 0, 0))
    process = subprocess.Popen([str(binary), '--workspace', str(workspace), '--prompt', 'request interaction', '--auto-approve'], stdin=slave, stdout=slave, stderr=slave, env=dict(env, TERM='xterm-256color'))
    os.close(slave)
    transcript = bytearray()
    answered = stopped = False
    try:
        deadline = time.monotonic() + 25
        while time.monotonic() < deadline:
            if select.select([master], [], [], 0.05)[0]:
                try:
                    chunk = os.read(master, 65536)
                except OSError:
                    break
                if not chunk:
                    break
                transcript.extend(chunk)
            if b'cancel-first-form' in transcript:
                wire.with_suffix('.ready').touch()
            if not answered and b'accept-second-form' in transcript:
                os.write(master, b'{"color":"blue"}\r')
                answered = True
            if not stopped and b'E2E complete' in transcript:
                os.write(master, b'\x03')
                stopped = True
            if process.poll() is not None:
                break
        process.wait(timeout=5)
        assert answered and stopped and process.returncode == 0 and b'WARNING: DATA RACE' not in transcript, transcript.decode(errors='replace')
        check_fixture_exit(wire)
        tasks = json.loads(subprocess.check_output([str(binary), 'tasks', '--json'], env=env, text=True, timeout=10))
        detail = json.loads(subprocess.check_output([str(binary), 'task', 'show', tasks[0]['id'], '--json'], env=env, text=True, timeout=10))
        fixture.write(directory / 'task.json', detail)
        assert len(detail['tool_calls']) == 1 and detail['tool_calls'][0]['status'] == 'succeeded', detail
        return {'name': 'cancel-tui-approval', 'passed': True}
    finally:
        if process.poll() is None:
            process.kill()
            process.wait()
        os.close(master)
        (directory / 'transcript.txt').write_bytes(transcript)
        server.shutdown()
        server.server_close()


def stdin_sources(binary, output, fixture):
    reports = []
    for name in ('regular-file', 'dev-null', 'dev-zero'):
        directory, home, workspace, server, env = fixture.setup(output, name)
        try:
            regular = directory / 'stdin.txt'
            regular.write_text('')
            source = regular if name == 'regular-file' else Path(os.devnull if name == 'dev-null' else '/dev/zero')
            # --prompt avoids a blocking user read before the unsupported-device guard.
            with source.open('rb') as stdin:
                result = subprocess.run([str(binary), '--workspace', str(workspace), '--prompt', 'EOF fixture'], stdin=stdin, capture_output=True, timeout=8, env=env)
            (directory / 'transcript.txt').write_bytes(result.stdout + result.stderr)
            assert b'WARNING: DATA RACE' not in result.stderr
            if name == 'dev-zero':
                assert result.returncode != 0 and b'unsupported nonterminal character device' in result.stderr, result
                assert not server.provider_requests, server.provider_requests
            else:
                assert result.returncode == 0 and b'E2E complete' in result.stdout, result
            reports.append({'name': name, 'passed': True})
        finally:
            server.shutdown()
            server.server_close()
    return reports


if __name__ == '__main__':
    if len(sys.argv) == 3 and sys.argv[1] in ('--cancel-fixture', '--cancel-url-fixture'):
        cancel_fixture(Path(sys.argv[2]), url=sys.argv[1] == '--cancel-url-fixture')
    elif len(sys.argv) == 4 and sys.argv[1] == '--tracked-fixture':
        wire = Path(sys.argv[2])
        record_pid(wire)
        fixture_path = Path(__file__).with_name('mcp-auth-interaction-e2e.py')
        spec = importlib.util.spec_from_file_location('interaction_fixture', fixture_path)
        fixture = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(fixture)
        fixture.fixture(wire, sys.argv[3])
    else:
        main()
