#!/usr/bin/env python3
"""Real-binary MCP E2E: python3 scripts/mcp-e2e.py --binary dist/meldra."""
import argparse
import hashlib
import http.server
import json
import os
from pathlib import Path
import socket
import select
import pty
import fcntl
import termios
import struct
import subprocess
import sys
import threading
import time
import traceback


def save(path, value):
    path.write_text(json.dumps(value, ensure_ascii=False, indent=2) + '\n')


def record(path, value):
    with path.open('a') as out:
        out.write(json.dumps(value) + '\n')


def messages(path):
    return [json.loads(line) for line in path.read_text().splitlines()] if path.exists() else []


def mcp_reply(request, log):
    record(log, request)
    method = request.get('method')
    if 'id' not in request:
        return None
    result = {}
    if method == 'server/discover':
        if log.parent.name == 'legacy-init-only':
            raise ConnectionAbortedError('legacy server requires initialize first')
        return {'jsonrpc': '2.0', 'id': request['id'], 'error': {'code': -32601, 'message': 'legacy fixture'}}
    if method == 'initialize':
        capability = 'resources' if log.parent.name.startswith('resources-') else ('prompts' if log.parent.name.startswith('prompts-') else 'tools')
        result = {'protocolVersion': '2025-03-26', 'capabilities': {capability: {}}, 'serverInfo': {'name': 'meldra-e2e', 'version': '1'}}
    elif method == 'resources/list':
        second = request.get('params', {}).get('cursor') == 'second'
        result = {'resources': [{'uri': 'fixture://second' if second else 'fixture://first', 'name': 'fixture resource'}]}
        if not second:
            result['nextCursor'] = 'second'
    elif method == 'resources/templates/list':
        result = {'resourceTemplates': [{'uriTemplate': 'fixture://{name}', 'name': 'fixture template'}]}
    elif method == 'resources/read':
        result = {'contents': [{'uri': request['params']['uri'], 'text': 'resource-content-evidence'}, {'uri': 'fixture://blob', 'blob': 'YmluYXJ5', 'mimeType': 'application/octet-stream'}]}
    elif method == 'prompts/list':
        result = {'prompts': [{'name': 'review', 'arguments': [{'name': 'focus', 'required': True}]}]}
    elif method == 'prompts/get':
        result = {'messages': [{'role': 'user', 'content': {'type': 'text', 'text': 'prompt-content-evidence: ' + request['params']['arguments']['focus']}}]}
    elif method == 'tools/list':
        second = request.get('params', {}).get('cursor') == 'second'
        result = {'tools': [{'name': 'second' if second else 'echo', 'description': 'E2E tool', 'inputSchema': {'type': 'object', 'properties': {'mode': {'type': 'string'}, 'optional': {'type': 'string'}}, 'required': ['mode']}}]}
        if log.parent.name == 'catalog-count':
            result['tools'] = [dict(result['tools'][0], name='echo' if i == 0 else 'tool_' + str(i)) for i in range(60)]
        elif log.parent.name == 'catalog-bytes':
            result['tools'][0]['description'] = 'x' * 600000
        elif log.parent.name == 'catalog-escaped-bytes':
            result['tools'][0]['description'] = '\x01' * 100000
        elif not second:
            result['nextCursor'] = 'second'
    elif method == 'tools/call':
        mode = request.get('params', {}).get('arguments', {}).get('mode', 'ok')
        if mode == 'drop':
            raise ConnectionAbortedError('deliberate post-dispatch loss')
        if mode in ('timeout', 'signal'):
            time.sleep(3)
        result = {'content': [{'type': 'text', 'text': 'fixture-error-evidence' if mode == 'error' else ('large-evidence:' + 'x' * 600000 if mode == 'large' else 'fixture-success-evidence')}], 'isError': mode == 'error'}
    if method == 'tools/call' and mode == 'structured':
        result['structuredContent'] = {'nested': {'verified': True, 'number': 42}}
    return {'jsonrpc': '2.0', 'id': request['id'], 'result': result}


def stdio_fixture(log):
    record(log, {'started_pid': os.getpid(), 'environment': {key: os.environ.get(key) for key in ('OPENAI_API_KEY', 'E2E_UNRELATED_SECRET', 'E2E_ALLOWED', 'E2E_OVERRIDE')}})
    if os.environ.get('E2E_SPAWN_CHILD') == '1':
        child = subprocess.Popen([sys.executable, '-c', 'import time; time.sleep(60)'])
        record(log, {'started_pid': child.pid, 'descendant': True})
    if log.stem == 'slow':
        time.sleep(5)
    for line in sys.stdin:
        try:
            response = mcp_reply(json.loads(line), log)
            if response is not None:
                print(json.dumps(response), flush=True)
        except (ConnectionAbortedError, BrokenPipeError):
            return


class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_GET(self):
        self.send_error(405)

    def do_DELETE(self):
        self.send_response(200)
        self.end_headers()

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        try:
            if self.path == '/mcp':
                if self.server.directory.name == 'http-slow-startup' and body.get('method') == 'initialize':
                    time.sleep(2)
                self.server.authorization.append(self.headers.get('Authorization'))
                response = mcp_reply(body, self.server.mcp_log)
            else:
                self.server.requests.append(body)
                save(self.server.directory / 'provider-requests.json', self.server.requests)
                if body.get('stream'):
                    self.send_response(400)
                    self.send_header('Content-Type', 'application/json')
                    self.end_headers()
                    self.wfile.write(b'{"error":{"message":"stream is not supported","type":"invalid_request_error","code":"unsupported_parameter","param":"stream"}}')
                    return
                index = self.server.response_index
                self.server.response_index += 1
                tools = [t for t in body.get('tools', []) if t.get('name', '').startswith('mcp_')]
                self.server.advertised = tools
                output = []
                if index < len(self.server.steps):
                    server, remote, mode, call_id = self.server.steps[index]
                    matches = [t for t in tools if (t['name'] == 'mcp_' + remote + '__' + server if remote in ('resources', 'prompts') else t['name'].startswith('mcp__' + server + '__') and t['name'].endswith('__' + remote))]
                    if len(matches) != 1:
                        self.server.errors.append('expected discovered tool %s/%s: %s' % (server, remote, [t['name'] for t in tools]))
                    else:
                        output = [{'type': 'function_call', 'id': 'fc_' + str(index), 'call_id': call_id, 'name': matches[0]['name'], 'arguments': json.dumps(mode if isinstance(mode, dict) else {'mode': 17 if mode == 'invalid-args' else mode}), 'status': 'completed'}]
                if not output:
                    output = [{'type': 'message', 'id': 'msg_' + str(index), 'role': 'assistant', 'status': 'completed', 'content': [{'type': 'output_text', 'text': 'E2E complete', 'annotations': []}]}]
                response = {'id': 'resp_' + str(index), 'object': 'response', 'created_at': 1, 'model': 'fixture', 'status': 'completed', 'output': output}
            self.send_response(202 if response is None else 200)
            self.send_header('Content-Type', 'application/json')
            self.end_headers()
            if response is not None:
                self.wfile.write(json.dumps(response).encode())
        except ConnectionAbortedError:
            self.connection.shutdown(socket.SHUT_RDWR)
            self.connection.close()
        except (BrokenPipeError, ConnectionResetError):
            pass
        except Exception:
            self.server.errors.append(traceback.format_exc())
            self.send_error(500)



def run_terminal(command, env, decision, server):
    master, slave = pty.openpty()
    fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack('HHHH', 40, 120, 0, 0))
    env['TERM'] = 'xterm-256color'
    process = subprocess.Popen(command, stdin=slave, stdout=slave, stderr=slave, env=env)
    os.close(slave)
    output = bytearray()
    answered = stopped = False
    deadline = time.monotonic() + 20
    try:
        while time.monotonic() < deadline:
            if select.select([master], [], [], 0.05)[0]:
                try:
                    chunk = os.read(master, 65536)
                except OSError:
                    break
                if not chunk:
                    break
                output.extend(chunk)
            if not answered and b'approve' in output.lower() and b'MCP' in output:
                os.write(master, decision.encode())
                answered = True
            if not stopped and server.response_index >= 2 and b'E2E complete' in output:
                os.write(master, b'\x03')
                stopped = True
            if process.poll() is not None:
                break
        if process.poll() is None:
            process.terminate()
        process.wait(timeout=10)
        assert answered, 'TUI approval never became visible: ' + output.decode(errors='replace')
        return subprocess.CompletedProcess(command, process.returncode, output.decode(errors='replace'), '')
    finally:
        if process.poll() is None:
            process.kill()
            process.wait()
        os.close(master)


def run_case(binary, output, name, mode='ok', approval='auto', two=False, transport='stdio', bad=None, unavailable=False):
    directory = output / name
    directory.mkdir()
    home, workspace = directory / 'home', directory / 'workspace'
    home.mkdir(mode=0o700)
    workspace.mkdir()
    server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Handler)
    server.daemon_threads = True
    server.directory = directory
    server.requests, server.errors, server.advertised, server.authorization = [], [], [], []
    server.response_index = 0
    server.mcp_log = directory / 'http-mcp.jsonl'
    server.steps = [('fixture', 'second' if name == 'pagination' else 'echo', mode, 'call_1')]
    if name.startswith('resources-'):
        server.steps = [('fixture', 'resources', args, 'catalog_' + str(index)) for index, args in enumerate([{'action': 'list'}, {'action': 'list', 'cursor': 'second'}, {'action': 'templates'}, {'action': 'read', 'uri': 'fixture://custom'}])]
    if name.startswith('prompts-'):
        server.steps = [('fixture', 'prompts', args, 'catalog_' + str(index)) for index, args in enumerate([{'action': 'list'}, {'action': 'get', 'name': 'review', 'arguments': {'focus': 'security'}}])]
    if name == 'resources-cancel':
        server.steps = [('fixture', 'resources', {'action': 'list'}, 'catalog_cancel')]
    if name == 'resources-invalid':
        server.steps = [('fixture', 'resources', {'action': 'execute'}, 'catalog_invalid')]
    if name == 'prompts-invalid':
        server.steps = [('fixture', 'prompts', {'action': 'get', 'arguments': {}}, 'catalog_invalid')]
    if mode == 'duplicate':
        server.steps = [('fixture', 'echo', 'ok', 'call_1')] * 2
    if two:
        server.steps.append(('other', 'echo', 'ok', 'call_2'))
    if bad:
        server.steps = []
    threading.Thread(target=server.serve_forever, daemon=True).start()
    url = 'http://127.0.0.1:' + str(server.server_port)
    (home / 'config.toml').write_text('model = "fixture"\nbase_url = "' + url + '/v1"\nallow_insecure_base_url = true\n')
    def stdio(key):
        return {'command': sys.executable, 'args': [str(Path(__file__).resolve()), '--stdio-fixture', str(directory / (key + '.jsonl'))], 'tool_timeout_sec': 1 if mode == 'timeout' else 10}
    config = {'mcpServers': {'fixture': stdio('fixture') if transport == 'stdio' else {'url': url + '/mcp', 'tool_timeout_sec': 1 if mode == 'timeout' else 10}}}
    if name == 'http-slow-startup':
        config['mcpServers']['fixture'].update(startup_timeout_sec=10, tool_timeout_sec=1)
    if name in ('catalog-count', 'catalog-bytes', 'catalog-escaped-bytes'):
        config['mcpServers']['overflow'] = stdio('overflow')
    if two:
        config['mcpServers']['other'] = stdio('other')
    if unavailable:
        config['mcpServers']['missing'] = {'command': '/meldra-e2e/no-such-executable'}
        config['mcpServers']['disabled'] = dict(stdio('disabled'), disabled=True)
    if name in ('startup-timeout', 'descendant-startup'):
        config['mcpServers']['slow'] = dict(stdio('slow'), startup_timeout_sec=1)
    if name.startswith('descendant-'):
        config['mcpServers']['fixture']['env'] = {'E2E_SPAWN_CHILD': '1'}
        if 'slow' in config['mcpServers']:
            config['mcpServers']['slow']['env'] = {'E2E_SPAWN_CHILD': '1'}
    if name == 'environment':
        config['mcpServers']['fixture'].update(env_vars=['E2E_ALLOWED'], env={'E2E_OVERRIDE': 'configured'})
    if name == 'bearer':
        config['mcpServers']['fixture']['bearer_token_env'] = 'E2E_BEARER'
    if bad == 'ambiguous':
        config['mcpServers']['fixture']['url'] = url + '/mcp'
    if bad == 'timeout':
        config['mcpServers']['fixture']['tool_timeout_sec'] = 0
    (home / 'mcp.json').write_text('{' if bad == 'json' else json.dumps(config))
    (home / 'config.toml').chmod(0o600)
    (home / 'mcp.json').chmod(0o600)
    env = {key: value for key, value in os.environ.items() if not key.startswith(('OPENAI_', 'MELDRA_'))}
    env.update(E2E_UNRELATED_SECRET='unrelated-secret-evidence', E2E_ALLOWED='allowed-value', E2E_OVERRIDE='parent-value', E2E_BEARER='bearer-secret-evidence')
    env.update(MELDRA_HOME=str(home), OPENAI_API_KEY='fixture-secret', TERM='dumb')
    command = [str(binary), '--workspace', str(workspace), '--prompt', 'Run the MCP E2E fixture']
    if approval == 'auto':
        command.append('--auto-approve')
    stdin = {'auto': '', 'yes': 'y\n' * len(server.steps), 'no': 'n\n' * len(server.steps), 'eof': '', 'tui-yes': '', 'tui-no': ''}[approval]
    try:
        if approval.startswith('tui-'):
            result = run_terminal(command, env, 'y' if approval == 'tui-yes' else 'n', server)
        elif name == 'resources-cancel':
            process = subprocess.Popen(command, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=env)
            captured = bytearray()
            try:
                deadline = time.monotonic() + 10
                while b'Allow MCP request?' not in captured:
                    assert time.monotonic() < deadline, 'catalog approval did not appear'
                    if select.select([process.stdout], [], [], 0.1)[0]:
                        captured.extend(os.read(process.stdout.fileno(), 65536))
                process.terminate()
                process.wait(timeout=10)
                stdout, stderr = process.communicate(timeout=10)
                result = subprocess.CompletedProcess(command, process.returncode, (captured + stdout).decode(), stderr.decode())
            finally:
                if process.poll() is None:
                    process.kill()
                    process.wait()
        elif mode == 'signal':
            process = subprocess.Popen(command, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, env=env)
            deadline = time.monotonic() + 10
            while time.monotonic() < deadline and not any(m.get('method') == 'tools/call' for m in messages(directory / 'fixture.jsonl')):
                time.sleep(0.02)
            process.terminate()
            stdout, stderr = process.communicate('', timeout=15)
            result = subprocess.CompletedProcess(command, process.returncode, stdout, stderr)
        else:
            result = subprocess.run(command, input=stdin, capture_output=True, text=True, env=env, timeout=30)
        (directory / 'transcript.txt').write_text(result.stdout + result.stderr)
        assert 'WARNING: DATA RACE' not in result.stdout + result.stderr, 'race detected; inspect transcript'
        assert not server.errors, server.errors
        if bad:
            assert result.returncode != 0, 'invalid config accepted'
            assert 'MCP' in result.stderr or 'mcp.json' in result.stderr, result.stderr
            assert not server.requests, 'invalid config reached provider'
            return {'name': name, 'passed': True, 'exit_code': result.returncode}
        calls_on_wire = [m for log in directory.glob('*.jsonl') for m in messages(log) if m.get('method') in ('tools/call', 'resources/list', 'resources/templates/list', 'resources/read', 'prompts/list', 'prompts/get')]
        assert server.advertised, 'MCP tools not advertised'
        for tool in server.advertised:
            assert tool.get('strict') is False, 'MCP schema forced strict'
            assert 'optional' not in tool['parameters'].get('required', []), 'optional schema rewritten'
        listing = subprocess.run([str(binary), 'tasks', '--json'], capture_output=True, text=True, env=env, timeout=10)
        (directory / 'tasks.json').write_text(listing.stdout)
        tasks = json.loads(listing.stdout)
        assert len(tasks) == 1, tasks
        task_id = tasks[0]['id']
        inspect = subprocess.run([str(binary), 'task', 'show', task_id, '--json'], capture_output=True, text=True, env=env, timeout=10)
        (directory / 'task-show.json').write_text(inspect.stdout)
        detail = json.loads(inspect.stdout)
        events = subprocess.run([str(binary), 'task', 'events', task_id, '--json'], capture_output=True, text=True, env=env, timeout=10)
        (directory / 'task-events.jsonl').write_text(events.stdout)
        calls = detail['tool_calls'] or []
        expected_count = 0 if mode in ('invalid-args', 'pre-cancel') or approval in ('no', 'eof', 'tui-no') else len(server.steps)
        assert len(calls_on_wire) == expected_count, ('wire count', calls_on_wire)
        status = 'cancelled' if mode == 'pre-cancel' else 'declined' if approval in ('no', 'eof', 'tui-no') else ('unknown' if mode in ('drop', 'timeout', 'signal') else ('failed' if mode in ('error', 'invalid-args') else 'succeeded'))
        assert len(calls) == len(server.steps), calls
        assert all(call['status'] == status for call in calls), calls
        assert bool(detail['approvals']) == (mode != 'invalid-args'), 'incorrect approval persistence'
        assert all(a['decision'] == ('declined' if approval in ('no', 'eof', 'tui-no') or mode == 'pre-cancel' else 'approved') for a in detail['approvals']), detail['approvals']
        if name.startswith(('resources-', 'prompts-')):
            wire = messages(directory / 'fixture.jsonl')
            assert not any(m.get('method') == 'tools/list' for m in wire), 'non-tool server was asked for tools'
            if approval != 'no' and mode not in ('invalid-args', 'pre-cancel'):
                evidence = 'resource-content-evidence' if name.startswith('resources-') else 'prompt-content-evidence: security'
                assert evidence in json.dumps(server.requests), 'catalog content did not reach model'
                assert all(call['result'].get('artifacts') for call in calls), 'catalog artifact missing'
                if name.startswith('resources-'):
                    assert any(m.get('params', {}).get('cursor') == 'second' for m in wire), 'pagination cursor missing'
        if mode == 'error':
            assert 'fixture-error-evidence' in json.dumps(calls), 'server error content lost'
            assert 'fixture-error-evidence' in json.dumps(server.requests), 'server error not returned to model'
        if mode in ('drop', 'timeout', 'signal'):
            assert result.returncode != 0 or mode == 'signal', 'unknown outcome did not stop CLI'
            count = len(server.requests)
            resume = subprocess.run([str(binary), 'task', 'resume', task_id, '--auto-approve'], input='', capture_output=True, text=True, env=env, timeout=20)
            (directory / 'resume-transcript.txt').write_text(resume.stdout + resume.stderr)
            assert resume.returncode != 0, 'unknown outcome resumed'
            assert len(server.requests) == count, 'unknown outcome reached provider on resume'
            assert sum(m.get('method') == 'tools/call' for log in directory.glob('*.jsonl') for m in messages(log)) == expected_count, 'resume repeated effect'
        elif mode != 'pre-cancel':
            assert result.returncode == 0, result.stderr
        if name in ('startup-timeout', 'descendant-startup'):
            assert 'slow' in result.stdout + result.stderr, 'startup timeout not reported'
        if name in ('catalog-count', 'catalog-bytes', 'catalog-escaped-bytes'):
            assert 'overflow unavailable' in result.stdout, result.stdout
            assert len(server.advertised) == (60 if name == 'catalog-count' else 1), len(server.advertised)
            assert not any(t['name'].startswith('mcp__overflow__') for t in server.advertised), 'overflow catalog reached model'
            assert len(json.dumps(server.advertised).encode()) <= 1 << 20, 'serialized catalog exceeds 1 MiB'
        if name == 'bearer':
            assert server.authorization and all(value == 'Bearer bearer-secret-evidence' for value in server.authorization), 'bearer token absent'
            assert 'bearer-secret-evidence' not in json.dumps(server.requests) + json.dumps(detail) + result.stdout + result.stderr, 'bearer credential leaked'
        if name == 'legacy-init-only':
            wire = messages(directory / 'fixture.jsonl')
            assert sum('started_pid' in row for row in wire) == 2, wire
            methods = [row.get('method') for row in wire if 'method' in row]
            assert methods[:3] == ['server/discover', 'initialize', 'notifications/initialized'], methods
            assert methods.count('tools/call') == 1, methods
        if name == 'environment':
            child = messages(directory / 'fixture.jsonl')[0]['environment']
            assert child == {'OPENAI_API_KEY': None, 'E2E_UNRELATED_SECRET': None, 'E2E_ALLOWED': 'allowed-value', 'E2E_OVERRIDE': 'configured'}, child
        if mode == 'structured':
            encoded = calls[0]['result']['output']
            assert json.loads(encoded)['structuredContent'] == {'nested': {'verified': True, 'number': 42}}, encoded
        if name == 'resume-success':
            resume = subprocess.run([str(binary), 'task', 'resume', task_id, '--auto-approve'], input='', capture_output=True, text=True, env=env, timeout=20)
            (directory / 'resume-transcript.txt').write_text(resume.stdout + resume.stderr)
            assert resume.returncode == 0, resume.stderr
            assert sum(m.get('method') == 'tools/call' for log in directory.glob('*.jsonl') for m in messages(log)) == 1, 'successful resume repeated effect'
        if unavailable:
            assert not (directory / 'disabled.jsonl').exists(), 'disabled server started'
            assert 'missing' in result.stdout + result.stderr, 'startup failure not reported'
        if mode == 'large':
            assert len(json.dumps(server.requests[-1])) < 400000, 'model output unbounded'
            artifacts = calls[0]['result'].get('artifacts', [])
            assert artifacts, 'full result artifact absent'
            artifact = max(artifacts, key=lambda item: item['size'])
            data = (home / 'tasks' / 'artifacts' / artifact['id']).read_bytes()
            assert len(data) == artifact['size'] and hashlib.sha256(data).hexdigest() == artifact['sha256'], 'artifact integrity mismatch'
            assert b'large-evidence:' + b'x' * 600000 in data, 'full MCP output not preserved'
        for log in directory.glob('*.jsonl'):
            for message in messages(log):
                if 'started_pid' in message:
                    try:
                        os.kill(message['started_pid'], 0)
                    except ProcessLookupError:
                        pass
                    else:
                        raise AssertionError('fixture process leaked: ' + str(message['started_pid']))
        return {'name': name, 'passed': True, 'exit_code': result.returncode, 'task_id': task_id, 'wire_calls': len(calls_on_wire), 'status': status}
    finally:
        # Clean fixture leaks only after the assertion has recorded the failure.
        for log in directory.glob('*.jsonl'):
            for message in messages(log):
                if message.get('descendant'):
                    try:
                        os.kill(message['started_pid'], 9)
                    except ProcessLookupError:
                        pass
        server.shutdown()
        server.server_close()


def run_serve_case(binary, output, name):
    directory = output / name
    directory.mkdir()
    home, workspace = directory / 'home', directory / 'workspace'
    home.mkdir(mode=0o700)
    workspace.mkdir()
    (workspace / 'hello.txt').write_text('server-read-evidence\n')
    env = dict(os.environ, MELDRA_HOME=str(home))
    process = subprocess.Popen([str(binary), 'mcp', 'serve', '--workspace', str(workspace)], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=env)
    latest = name.startswith('serve-latest-')
    accepted = name.endswith('approve') or name in ('serve-command-failed', 'serve-approval-budget', 'serve-latest-undo-state', 'serve-latest-undo-decline')
    buffer = bytearray()
    transcript = directory / 'protocol.jsonl'
    sequence = 0
    approval_waited = False
    def send(message):
        record(transcript, {'direction': 'client', 'message': message})
        process.stdin.write((json.dumps(message) + '\n').encode())
        process.stdin.flush()
    def receive():
        deadline = time.monotonic() + 15
        while b'\n' not in buffer:
            assert time.monotonic() < deadline, 'MCP server response timed out'
            if select.select([process.stdout], [], [], 0.1)[0]:
                chunk = os.read(process.stdout.fileno(), 65536)
                assert chunk, 'MCP server closed stdout'
                buffer.extend(chunk)
        line, _, remaining = buffer.partition(b'\n')
        buffer[:] = remaining
        message = json.loads(line)
        record(transcript, {'direction': 'server', 'message': message})
        return message
    def request(method, params, handle_approval=True):
        nonlocal sequence, approval_waited
        sequence += 1
        request_id = sequence
        if latest:
            params['_meta'] = {'io.modelcontextprotocol/protocolVersion': '2026-07-28', 'io.modelcontextprotocol/clientInfo': {'name': 'e2e', 'version': '1'}, 'io.modelcontextprotocol/clientCapabilities': {'elicitation': {'form': {}}}}
        send({'jsonrpc': '2.0', 'id': request_id, 'method': method, 'params': params})
        while True:
            message = receive()
            if message.get('method') == 'elicitation/create':
                if name == 'serve-approval-budget' and not approval_waited:
                    approval_waited = True
                    time.sleep(121)
                send({'jsonrpc': '2.0', 'id': message['id'], 'result': {'action': 'accept' if accepted else 'decline', 'content': {'approve': True} if accepted else {}}})
            elif message.get('id') == request_id:
                result = message.get('result', {})
                if result.get('resultType') == 'input_required' and handle_approval:
                    assert latest and 'approval' in result['inputRequests'], result
                    params['inputResponses'] = {'approval': {'action': 'accept' if accepted else 'decline', 'content': {'approve': True} if accepted else {}}}
                    params['requestState'] = result['requestState']
                    return request(method, params)
                return message
    try:
        response = request('server/discover' if latest else 'initialize', {'protocolVersion': '2025-11-25', 'clientInfo': {'name': 'e2e', 'version': '1'}, 'capabilities': {'elicitation': {'form': {}}}})
        assert 'result' in response, response
        if not latest:
            send({'jsonrpc': '2.0', 'method': 'notifications/initialized'})
        if name == 'serve-latest-context-state':
            def call(tool, arguments, state=None):
                params = {'name': tool, 'arguments': arguments}
                if state:
                    params.update(requestState=state, inputResponses={'approval': {'action': 'accept', 'content': {'approve': True}}})
                return request('tools/call', params, handle_approval=False)['result']

            def context(path=None):
                result = call('read_project_context', {'path': path})
                assert not result.get('isError'), result

            def challenge(tool, arguments):
                result = call(tool, arguments)
                assert result.get('resultType') == 'input_required', result
                return result['requestState']

            def accepted(tool, arguments):
                result = call(tool, arguments, challenge(tool, arguments))
                assert not result.get('isError'), result

            (workspace / 'AGENTS.md').write_text('Root revision one.\n')
            context()
            arguments = {'path': 'hello.txt', 'old_str': 'server-read-evidence', 'new_str': 'current-write'}
            state = challenge('edit_file', arguments)
            (workspace / 'AGENTS.md').write_text('Root revision two.\n')
            context()
            stale = call('edit_file', arguments, state)
            assert stale.get('isError'), 'context reread revived stale approval: ' + json.dumps(stale)
            assert (workspace / 'hello.txt').read_text() == 'server-read-evidence\n'
            accepted('edit_file', arguments)

            replacement = {'path': 'hello.txt', 'old_str': 'current-write', 'new_str': 'must-not-revive'}
            consumed = challenge('edit_file', replacement)
            (workspace / 'AGENTS.md').write_text('Temporarily changed during approval.\n')
            assert call('edit_file', replacement, consumed).get('isError'), 'changed rules accepted before context read'
            (workspace / 'AGENTS.md').write_text('Root revision two.\n')
            context()
            assert call('edit_file', replacement, consumed).get('isError'), 'reverted rules revived consumed approval'
            assert (workspace / 'hello.txt').read_text() == 'current-write\n'

            for path in ('left', 'right'):
                (workspace / path).mkdir()
                (workspace / path / 'AGENTS.md').write_text(path + ' scoped instructions.\n')
            patch = {'patch': None, 'changes': [{'path': 'left/new.txt', 'old_str': '', 'new_str': 'left\n'}, {'path': 'right/new.txt', 'old_str': '', 'new_str': 'right\n'}]}
            unseen = call('apply_patch', patch)
            assert unseen.get('isError') and not unseen.get('requestState'), 'approval issued before target instructions were loaded: ' + json.dumps(unseen)
            context('left/new.txt')
            context('right/new.txt')
            accepted('apply_patch', patch)

            deletion = {'patch': '--- a/left/new.txt\n+++ /dev/null\n@@ -1,2 +0,0 @@\n-left\n-\n', 'changes': None}
            state = challenge('apply_patch', deletion)
            (workspace / 'left/AGENTS.md').write_text('Left changed during delete approval.\n')
            context('left/new.txt')
            stale = call('apply_patch', deletion, state)
            assert stale.get('isError') and (workspace / 'left/new.txt').exists(), stale
            accepted('apply_patch', deletion)
            assert not (workspace / 'left/new.txt').exists()

            state = challenge('undo_last_change', {})
            (workspace / 'left/AGENTS.md').write_text('Left changed during undo approval.\n')
            context('left/new.txt')
            stale = call('undo_last_change', {}, state)
            assert stale.get('isError') and not (workspace / 'left/new.txt').exists(), stale
            accepted('undo_last_change', {})
            assert (workspace / 'left/new.txt').read_text() == 'left\n'
            process.stdin.close()
            process.wait(timeout=10)
            stderr = process.stderr.read().decode()
            (directory / 'stderr.txt').write_text(stderr)
            assert process.returncode == 0 and 'DATA RACE' not in stderr, stderr
            return {'name': name, 'passed': True, 'stale_context_approvals_rejected': 3, 'failed_redemption_consumed': True, 'multi_scope_discovery_before_approval': True}
        if name == 'serve-cancel':
            process.terminate()
            process.wait(timeout=5)
            (directory / 'stderr.txt').write_text(process.stderr.read().decode())
            return {'name': name, 'passed': True, 'exit_code': process.returncode}
        tools = request('tools/list', {})['result']['tools']
        assert {'read_file', 'edit_file', 'git_review'} <= {t['name'] for t in tools}
        response = request('tools/call', {'name': 'git_review'})
        assert 'result' in response and 'invalid tool arguments' not in json.dumps(response), response
        response = request('tools/call', {'name': 'read_file', 'arguments': {'path': 'hello.txt', 'offset': None, 'limit': None}})
        assert 'server-read-evidence' in json.dumps(response), response
        response = request('tools/call', {'name': 'read_file', 'arguments': {'path': '../outside', 'offset': None, 'limit': None}})
        assert response['result']['isError'], response
        response = request('tools/call', {'name': 'read_file', 'arguments': {'path': '.git/config', 'offset': None, 'limit': None}})
        assert response['result']['isError'], response
        response = request('resources/list', {})
        assert response['result']['resources'][0]['uri'] == 'meldra://workspace', response
        response = request('resources/templates/list', {})
        assert response['result']['resourceTemplates'][0]['uriTemplate'] == 'meldra://workspace/{path}', response
        response = request('resources/read', {'uri': 'meldra://workspace/hello.txt'})
        assert 'server-read-evidence' in json.dumps(response), response
        response = request('resources/read', {'uri': 'meldra://workspace/%2e%2e%2foutside'})
        assert 'error' in response, response
        response = request('prompts/list', {})
        assert response['result']['prompts'][0]['name'] == 'review_workspace', response
        response = request('prompts/get', {'name': 'review_workspace', 'arguments': {'focus': 'correctness'}})
        assert 'correctness' in json.dumps(response), response
        if latest:
            forged = request('tools/call', {'name': 'edit_file', 'arguments': {'path': 'forged.txt', 'old_str': '', 'new_str': 'must never write'}, 'requestState': 'forged-state', 'inputResponses': {'approval': {'action': 'accept', 'content': {'approve': True}}}})
            assert forged['result'].get('isError'), forged
            assert not (workspace / 'forged.txt').exists(), forged
        if name == 'serve-command-failed':
            (workspace / 'fail.py').write_text('print("nonzero-command-evidence")\nraise SystemExit(7)\n')
            failed = request('tools/call', {'name': 'run_command', 'arguments': {'command': 'python3', 'args': ['fail.py'], 'timeout': 5}})
            assert failed['result'].get('isError') and 'nonzero-command-evidence' in json.dumps(failed), failed
        response = request('tools/call', {'name': 'edit_file', 'arguments': {'path': 'hello.txt', 'old_str': 'server-read-evidence', 'new_str': 'server-write-evidence'}})
        assert bool(response['result'].get('isError')) == (not accepted), response
        if name == 'serve-latest-undo-decline':
            challenge = request('tools/call', {'name': 'undo_last_change', 'arguments': {}}, handle_approval=False)['result']
            accepted = False
            rejected = request('tools/call', {'name': 'edit_file', 'arguments': {'path': 'hello.txt', 'old_str': 'server-write-evidence', 'new_str': 'must-not-write'}})
            assert rejected['result'].get('isError'), rejected
            accepted = True
            unchanged = request('tools/call', {'name': 'undo_last_change', 'arguments': {}, 'requestState': challenge['requestState'], 'inputResponses': {'approval': {'action': 'accept', 'content': {'approve': True}}}})
            assert not unchanged['result'].get('isError'), 'declined edit invalidated pending undo approval: ' + json.dumps(unchanged)
        if name == 'serve-latest-undo-state':
            challenge = request('tools/call', {'name': 'undo_last_change', 'arguments': {}}, handle_approval=False)['result']
            assert challenge.get('resultType') == 'input_required', challenge
            newer = request('tools/call', {'name': 'edit_file', 'arguments': {'path': 'hello.txt', 'old_str': 'server-write-evidence', 'new_str': 'server-newer-evidence'}})
            assert not newer['result'].get('isError'), newer
            stale = request('tools/call', {'name': 'undo_last_change', 'arguments': {}, 'requestState': challenge['requestState'], 'inputResponses': {'approval': {'action': 'accept', 'content': {'approve': True}}}})
            assert stale['result'].get('isError'), 'stale undo approval executed: ' + json.dumps(stale)
            assert (workspace / 'hello.txt').read_text() == 'server-newer-evidence\n', 'stale approval undid the newer change'
            fresh = request('tools/call', {'name': 'undo_last_change', 'arguments': {}})
            assert not fresh['result'].get('isError'), fresh
        if latest:
            approved_requests = [row['message']['params'] for row in messages(transcript) if row['direction'] == 'client' and row['message'].get('method') == 'tools/call' and row['message']['params'].get('requestState') not in (None, 'forged-state')]
            assert approved_requests, 'no approval challenge issued'
            replay = request('tools/call', dict(approved_requests[-1]))
            assert replay['result'].get('isError'), replay
        if latest and not accepted:
            declined_command = request('tools/call', {'name': 'run_command', 'arguments': {'command': 'git', 'args': ['status'], 'timeout': None}})
            assert 'Declined; operation was not executed.' in json.dumps(declined_command), declined_command
        expected = 'server-write-evidence\n' if accepted and name != 'serve-latest-undo-decline' else 'server-read-evidence\n'
        assert (workspace / 'hello.txt').read_text() == expected, response
        assert any(row['message'].get('method') == 'elicitation/create' or row['message'].get('result', {}).get('resultType') == 'input_required' for row in messages(transcript)), 'server did not request approval'
        process.stdin.close()
        process.wait(timeout=10)
        stderr = process.stderr.read().decode()
        (directory / 'stderr.txt').write_text(stderr)
        assert process.returncode == 0, stderr
        listing = subprocess.run([str(binary), 'tasks', '--json'], capture_output=True, text=True, env=env, timeout=10)
        (directory / 'tasks.json').write_text(listing.stdout)
        tasks = json.loads(listing.stdout)
        assert len(tasks) >= 5, tasks
        details = []
        for task in tasks:
            show = subprocess.run([str(binary), 'task', 'show', task['id'], '--json'], capture_output=True, text=True, env=env, timeout=10)
            details.append(json.loads(show.stdout))
        save(directory / 'task-details.json', details)
        decisions = [approval['decision'] for detail in details for approval in (detail.get('approvals') or [])]
        if name == 'serve-latest-undo-decline':
            assert sorted(decisions) == ['approved', 'approved', 'declined'], decisions
        else:
            assert decisions == (['approved'] * (3 if name == 'serve-latest-undo-state' else 2 if name == 'serve-command-failed' else 1) if accepted else ['declined'] * (2 if latest else 1)), decisions
        return {'name': name, 'passed': True, 'tasks': len(tasks), 'approval': decisions[0]}
    finally:
        if process.poll() is None:
            process.kill()
            process.wait()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', default='dist/meldra')
    parser.add_argument('--output', default='dist/mcp-e2e')
    parser.add_argument('--case', help='Run one named scenario')
    parser.add_argument('--stdio-fixture', type=Path, help=argparse.SUPPRESS)
    args = parser.parse_args()
    if args.stdio_fixture:
        stdio_fixture(args.stdio_fixture)
        return
    binary, output = Path(args.binary).resolve(), Path(args.output).resolve()
    if output.exists():
        parser.error('output exists; choose a fresh --output to preserve evidence')
    output.mkdir(parents=True)
    scenarios = [('stdio', {}), ('http', {'transport': 'http'}), ('pagination', {}), ('namespaces', {'two': True}), ('approve', {'approval': 'yes'}), ('deny', {'approval': 'no'}), ('deny-eof', {'approval': 'eof'}), ('server-error', {'mode': 'error'}), ('timeout', {'mode': 'timeout'}), ('disconnect', {'mode': 'drop'}), ('http-disconnect', {'mode': 'drop', 'transport': 'http'}), ('duplicate', {'mode': 'duplicate'}), ('unavailable-disabled', {'unavailable': True}), ('large-output', {'mode': 'large'}), ('invalid-json', {'bad': 'json'}), ('invalid-ambiguous', {'bad': 'ambiguous'}), ('invalid-timeout', {'bad': 'timeout'})]
    scenarios += [('invalid-arguments', {'mode': 'invalid-args'}), ('bearer', {'transport': 'http'}), ('environment', {}), ('structured-content', {'mode': 'structured'}), ('startup-timeout', {}), ('resume-success', {}), ('sigterm', {'mode': 'signal'})]
    scenarios += [('tui-approve', {'approval': 'tui-yes'}), ('tui-deny', {'approval': 'tui-no'})]
    scenarios += [('descendant-normal', {}), ('descendant-startup', {}), ('descendant-sigterm', {'mode': 'signal'})]
    scenarios += [('legacy-init-only', {})]
    scenarios += [('resources-client', {}), ('resources-deny', {'approval': 'no'}), ('prompts-client', {}), ('resources-cancel', {'mode': 'pre-cancel', 'approval': 'yes'}), ('resources-invalid', {'mode': 'invalid-args'}), ('prompts-invalid', {'mode': 'invalid-args'})]
    scenarios += [('serve-approve', {}), ('serve-deny', {}), ('serve-latest-approve', {}), ('serve-latest-deny', {}), ('serve-cancel', {})]
    scenarios += [('http-slow-startup', {'transport': 'http'})]
    scenarios += [('catalog-count', {}), ('catalog-bytes', {}), ('catalog-escaped-bytes', {}), ('serve-command-failed', {}), ('serve-approval-budget', {}), ('serve-latest-undo-state', {}), ('serve-latest-undo-decline', {})]
    scenarios += [('serve-latest-context-state', {})]
    reports = []
    for name, options in scenarios:
        if args.case and name != args.case:
            continue
        try:
            report = run_serve_case(binary, output, name) if name.startswith('serve-') else run_case(binary, output, name, **options)
        except Exception:
            report = {'name': name, 'passed': False, 'error': traceback.format_exc()}
        reports.append(report)
        print(name + ': ' + ('PASS' if report['passed'] else 'FAIL'), flush=True)
        save(output / 'report.json', {'binary': str(binary), 'binary_sha256': hashlib.sha256(binary.read_bytes()).hexdigest(), 'scenarios': reports})
    assert reports, 'no matching scenario'
    checksums = {str(path.relative_to(output)): hashlib.sha256(path.read_bytes()).hexdigest() for path in sorted(output.rglob('*')) if path.is_file()}
    save(output / 'checksums.json', checksums)
    print(output / 'report.json')
    sys.exit(0 if all(report['passed'] for report in reports) else 1)


if __name__ == '__main__':
    main()
