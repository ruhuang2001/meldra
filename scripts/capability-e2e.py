#!/usr/bin/env python3
"""Exercise capability boundaries with a real Meldra binary and an offline provider.

No configured provider, credentials, or external MCP server is used. Each case
gets a private HOME, state directory, workspace, and loopback Responses server.
The optional --long-command case really runs longer than 120 seconds.
"""
import argparse
import errno
import fcntl
import hashlib
import http.server
import json
import os
from pathlib import Path
import platform
import pty
import select
import struct
import subprocess
import sys
import threading
import time
import termios
import traceback


ROOT = Path(__file__).resolve().parents[1]


def write(path, content):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(content if isinstance(content, bytes) else content.encode())
    return path


def dump(path, value):
    write(path, json.dumps(value, indent=2, ensure_ascii=False) + '\n')


def text(value):
    return json.dumps(value, ensure_ascii=False)


def edit(path, content):
    return ('edit_file', {'path': path, 'old_str': '', 'new_str': content})


def command(script, timeout=30):
    return ('run_command', {'command': 'python3', 'args': [script], 'timeout': timeout})


class Case:
    def __init__(self, binary, output, name, steps=()):
        self.binary, self.directory = binary, output / name
        self.directory.mkdir()
        self.workspace, self.home, self.state, self.tmp = [self.directory / value for value in ('workspace', 'home', 'state', 'tmp')]
        for directory in (self.workspace, self.home, self.state, self.tmp):
            directory.mkdir(mode=0o700)
        self.env = {'PATH': os.environ.get('PATH', '/usr/bin:/bin'), 'HOME': str(self.home),
                    'MELDRA_HOME': str(self.state), 'TMPDIR': str(self.tmp),
                    'XDG_CACHE_HOME': str(self.home / '.cache'), 'NO_COLOR': '1',
                    'TERM': 'dumb', 'LANG': 'C.UTF-8', 'OPENAI_API_KEY': 'offline-fixture-not-a-credential'}
        if os.environ.get('GOCOVERDIR'):
            self.env['GOCOVERDIR'] = os.environ['GOCOVERDIR']
        self.steps, self.requests, self.errors = list(steps), [], []
        self.condition, self.release, self.cancelled = threading.Condition(), threading.Event(), threading.Event()
        self.processes = []
        case = self

        class Handler(http.server.BaseHTTPRequestHandler):
            def log_message(self, *_args):
                pass

            def do_POST(self):
                try:
                    assert self.path in ('/v1/responses', '/responses'), self.path
                    request = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
                    with case.condition:
                        case.requests.append(request)
                        index = len(case.requests)
                        assert case.steps, 'unexpected provider request'
                        step = case.steps.pop(0)
                        case.condition.notify_all()
                    if callable(step):
                        step = step(case, request)
                    if step.get('stream_wait'):
                        self.send_response(200)
                        self.send_header('Content-Type', 'text/event-stream')
                        self.send_header('Connection', 'close')
                        self.end_headers()
                        event = {'type': 'response.output_text.delta', 'item_id': f'msg_{index}',
                                 'output_index': 0, 'content_index': 0, 'delta': 'CAPABILITY_PARTIAL_STREAM'}
                        self.wfile.write(('event: ' + event['type'] + '\ndata: ' + text(event) + '\n\n').encode())
                        self.wfile.flush()
                        deadline = time.monotonic() + 30
                        while not case.release.wait(0.05):
                            assert time.monotonic() < deadline, 'blocked stream was not released'
                            self.wfile.write(b': fixture heartbeat\n\n')
                            self.wfile.flush()
                    output = [{'type': 'function_call', 'id': f'fc_{index}_{i}', 'call_id': f'call_{index}_{i}',
                               'name': name, 'arguments': text(arguments), 'status': 'completed'}
                              for i, (name, arguments) in enumerate(step.get('calls', []))]
                    if not output:
                        output = [{'type': 'message', 'id': f'msg_{index}', 'role': 'assistant', 'status': 'completed',
                                   'content': [{'type': 'output_text', 'text': step.get('text', 'CAPABILITY_COMPLETE'), 'annotations': []}]}]
                    response = {'id': f'resp_{index}', 'object': 'response', 'status': 'completed', 'output': output,
                                'usage': {'input_tokens': 1, 'output_tokens': 1, 'total_tokens': 2}}
                    event = {'type': 'response.completed', 'response': response}
                    payload = ('event: response.completed\ndata: ' + text(event) + '\n\n').encode()
                    if not step.get('stream_wait'):
                        self.send_response(200)
                        self.send_header('Content-Type', 'text/event-stream')
                        self.send_header('Content-Length', str(len(payload)))
                        self.end_headers()
                    self.wfile.write(payload)
                    self.wfile.flush()
                except (BrokenPipeError, ConnectionResetError):
                    case.cancelled.set()
                except Exception:
                    case.errors.append(traceback.format_exc())
                    self.send_error(500)

        self.server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        write(self.state / 'config.toml', f'model="capability-fixture"\nbase_url="http://127.0.0.1:{self.server.server_port}/v1"\nallow_insecure_base_url=true\n').chmod(0o600)

    def __enter__(self):
        return self

    def __exit__(self, *_args):
        self.release.set()
        for process in self.processes:
            if process.poll() is None:
                process.terminate()
                try:
                    process.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=5)
        self.server.shutdown()
        self.server.server_close()
        dump(self.directory / 'provider-requests.json', self.requests)
        dump(self.directory / 'server-errors.json', self.errors)

    def run(self, args, stdin='', timeout=30, success=True, label='cli'):
        result = subprocess.run([str(self.binary), *args], cwd=self.workspace, env=self.env,
                                input=stdin, capture_output=True, text=True, timeout=timeout)
        write(self.directory / (label + '.stdout.txt'), result.stdout)
        write(self.directory / (label + '.stderr.txt'), result.stderr)
        assert 'WARNING: DATA RACE' not in result.stderr, result.stderr
        assert (result.returncode == 0) == success, (result.returncode, result.stdout, result.stderr)
        return result

    def chat(self, prompt='Exercise the fixture.', flags=(), **kwargs):
        return self.run(['--workspace', str(self.workspace), '--prompt', prompt, *flags], **kwargs)

    def audit(self):
        tasks = json.loads(self.run(['tasks', '--json'], label='tasks').stdout)
        records = []
        for task in tasks:
            record = json.loads(self.run(['task', 'show', task['id'], '--json'], label='task-' + task['id']).stdout)
            events = self.run(['task', 'events', task['id'], '--json'], label='events-' + task['id']).stdout
            record['events'] = [json.loads(line) for line in events.splitlines() if line.strip()]
            records.append(record)
        dump(self.directory / 'task-records.json', records)
        return records

    def complete(self):
        assert not self.errors, self.errors
        assert not self.steps, ('provider steps not exercised', self.steps)

    def wait_requests(self, count, timeout=15):
        with self.condition:
            assert self.condition.wait_for(lambda: len(self.requests) >= count or self.errors, timeout), ('missing provider request', count, self.requests)
        assert not self.errors, self.errors


class Interactive:
    """A line-mode or PTY client with bounded transcript capture and cleanup."""
    def __init__(self, case, terminal=False, flags=()):
        self.case, self.terminal, self.transcript = case, terminal, bytearray()
        args = [str(case.binary), '--workspace', str(case.workspace), '--prompt', 'Initial controlled request.', *flags]
        if terminal:
            self.master, slave = pty.openpty()
            fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack('HHHH', 45, 150, 0, 0))
            self.process = subprocess.Popen(args, stdin=slave, stdout=slave, stderr=slave, cwd=case.workspace, env=dict(case.env, TERM='xterm-256color'))
            os.close(slave)
            self.reader = self.master
        else:
            self.master = None
            self.process = subprocess.Popen(args, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                            cwd=case.workspace, env=case.env)
            self.reader = self.process.stdout.fileno()
        case.processes.append(self.process)

    def __enter__(self):
        return self

    def __exit__(self, *_args):
        self.case.release.set()
        if self.process.poll() is None:
            self.process.terminate()
        try:
            self.process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            self.process.kill()
            self.process.wait(timeout=5)
        self.read()
        if self.master is not None:
            os.close(self.master)
        write(self.case.directory / 'interactive-transcript.txt', bytes(self.transcript))

    def send(self, message):
        data = message.encode() + (b'\r' if self.terminal else b'\n')
        if self.terminal:
            os.write(self.master, data)
        else:
            self.process.stdin.write(data)
            self.process.stdin.flush()

    def read(self, timeout=0):
        if select.select([self.reader], [], [], timeout)[0]:
            try:
                self.transcript.extend(os.read(self.reader, 65536))
            except OSError as error:
                if error.errno != errno.EIO:
                    raise
        assert len(self.transcript) < 16 << 20, 'unbounded terminal output'

    def wait(self, marker, timeout=15, after=0):
        target, deadline = marker.encode(), time.monotonic() + timeout
        while target not in self.transcript[after:]:
            assert time.monotonic() < deadline, ('missing terminal marker', marker, self.transcript.decode(errors='replace'))
            assert self.process.poll() is None, ('application exited', self.transcript.decode(errors='replace'))
            self.read(0.05)
        assert b'WARNING: DATA RACE' not in self.transcript

    def finish(self):
        if self.terminal:
            os.write(self.master, b'\x03')
        else:
            self.process.stdin.close()
        deadline = time.monotonic() + 10
        while self.process.poll() is None:
            assert time.monotonic() < deadline, 'application did not shut down'
            self.read(0.05)
        self.read()
        assert self.process.returncode == 0, (self.process.returncode, self.transcript.decode(errors='replace'))


def context_scope(binary, output):
    with Case(binary, output, 'context-scope') as case:
        write(case.workspace / 'AGENTS.md', 'ROOT_RULE_MARKER\n')
        write(case.workspace / 'src/AGENTS.md', 'NESTED_RULE_MARKER\n')
        write(case.workspace / 'other/AGENTS.md', 'SIBLING_RULE_MARKER\n')
        write(case.workspace / 'src/item.txt', 'item\n')
        result = case.run(['context', '--workspace', str(case.workspace), '--path', 'src/item.txt', '--json'])
        value = json.loads(result.stdout)
        encoded = text(value)
        assert 'ROOT_RULE_MARKER' in encoded and 'NESTED_RULE_MARKER' in encoded, value
        assert 'SIBLING_RULE_MARKER' not in encoded, value
        assert 'digest' in encoded or 'sha256' in encoded, value
        assert not case.requests


def context_rejects(binary, output):
    for name, contents in [('binary', b'\x00binary'), ('invalid-utf8', b'\xff'), ('oversized', b'x' * (33 << 10))]:
        with Case(binary, output, 'context-' + name) as case:
            write(case.workspace / 'AGENTS.md', contents)
            result = case.run(['context', '--workspace', str(case.workspace), '--json'], success=False)
            assert result.stderr, name
            assert not case.requests
    with Case(binary, output, 'context-symlink') as case:
        outside = write(case.directory / 'outside.txt', 'OUTSIDE_SECRET_MARKER')
        (case.workspace / 'AGENTS.md').symlink_to(outside)
        result = case.run(['context', '--workspace', str(case.workspace), '--json'], success=False)
        assert 'OUTSIDE_SECRET_MARKER' not in result.stdout + result.stderr


def plan_denials(binary, output):
    steps = [{'calls': [edit('forbidden.txt', 'bad'),
                         ('apply_patch', {'patch': None, 'changes': [{'path': 'patch.txt', 'old_str': '', 'new_str': 'bad'}]}),
                         command('effect.py'), ('verify', {'preset': 'test'}), ('undo_last_change', {})]}, {}]
    with Case(binary, output, 'plan-denials', steps) as case:
        write(case.workspace / 'effect.py', 'from pathlib import Path\nPath("command.txt").write_text("bad")\n')
        write(case.workspace / 'Makefile', 'test:\n\tpython3 effect.py\n')
        case.chat(flags=['--mode', 'plan', '--auto-approve', '--permissions', 'workspace-edit'])
        assert not any((case.workspace / name).exists() for name in ('forbidden.txt', 'patch.txt', 'command.txt'))
        assert len(case.requests) == 2, case.requests
        outputs = [item for item in case.requests[1].get('input', []) if item.get('type') == 'function_call_output']
        assert len(outputs) == 5, outputs
        assert all('plan' in text(item).lower() for item in outputs), outputs
        case.audit()
        case.complete()


def plan_metadata(binary, output):
    steps = [{'calls': [('read_file', {'path': 'input.txt', 'offset': None, 'limit': None}),
                         ('update_plan', {'steps': ['Inspect fixture', 'Propose change']}),
                         ('save_summary', {'summary': 'Metadata is allowed in Plan.'})]}, {}]
    with Case(binary, output, 'plan-metadata', steps) as case:
        write(case.workspace / 'input.txt', 'READ_ONLY_FIXTURE\n')
        case.chat(flags=['--mode', 'plan'])
        assert sorted(str(path.relative_to(case.workspace)) for path in case.workspace.rglob('*')) == ['input.txt']
        assert 'READ_ONLY_FIXTURE' in text(case.requests[1]), case.requests[1]
        records = case.audit()
        assert 'Propose change' in text(records) and 'Metadata is allowed in Plan.' in text(records), records
        case.complete()


def plan_no_mcp_start(binary, output):
    with Case(binary, output, 'plan-no-mcp-start', [{}]) as case:
        marker = case.directory / 'mcp-started.txt'
        script = write(case.directory / 'mcp.py', 'from pathlib import Path\nPath(' + repr(str(marker)) + ').write_text("started")\n')
        dump(case.state / 'mcp.json', {'mcpServers': {'fixture': {'command': sys.executable, 'args': [str(script)]}}})
        (case.state / 'mcp.json').chmod(0o600)
        case.chat(flags=['--mode', 'plan'])
        assert not marker.exists(), 'Plan initiated MCP process'
        case.complete()


def permission_profile(binary, output):
    steps = [{'calls': [edit('approved-file.txt', 'workspace edit\n'), command('effect.py')]}, {}]
    with Case(binary, output, 'workspace-edit-policy', steps) as case:
        write(case.workspace / 'effect.py', 'from pathlib import Path\nPath("command.txt").write_text("bad")\n')
        case.chat(flags=['--permissions', 'workspace-edit'], stdin='n\n')
        assert (case.workspace / 'approved-file.txt').read_text() == 'workspace edit\n'
        assert not (case.workspace / 'command.txt').exists(), 'workspace-edit authorized project execution'
        case.audit()
        case.complete()


def plan_mcp_server(binary, output):
    with Case(binary, output, 'plan-mcp-server') as case:
        write(case.workspace / 'input.txt', 'MCP_PLAN_READ_MARKER\n')
        write(case.workspace / 'effect.py', 'from pathlib import Path\nPath("effect.txt").write_text("bad")\n')
        process = subprocess.Popen([str(binary), 'mcp', 'serve', '--workspace', str(case.workspace), '--mode', 'plan',
                                    '--permissions', 'workspace-edit', '--auto-approve'], stdin=subprocess.PIPE,
                                   stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=case.env, cwd=case.workspace)
        case.processes.append(process)
        transcript, buffer, serial = [], bytearray(), 0

        def send(value):
            transcript.append({'direction': 'client', 'message': value})
            process.stdin.write((text(dict(jsonrpc='2.0', **value)) + '\n').encode())
            process.stdin.flush()

        def request(method, params):
            nonlocal serial
            serial += 1
            send({'id': serial, 'method': method, 'params': params})
            deadline = time.monotonic() + 10
            while True:
                while b'\n' not in buffer:
                    assert time.monotonic() < deadline, ('MCP response timed out', method, transcript)
                    if select.select([process.stdout], [], [], 0.05)[0]:
                        chunk = os.read(process.stdout.fileno(), 65536)
                        assert chunk, ('MCP server exited', transcript)
                        buffer.extend(chunk)
                raw, _, rest = buffer.partition(b'\n')
                buffer[:] = rest
                value = json.loads(raw)
                transcript.append({'direction': 'server', 'message': value})
                assert value.get('method') != 'elicitation/create', 'Plan must deny before asking execution approval'
                if value.get('id') == serial:
                    return value

        try:
            response = request('initialize', {'protocolVersion': '2025-11-25', 'clientInfo': {'name': 'capability-fixture', 'version': '1'}, 'capabilities': {}})
            assert 'result' in response, response
            send({'method': 'notifications/initialized'})
            catalog = request('tools/list', {})['result']['tools']
            assert not {'start_process', 'process_status', 'wait_process', 'stop_process'} & {tool['name'] for tool in catalog}, catalog
            response = request('tools/call', {'name': 'read_file', 'arguments': {'path': 'input.txt', 'offset': None, 'limit': None}})
            assert 'MCP_PLAN_READ_MARKER' in text(response) and not response['result'].get('isError'), response
            for name, arguments in (edit('effect.txt', 'bad'), command('effect.py'), ('verify', {'preset': 'test'})):
                response = request('tools/call', {'name': name, 'arguments': arguments})
                assert response['result'].get('isError') and 'plan' in text(response).lower(), response
            assert not (case.workspace / 'effect.txt').exists()
            process.stdin.close()
            process.wait(timeout=10)
            stderr = process.stderr.read().decode()
            write(case.directory / 'mcp.stderr.txt', stderr)
            assert process.returncode == 0 and 'WARNING: DATA RACE' not in stderr, stderr
            assert not case.requests
        finally:
            dump(case.directory / 'mcp-wire.json', transcript)


def references(binary, output):
    with Case(binary, output, 'explicit-references', [{}]) as case:
        write(case.workspace / 'unicodé/你好 file.txt', 'FIRST_NOT_SELECTED\nSELECTED_UNICODE_你好\nLAST_NOT_SELECTED\n')
        case.chat('Inspect @"unicodé/你好 file.txt":2-2. Email a@b.test and literal \\@missing.txt are prose.')
        request = text(case.requests[0])
        assert 'SELECTED_UNICODE_你好' in request, request
        assert 'FIRST_NOT_SELECTED' not in request and 'LAST_NOT_SELECTED' not in request, request
        records = case.audit()
        assert 'unicodé/你好 file.txt' in text(records), records
        case.complete()
    for name, reference in [('missing', '@missing.txt'), ('traversal', '@../outside.txt'), ('range', '@sample.txt:20-30')]:
        with Case(binary, output, 'reference-' + name) as case:
            write(case.directory / 'outside.txt', 'OUTSIDE_SECRET_MARKER')
            write(case.workspace / 'sample.txt', 'one\ntwo\n')
            result = case.chat('Read ' + reference, success=False)
            assert not case.requests, ('invalid reference reached provider', case.requests)
            assert 'OUTSIDE_SECRET_MARKER' not in result.stdout + result.stderr


def root_instructions(binary, output):
    with Case(binary, output, 'root-instructions', [{}]) as case:
        write(case.workspace / 'AGENTS.md', 'ROOT_MODEL_INSTRUCTION_7931\n')
        case.chat()
        assert 'ROOT_MODEL_INSTRUCTION_7931' in case.requests[0].get('instructions', ''), case.requests[0]
        case.complete()


def reference_resume(binary, output):
    with Case(binary, output, 'reference-resume', [{}]) as case:
        body = ('bounded line ' + 'x' * 900 + '\n') * 24 + 'ORIGINAL_ATTACHMENT_TAIL_MARKER\n'
        assert len(body) > 16 << 10
        write(case.workspace / 'large.txt', body)
        case.chat('Inspect @large.txt')
        assert 'ORIGINAL_ATTACHMENT_TAIL_MARKER' in text(case.requests[0]), 'attachment truncated at session message limit'
        records = case.audit()
        assert len(records) == 1, records
        task_id = records[0]['task']['id']
        write(case.workspace / 'large.txt', 'CURRENT_FILE_DIFFERENT_MARKER\n')
        case.steps.append({})
        case.run(['task', 'resume', task_id, '--prompt', 'Compare the original submitted attachment.'], label='resume')
        assert len(case.requests) == 2, case.requests
        request = text(case.requests[1])
        assert 'ORIGINAL_ATTACHMENT_TAIL_MARKER' in request, 'resume lost historical attachment'
        assert 'CURRENT_FILE_DIFFERENT_MARKER' not in request, 'resume silently replaced submitted snapshot'
        records = case.audit()
        assert len(records) == 1 and len(records[0]['runs']) == 2, records
        case.complete()


def nested_retry(binary, output):
    def verify_no_effect(case, _request):
        assert not (case.workspace / 'src/new.txt').exists(), 'new rule did not block original edit'
        assert not (case.workspace / 'other.txt').exists(), 'old batch continued after newly discovered rule'
        return {'calls': [edit('src/new.txt', 'fresh operation\n')]}

    steps = [{'calls': [edit('src/new.txt', 'stale operation\n'), edit('other.txt', 'stale batch\n')]}, verify_no_effect, {}]
    with Case(binary, output, 'nested-retry', steps) as case:
        write(case.workspace / 'src/AGENTS.md', 'NESTED_FRESH_INSTRUCTION\n')
        case.chat(flags=['--permissions', 'workspace-edit'])
        assert (case.workspace / 'src/new.txt').read_text() == 'fresh operation\n'
        assert not (case.workspace / 'other.txt').exists()
        assert 'NESTED_FRESH_INSTRUCTION' in text(case.requests[1]), case.requests[1]
        case.complete()


def long_command(binary, output):
    steps = [{'calls': [command('long.py', timeout=140)]}, {}]
    with Case(binary, output, 'long-command', steps) as case:
        write(case.workspace / 'long.py', 'import time\nprint("LONG_STARTED", flush=True)\ntime.sleep(121)\nprint("LONG_FINISHED", flush=True)\n')
        started = time.monotonic()
        case.chat(flags=['--auto-approve'], timeout=155)
        elapsed = time.monotonic() - started
        assert elapsed >= 120, elapsed
        assert 'LONG_FINISHED' in text(case.requests[1]), case.requests[1]
        records = case.audit()
        dump(case.directory / 'duration.json', {'elapsed_seconds': elapsed})
        assert 'succeeded' in text(records), records
        case.complete()


def control_stop(binary, output, terminal=False):
    name = 'stop-' + ('pty' if terminal else 'line')
    with Case(binary, output, name, [{'stream_wait': True}, {}]) as case:
        with Interactive(case, terminal) as client:
            client.wait('CAPABILITY_PARTIAL_STREAM')
            case.wait_requests(1)
            client.send('/stop')
            assert case.cancelled.wait(10), 'stop did not cancel active inference'
            client.send('FOLLOWUP_AFTER_STOP')
            case.wait_requests(2)
            client.wait('CAPABILITY_COMPLETE')
            assert 'FOLLOWUP_AFTER_STOP' in text(case.requests[1]), case.requests[1]
            client.finish()
        case.audit()
        case.complete()

def control_steer(binary, output, terminal=False):
    name = 'steer-' + ('pty' if terminal else 'line')
    with Case(binary, output, name, [{'stream_wait': True, 'calls': [edit('stale.txt', 'bad')]}, {}]) as case:
        with Interactive(case, terminal, ['--permissions', 'workspace-edit']) as client:
            client.wait('CAPABILITY_PARTIAL_STREAM')
            case.wait_requests(1)
            client.send('/steer CORRECTION_DURING_INFERENCE')
            assert case.cancelled.wait(10), 'steering did not cancel active inference'
            case.wait_requests(2)
            client.wait('CAPABILITY_COMPLETE')
            assert 'CORRECTION_DURING_INFERENCE' in text(case.requests[1]), case.requests[1]
            assert not (case.workspace / 'stale.txt').exists()
            client.finish()
        records = case.audit()
        assert 'CORRECTION_DURING_INFERENCE' in text(records), records
        case.complete()

def control_modes(binary, output, terminal=False):
    steps = [{'text': 'INITIAL_MODE_READY'}, {'calls': [edit('mode.txt', 'bad')]}, {'text': 'PLAN_MODE_DONE'},
             {'calls': [edit('mode.txt', 'build write\n')]}, {'text': 'BUILD_MODE_DONE'}]
    name = 'modes-' + ('pty' if terminal else 'line')
    with Case(binary, output, name, steps) as case:
        with Interactive(case, terminal, ['--permissions', 'workspace-edit']) as client:
            client.wait('INITIAL_MODE_READY')
            client.send('/plan')
            client.wait('Mode: plan')
            client.send('PLAN_WORK_REQUEST')
            client.wait('PLAN_MODE_DONE')
            assert not (case.workspace / 'mode.txt').exists(), 'Plan command allowed workspace mutation'
            client.send('/build')
            client.wait('Mode: build')
            client.send('BUILD_WORK_REQUEST')
            client.wait('BUILD_MODE_DONE')
            assert (case.workspace / 'mode.txt').read_text() == 'build write\n'
            client.finish()
        case.audit()
        case.complete()


def control_queue(binary, output, terminal=False):
    name = 'queue-' + ('pty' if terminal else 'line')
    with Case(binary, output, name, [{'stream_wait': True}, {}, {'text': 'QUEUE_TWICE_DONE'}]) as case:
        with Interactive(case, terminal) as client:
            client.wait('CAPABILITY_PARTIAL_STREAM')
            case.wait_requests(1)
            client.send('/queue SAME_QUEUED_REQUEST')
            client.wait('Queued')
            previous = len(client.transcript)
            client.send('/queue SAME_QUEUED_REQUEST')
            client.wait('Queued', after=previous)
            records = case.audit()
            received = [event['data'] for record in records for event in record['events'] if event['kind'] == 'control.received']
            assert len(received) == 2 and received[0]['id'] != received[1]['id'], received
            assert all(event['text'] == 'SAME_QUEUED_REQUEST' for event in received), received
            assert len(case.requests) == 1, 'queued input interrupted active inference'
            case.release.set()
            case.wait_requests(3)
            client.wait('QUEUE_TWICE_DONE')
            client.finish()
        records = case.audit()
        applied = [event['data']['id'] for record in records for event in record['events'] if event['kind'] == 'control.applied']
        assert sorted(applied) == sorted(event['id'] for event in received), (received, applied)
        assert all('SAME_QUEUED_REQUEST' in text(request) for request in case.requests[1:])
        case.complete()


def approval_correction(binary, output):
    with Case(binary, output, 'approval-correction', [{'calls': [edit('old-operation.txt', 'must not write')]}, {}]) as case:
        with Interactive(case, terminal=True) as client:
            client.wait('Approval required')
            os.write(client.master, b'\x07')
            client.wait('Enter a correction')
            client.send('CORRECTION_DURING_APPROVAL')
            client.wait('CAPABILITY_COMPLETE')
            assert len(case.requests) == 2, case.requests
            assert 'CORRECTION_DURING_APPROVAL' in text(case.requests[1]), case.requests[1]
            assert not (case.workspace / 'old-operation.txt').exists(), 'superseded approval admitted its edit'
            client.finish()
        case.audit()
        case.complete()


def queue_recovery(binary, output):
    with Case(binary, output, 'queue-recovery', [{'stream_wait': True}, {}, {}]) as case:
        write(case.workspace / 'queued.txt', 'ACCEPTED_QUEUE_SNAPSHOT\n')
        with Interactive(case) as client:
            client.wait('CAPABILITY_PARTIAL_STREAM')
            case.wait_requests(1)
            client.send('/queue Read @queued.txt and apply QUEUED_AFTER_CRASH.')
            client.wait('Queued')
            records = case.audit()
            task_id = records[0]['task']['id']
            received = [event['data'] for event in records[0]['events'] if event['kind'] == 'control.received']
            assert len(received) == 1, records
            client.process.kill()
            client.process.wait(timeout=5)
        write(case.workspace / 'queued.txt', 'CHANGED_AFTER_ACCEPTANCE\n')
        records = case.audit()
        assert len(case.requests) == 1, 'inspection resumed queued input'
        assert not [event for event in records[0]['events'] if event['kind'] == 'control.applied']
        case.run(['task', 'resume', task_id, '--prompt', 'Inspect current task only.'], label='ordinary-resume')
        records = case.audit()
        assert not [event for event in records[0]['events'] if event['kind'] == 'control.applied'], 'ordinary resume automatically applied recovered queue'
        case.run(['task', 'resume', task_id, '--prompt', '/continue-queue'], label='continue-queue')
        assert len(case.requests) == 3, case.requests
        assert 'QUEUED_AFTER_CRASH' in text(case.requests[2]), case.requests[2]
        assert 'ACCEPTED_QUEUE_SNAPSHOT' in text(case.requests[2]) and 'CHANGED_AFTER_ACCEPTANCE' not in text(case.requests[2]), 'queue attachment was reread after acceptance'
        records = case.audit()
        applied = [event['data']['id'] for event in records[0]['events'] if event['kind'] == 'control.applied']
        assert applied == [received[0]['id']], (received, applied)
        case.run(['task', 'resume', task_id, '--prompt', '/continue-queue'], label='continue-queue-again')
        assert len(case.requests) == 3, 'recovered request replayed twice'
        case.complete()

def managed_process(binary, output):
    def inspect_started(case, request):
        outputs = [item.get('output', '') for item in request.get('input', []) if item.get('type') == 'function_call_output']
        candidates = []
        for value in outputs:
            try:
                parsed = json.loads(value)
            except (ValueError, TypeError):
                continue
            if isinstance(parsed, dict) and isinstance(parsed.get('process'), dict):
                candidates.append(parsed)
        assert candidates, ('missing managed process handle', outputs)
        process = candidates[-1]['process']
        assert process['state'] == 'running', process
        case.managed_id = process['id']
        return {'calls': [edit('concurrent.txt', 'must be rejected'),
                          ('process_status', {'process_id': case.managed_id, 'cursor': 0})]}

    def release_process(case, _request):
        assert not (case.workspace / 'concurrent.txt').exists(), 'edit ran alongside writing process'
        write(case.workspace / 'release-managed', 'release\n')
        return {'calls': [('wait_process', {'process_id': case.managed_id, 'cursor': 0, 'wait_ms': 5000})]}

    def inspect_finished(case, request):
        assert not (case.workspace / 'concurrent.txt').exists(), 'edit ran alongside writing process'
        assert 'MANAGED_FINISHED' in text(request), request
        return {'calls': [edit('after-process.txt', 'safe after exit\n')]}

    steps = [{'calls': [('start_process', {'command': 'python3', 'args': ['managed.py'], 'timeout': 20})]},
             inspect_started, release_process, inspect_finished, {}]
    with Case(binary, output, 'managed-process', steps) as case:
        write(case.workspace / 'managed.py', 'import time\nfrom pathlib import Path\nprint("MANAGED_STARTED", flush=True)\nwhile not Path("release-managed").exists():\n    time.sleep(0.01)\nprint("MANAGED_FINISHED", flush=True)\n')
        case.chat(flags=['--auto-approve'], timeout=30)
        assert (case.workspace / 'after-process.txt').read_text() == 'safe after exit\n'
        records = case.audit()
        assert 'exited' in text(records) and case.managed_id in text(records), records
        case.complete()


def managed_stop(binary, output):
    def stop_started(case, request):
        output = next(item['output'] for item in request['input'] if item.get('type') == 'function_call_output')
        case.managed_id = json.loads(output)['process']['id']
        deadline = time.monotonic() + 5
        while not (case.workspace / 'pid.txt').exists():
            assert time.monotonic() < deadline, 'managed child did not reach readiness barrier'
            time.sleep(0.01)
        case.child_pid = int((case.workspace / 'pid.txt').read_text())
        return {'calls': [('stop_process', {'process_id': case.managed_id, 'cursor': 0})]}

    def check_stop(case, request):
        outputs = [item['output'] for item in request['input'] if item.get('type') == 'function_call_output']
        latest = json.loads(outputs[-1])['process']
        assert latest['state'] == 'stopped' and latest['effects'] == 'unknown', latest
        return {'calls': [edit('must-not-write.txt', 'blocked until reconciliation')]}

    steps = [{'calls': [('start_process', {'command': 'python3', 'args': ['forever.py'], 'timeout': 30})]}, stop_started, check_stop, {}]
    with Case(binary, output, 'managed-stop', steps) as case:
        write(case.workspace / 'forever.py', 'import os, time\nfrom pathlib import Path\nwith Path("starts.txt").open("a") as f: f.write("started\\n")\nPath("pid.txt").write_text(str(os.getpid()))\nwhile True: time.sleep(0.05)\n')
        case.chat(flags=['--auto-approve'], success=False)
        assert not (case.workspace / 'must-not-write.txt').exists()
        try:
            os.kill(case.child_pid, 0)
        except ProcessLookupError:
            pass
        else:
            raise AssertionError('owned process survived stopped Run')
        records = case.audit()
        task_id = records[0]['task']['id']
        process = records[0]['processes'][0]
        assert process['state'] == 'stopped' and process['effects'] == 'unknown', process
        before = len(case.requests)
        case.run(['task', 'resume', task_id], success=False, label='blocked-resume')
        assert len(case.requests) == before, 'unresolved process reached model'
        case.run(['task', 'resolve-process', task_id, case.managed_id, '--reason', 'Fixture inspected pid and files.'], label='resolve-process')
        case.steps.append({})
        case.run(['task', 'resume', task_id, '--prompt', 'Continue after inspected reconciliation.'], label='resolved-resume')
        assert (case.workspace / 'starts.txt').read_text() == 'started\n', 'unknown process restarted automatically'
        case.complete()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--only', action='append', help='Run a named scenario group; may be repeated.')
    parser.add_argument('--long-command', action='store_true', help='Include the real 121-second execution check.')
    parser.add_argument('--controls', action='store_true', help='Include line-mode and real PTY stop/steer/mode-switch checks.')
    parser.add_argument('--processes', action='store_true', help='Include Run-scoped process start/read/wait and ownership checks.')
    args = parser.parse_args()
    binary, output = args.binary.resolve(), args.output.resolve()
    output.mkdir(parents=True, exist_ok=False)
    scenarios = {'context-scope': context_scope, 'context-rejects': context_rejects, 'root-instructions': root_instructions,
                 'plan-denials': plan_denials, 'plan-metadata': plan_metadata, 'plan-no-mcp-start': plan_no_mcp_start,
                 'plan-mcp-server': plan_mcp_server,
                 'workspace-edit-policy': permission_profile, 'explicit-references': references,
                 'reference-resume': reference_resume, 'nested-retry': nested_retry}
    if args.long_command:
        scenarios['long-command'] = long_command
    if args.controls:
        for label, scenario in [('stop', control_stop), ('steer', control_steer), ('modes', control_modes), ('queue', control_queue)]:
            for terminal in (False, True):
                scenarios['control-' + label + ('-pty' if terminal else '-line')] = lambda binary, output, f=scenario, t=terminal: f(binary, output, t)
        scenarios['queue-recovery'] = queue_recovery
        scenarios['approval-correction'] = approval_correction
    if args.processes:
        scenarios['managed-process'] = managed_process
        scenarios['managed-stop'] = managed_stop
    if args.only:
        unknown = set(args.only) - scenarios.keys()
        if unknown:
            parser.error('unknown scenario groups: ' + ', '.join(sorted(unknown)))
        scenarios = {name: scenarios[name] for name in args.only}
    report = {'passed': False, 'live_model': False, 'platform': platform.platform(),
              'binary_sha256': hashlib.sha256(binary.read_bytes()).hexdigest(), 'scenarios': []}
    for name, scenario in scenarios.items():
        started = time.monotonic()
        result = {'name': name, 'passed': False}
        try:
            scenario(binary, output)
            result['passed'] = True
        except Exception:
            result['error'] = traceback.format_exc()
        result['elapsed_seconds'] = round(time.monotonic() - started, 3)
        report['scenarios'].append(result)
        dump(output / 'report.json', report)
        print(('PASS ' if result['passed'] else 'FAIL ') + name, flush=True)
        if not result['passed']:
            print(result['error'], file=sys.stderr, flush=True)
    report['passed'] = all(result['passed'] for result in report['scenarios'])
    dump(output / 'report.json', report)
    dump(output / 'checksums.json', {str(path.relative_to(output)): hashlib.sha256(path.read_bytes()).hexdigest()
                                   for path in sorted(output.rglob('*')) if path.is_file()})
    raise SystemExit(0 if report['passed'] else 1)


if __name__ == '__main__':
    main()
