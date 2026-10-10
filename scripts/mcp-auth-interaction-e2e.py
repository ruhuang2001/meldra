#!/usr/bin/env python3
"""Real-binary OAuth, sampling, elicitation checks; writes repeatable wire/report artifacts.
Failure matrix: bad state/PKCE/issuer, unauthorized/token refresh/config binding,
secret or malformed forms, decline/cancel, maxTokens and model-context isolation.
Schema regressions: reject camelCase secrets, additional/pattern properties and
object composition before prompting; reject undeclared answers; retain schema
on retries; allow supported titled enums and string-enum arrays.
"""
import argparse
import base64
import hashlib
import http.server
import json
import os
from pathlib import Path
import queue
import pty
import select
import fcntl
import struct
import termios
import subprocess
import sys
import threading
import time
import urllib.parse
import urllib.request


def write(path, value):
    path.write_text(json.dumps(value, indent=2) + '\n')


def log(path, value):
    with path.open('a') as out:
        out.write(json.dumps(value) + '\n')


def fixture(path, mode):
    for line in sys.stdin:
        request = json.loads(line)
        log(path, request)
        if 'id' not in request:
            continue
        method = request.get('method')
        result = {}
        error = None
        if method == 'server/discover':
            if mode.endswith('-modern'):
                result = {'supportedVersions': ['2026-07-28'], 'capabilities': {'tools': {}}, 'resultType': 'complete', 'cacheScope': 'public'}
            else:
                error = {'code': -32601, 'message': 'legacy fixture'}
        elif method == 'initialize':
            result = {'protocolVersion': '2025-11-25', 'serverInfo': {'name': 'interaction-fixture', 'version': '1'}, 'capabilities': {'tools': {}}}
        elif method == 'tools/list':
            if mode == 'unsolicited':
                print(json.dumps({'jsonrpc': '2.0', 'id': 'interaction', 'method': 'elicitation/create', 'params': {'mode': 'form', 'message': 'Unsolicited request', 'requestedSchema': {'type': 'object', 'properties': {}}}}), flush=True)
                while True:
                    answer = json.loads(sys.stdin.readline())
                    if answer.get('id') == 'interaction':
                        log(path, {'interaction_response': answer})
                        break
            result = {'tools': [{'name': 'interact', 'description': 'interaction fixture', 'inputSchema': {'type': 'object', 'properties': {}}}]}
        elif method == 'tools/call' and mode == 'unsolicited':
            result = {'content': [{'type': 'text', 'text': 'unsolicited rejection evidence'}]}
        elif method == 'tools/call':
            params = {'mode': 'form', 'message': 'Fixture asks for a color', 'requestedSchema': {'type': 'object', 'properties': {'color': {'type': 'string', 'enum': ['blue', 'green']}}, 'required': ['color'], 'additionalProperties': False}}
            call = 'elicitation/create'
            if mode.startswith('url'):
                params = {'mode': 'url', 'message': 'Fixture external interaction', 'url': 'https://example.com/mcp-complete', 'elicitationId': 'e1'}
            if mode == 'url-insecure':
                params['url'] = 'http://example.com/insecure'
            if mode.startswith('secret'):
                key = 'apiKey' if mode == 'secret-camel' else 'password'
                params['requestedSchema']['properties'] = {key: {'type': 'string'}}
                params['requestedSchema']['required'] = [key]
            if mode == 'schema-additional':
                params['requestedSchema']['additionalProperties'] = {'type': 'object'}
            if mode == 'schema-pattern':
                params['requestedSchema']['patternProperties'] = {'.*': {'type': 'object'}}
            if mode == 'schema-composition':
                params['requestedSchema']['allOf'] = [{'properties': {'password': {'type': 'string'}}}]
            if mode == 'form-extra':
                params['requestedSchema'].pop('additionalProperties')
            if mode == 'titled-enum':
                params['requestedSchema']['properties']['color'] = {'type': 'string', 'oneOf': [{'const': 'blue', 'title': 'Blue'}, {'const': 'green', 'title': 'Green'}]}
            if mode == 'titled-array':
                params['requestedSchema']['properties']['color'] = {'type': 'array', 'items': {'anyOf': [{'const': 'blue', 'title': 'Blue'}, {'const': 'green', 'title': 'Green'}]}}
            if mode in ('form-integer-exact', 'form-integer-exact-modern', 'form-integer-fraction', 'form-number-exact', 'format-password'):
                field = {'type': 'number' if mode == 'form-number-exact' else 'string' if mode == 'format-password' else 'integer'}
                if mode == 'format-password':
                    field['format'] = 'password'
                params['requestedSchema']['properties'] = {'value': field}
                params['requestedSchema']['required'] = ['value']
            if mode.startswith('sampling'):
                call = 'sampling/createMessage'
                params = {'maxTokens': 17, 'systemPrompt': 'isolated-sampling-system', 'messages': [{'role': 'user', 'content': {'type': 'text', 'text': 'isolated-sampling-user'}}]}
            if mode.endswith('-modern') and not request.get('params', {}).get('inputResponses'):
                result = {'resultType': 'input_required', 'requestState': 'fixture-state', 'inputRequests': {'interaction': {'method': call, 'params': params}}}
            else:
                if mode.endswith('-modern'):
                    assert request['params']['requestState'] == 'fixture-state'
                    answer = request['params']['inputResponses']['interaction']
                else:
                    print(json.dumps({'jsonrpc': '2.0', 'id': 'interaction', 'method': call, 'params': params}), flush=True)
                    response_line = sys.stdin.readline()
                    log(path, {'interaction_response_raw': response_line.rstrip('\n')})
                    answer = json.loads(response_line)
                log(path, {'interaction_response': answer})
                result = {'content': [{'type': 'text', 'text': 'interaction-evidence: ' + json.dumps(answer)}]}
                if mode.endswith('-modern'):
                    result['resultType'] = 'complete'
        reply = {'jsonrpc': '2.0', 'id': request['id']}
        reply['error' if error else 'result'] = error or result
        print(json.dumps(reply), flush=True)


class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def reply(self, body, status=200, headers=None):
        self.send_response(status)
        self.send_header('Content-Type', 'application/json')
        for key, value in (headers or {}).items():
            self.send_header(key, value)
        self.end_headers()
        self.wfile.write(json.dumps(body).encode())

    def do_GET(self):
        server = self.server
        if self.path.startswith('/.well-known/oauth-protected-resource'):
            self.reply({'resource': server.url + '/mcp', 'authorization_servers': [server.url]})
        elif self.path == '/.well-known/oauth-authorization-server':
            self.reply({'issuer': server.url, 'authorization_endpoint': server.url + '/authorize', 'token_endpoint': server.url + '/token', 'registration_endpoint': server.url + '/register', 'code_challenge_methods_supported': [] if server.bad_pkce else ['S256'], 'scopes_supported': ['read', 'offline_access'], 'authorization_response_iss_parameter_supported': True})
        else:
            self.reply({}, 405)

    def do_DELETE(self):
        self.reply({})

    def do_POST(self):
        server = self.server
        raw = self.rfile.read(int(self.headers.get('Content-Length', 0)))
        if self.path == '/token':
            fields = urllib.parse.parse_qs(raw.decode())
            server.token_requests.append(fields)
            if fields['grant_type'] == ['authorization_code']:
                verifier = fields['code_verifier'][0]
                challenge = base64.urlsafe_b64encode(hashlib.sha256(verifier.encode()).digest()).rstrip(b'=').decode()
                assert challenge == server.challenge, 'PKCE verifier did not match'
                assert fields['resource'] == [server.url + '/mcp'], 'token audience missing'
            else:
                assert fields['refresh_token'][0].startswith('fixture-refresh-')
            server.token_serial += 1
            self.reply({'access_token': 'fixture-access-' + str(server.token_serial), 'refresh_token': 'fixture-refresh-' + str(server.token_serial), 'token_type': 'Bearer', 'expires_in': 3600})
            return
        body = json.loads(raw)
        if self.path == '/register':
            self.reply(dict(body, client_id='dynamic-fixture'), 201)
            return
        if self.path == '/mcp':
            server.auth_headers.append(self.headers.get('Authorization'))
            if self.headers.get('Authorization') != 'Bearer fixture-access-' + str(server.token_serial) or not server.token_serial:
                self.reply({}, 401, {'WWW-Authenticate': 'Bearer resource_metadata="' + server.url + '/.well-known/oauth-protected-resource"'})
                return
            if 'id' not in body:
                self.send_response(202)
                self.end_headers()
                return
            if body['method'] == 'server/discover':
                self.reply({'jsonrpc': '2.0', 'id': body['id'], 'error': {'code': -32601, 'message': 'legacy fixture'}})
            else:
                result = {'protocolVersion': '2025-11-25', 'serverInfo': {'name': 'oauth-fixture', 'version': '1'}, 'capabilities': {}} if body['method'] == 'initialize' else {}
                self.reply({'jsonrpc': '2.0', 'id': body['id'], 'result': result})
            return
        server.provider_requests.append(body)
        if body.get('stream'):
            self.reply({'error': {'message': 'stream is not supported', 'type': 'invalid_request_error', 'code': 'unsupported_parameter', 'param': 'stream'}}, 400)
            return
        sampling = body.get('instructions') == 'isolated-sampling-system'
        output = []
        if sampling:
            text = 'isolated-sampling-result'
        elif not server.main_calls and any(t['name'].startswith('mcp__') for t in body.get('tools', [])):
            server.main_calls += 1
            tool = next(t['name'] for t in body['tools'] if t['name'].startswith('mcp__'))
            output = [{'type': 'function_call', 'id': 'fc_interact', 'call_id': 'call_interact', 'name': tool, 'arguments': '{}', 'status': 'completed'}]
        else:
            text = 'E2E complete'
        if not output:
            output = [{'type': 'message', 'id': 'msg_result', 'role': 'assistant', 'status': 'completed', 'content': [{'type': 'output_text', 'text': text, 'annotations': []}]}]
        self.reply({'id': 'resp_' + str(len(server.provider_requests)), 'object': 'response', 'status': 'completed', 'output': output, 'usage': {'input_tokens': 1, 'output_tokens': 1, 'total_tokens': 2}})


def setup(output, name):
    directory = output / name
    directory.mkdir()
    home, workspace = directory / 'home', directory / 'workspace'
    home.mkdir(mode=0o700)
    workspace.mkdir()
    server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Handler)
    server.url = 'http://127.0.0.1:' + str(server.server_port)
    server.bad_pkce = False
    server.main_calls = server.token_serial = 0
    server.provider_requests, server.token_requests, server.auth_headers = [], [], []
    threading.Thread(target=server.serve_forever, daemon=True).start()
    (home / 'config.toml').write_text('model="fixture"\nbase_url="' + server.url + '/v1"\nallow_insecure_base_url=true\n')
    (home / 'config.toml').chmod(0o600)
    env = {k: v for k, v in os.environ.items() if not k.startswith(('MELDRA_', 'OPENAI_'))}
    env.update(MELDRA_HOME=str(home), OPENAI_API_KEY='fixture-model-key', TERM='dumb')
    return directory, home, workspace, server, env


def set_config(home, server):
    write(home / 'mcp.json', {'mcpServers': {'fixture': server}})
    (home / 'mcp.json').chmod(0o600)


def chat(binary, workspace, env, stdin=''):
    return subprocess.run([str(binary), '--workspace', str(workspace), '--prompt', 'main-secret-context-do-not-share', '--auto-approve'], input=stdin, capture_output=True, text=True, env=env, timeout=30)


def terminal_chat(binary, workspace, env, mode):
    master, slave = pty.openpty()
    fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack('HHHH', 45, 150, 0, 0))
    env = dict(env, TERM='xterm-256color')
    process = subprocess.Popen([str(binary), '--workspace', str(workspace), '--prompt', 'main-secret-context-do-not-share', '--auto-approve'], stdin=slave, stdout=slave, stderr=slave, env=env)
    os.close(slave)
    output = bytearray()
    answered = stopped = False
    try:
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline:
            if select.select([master], [], [], 0.05)[0]:
                try:
                    chunk = os.read(master, 65536)
                except OSError:
                    break
                if not chunk:
                    break
                output.extend(chunk)
            if not answered and b'Enter one JSON' in output:
                os.write(master, b'{"color":"blue"}\r')
                answered = True
            if not stopped and b'E2E complete' in output:
                os.write(master, b'\x03')
                stopped = True
            if process.poll() is not None:
                break
        if process.poll() is None:
            process.terminate()
        process.wait(timeout=5)
        assert answered and stopped, output.decode(errors='replace')
        return subprocess.CompletedProcess([], process.returncode, output.decode(errors='replace'), '')
    finally:
        if process.poll() is None:
            process.kill()
            process.wait()
        os.close(master)


def interaction_case(binary, output, mode, stdin, expected):
    directory, home, workspace, server, env = setup(output, mode)
    try:
        set_config(home, {'command': sys.executable, 'args': [str(Path(__file__).resolve()), '--fixture', str(directory / 'wire.jsonl'), mode], 'sampling': mode.startswith('sampling') and mode != 'sampling-disabled', 'tool_timeout_sec': 20})
        result = terminal_chat(binary, workspace, env, mode) if mode.endswith("-tui") else chat(binary, workspace, env, stdin)
        (directory / 'transcript.txt').write_text(result.stdout + result.stderr)
        write(directory / 'provider.json', server.provider_requests)
        assert result.returncode == 0, result.stderr
        wire = [json.loads(line) for line in (directory / 'wire.jsonl').read_text().splitlines()]
        answer = next(m['interaction_response'] for m in wire if 'interaction_response' in m)
        assert expected in json.dumps(answer), answer
        if mode == 'form-retry':
            assert result.stdout.count('Schema:') >= 2, 'retry hides the schema'
        if mode in ('secret-camel', 'schema-additional', 'schema-pattern', 'schema-composition', 'format-password'):
            assert 'Enter one JSON object' not in result.stdout, 'invalid schema reached user input'
        if mode in ('form-integer-exact', 'form-integer-exact-modern', 'form-number-exact', 'form-integer-fraction'):
            response = answer if mode.endswith('-modern') else answer['result']
            assert response['action'] == 'accept', response
            value = response['content']['value']
            if mode.startswith('form-integer-exact'):
                assert value == 9007199254740993, 'large integer changed on the wire'
            elif mode == 'form-integer-fraction':
                assert value == 42 and result.stdout.count('Schema:') >= 2, 'fractional integer was accepted'
            else:
                # Inspect the wire spelling too: Python's float parser also rounds decimals.
                assert '0.1234567890123456789' in (directory / 'wire.jsonl').read_text(), 'decimal changed on the wire'
        samples = [r for r in server.provider_requests if r.get('instructions') == 'isolated-sampling-system']
        if mode.startswith('sampling-accept') or mode == 'sampling-share-decline':
            assert samples, 'sampling provider was not invoked'
            assert all(r['max_output_tokens'] == 17 and not r.get('tools') and not r.get('previous_response_id') for r in samples), samples
            assert all('main-secret-context-do-not-share' not in json.dumps(r) for r in samples)
        elif mode == 'sampling-decline':
            assert not samples, 'declined sampling reached provider'
        return {'name': mode, 'passed': True}
    finally:
        server.shutdown()
        server.server_close()


def login(binary, env, server, directory, suffix='', wrong_state=False, bad_issuer=False, cancel=False):
    process = subprocess.Popen([str(binary), 'mcp', 'login', 'fixture'], stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, env=env)
    lines = queue.Queue()
    def reader():
        for line in process.stdout:
            lines.put(line)
    threading.Thread(target=reader, daemon=True).start()
    output = []
    deadline = time.monotonic() + 15
    try:
        while time.monotonic() < deadline:
            try:
                line = lines.get(timeout=0.1)
            except queue.Empty:
                if process.poll() is not None:
                    break
                continue
            output.append(line)
            if line.startswith(server.url + '/authorize?'):
                if cancel:
                    process.terminate()
                    break
                query = urllib.parse.parse_qs(urllib.parse.urlparse(line.strip()).query)
                assert query['resource'] == [server.url + '/mcp']
                assert query['code_challenge_method'] == ['S256']
                server.challenge = query['code_challenge'][0]
                callback = query['redirect_uri'][0]
                if wrong_state:
                    try:
                        urllib.request.urlopen(callback + '?code=fixture-code&state=WRONG', timeout=5)
                        raise AssertionError('wrong state accepted')
                    except urllib.error.HTTPError as error:
                        assert error.code == 400
                params = {'code': 'fixture-code', 'state': query['state'][0], 'iss': server.url + '/wrong' if bad_issuer else server.url}
                urllib.request.urlopen(callback + '?' + urllib.parse.urlencode(params), timeout=5).read()
        process.wait(timeout=5)
        while not lines.empty():
            output.append(lines.get())
        text = ''.join(output)
        (directory / ('login' + suffix + '.txt')).write_text(text)
        assert 'fixture-access-' not in text and 'fixture-refresh-' not in text, 'token leaked'
        return process.returncode
    finally:
        if process.poll() is None:
            process.kill()
            process.wait()


def oauth_case(binary, output):
    directory, home, workspace, server, env = setup(output, 'oauth')
    try:
        set_config(home, {'url': server.url + '/mcp', 'oauth': {}})
        assert login(binary, env, server, directory, wrong_state=True) == 0
        path = home / 'mcp-oauth-fixture.json'
        saved = json.loads(path.read_text())
        assert path.stat().st_mode & 0o777 == 0o600
        assert saved['token']['access_token'].startswith('fixture-access-')
        result = chat(binary, workspace, env)
        assert result.returncode == 0 and 'unavailable' not in result.stdout, result.stdout + result.stderr
        assert len(server.token_requests) == 1, 'persisted session did not reuse access token'
        saved['token']['expiry'] = '2000-01-01T00:00:00Z'
        write(path, saved)
        result = chat(binary, workspace, env)
        assert result.returncode == 0 and 'unavailable' not in result.stdout, result.stdout + result.stderr
        assert len(server.token_requests) == 2 and server.token_requests[-1]['grant_type'] == ['refresh_token']
        refreshed = json.loads(path.read_text())
        assert refreshed['token']['refresh_token'] != saved['token']['refresh_token'], 'refresh rotation was not persisted'
        path.chmod(0o644)
        result = chat(binary, workspace, env)
        assert 'permissions' in result.stdout + result.stderr, 'insecure token file accepted'
        path.chmod(0o600)
        set_config(home, {'url': server.url + '/mcp', 'oauth': {'scopes': ['different']}})
        result = chat(binary, workspace, env)
        assert 'configuration changed' in result.stdout + result.stderr, 'token reused for different authorization config'
        set_config(home, {'url': server.url + '/mcp', 'oauth': {}})
        result = subprocess.run([str(binary), 'mcp', 'logout', 'fixture'], env=env, capture_output=True, text=True, timeout=10)
        assert result.returncode == 0 and not path.exists(), result.stderr
        assert login(binary, env, server, directory, suffix='-bad-issuer', bad_issuer=True) != 0
        assert not path.exists(), 'bad issuer persisted token'
        server.bad_pkce = True
        assert login(binary, env, server, directory, suffix='-bad-pkce') != 0
        assert not path.exists(), 'missing S256 accepted'
        # Keep replayable metadata without persisting authorization codes or tokens.
        write(directory / 'oauth-evidence.json', {'grants': [r['grant_type'][0] for r in server.token_requests], 'bearer_requests': sum(bool(h) for h in server.auth_headers), 'state_rejected_then_login_succeeded': True, 'refresh_rotated': True, 'issuer_rejected': True, 'pkce_rejected': True})
        return {'name': 'oauth-login-refresh-reject-logout', 'passed': True}
    finally:
        server.shutdown()
        server.server_close()


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--binary', type=Path)
    parser.add_argument('--output', type=Path)
    parser.add_argument('--fixture', nargs=2)
    args = parser.parse_args()
    if args.fixture:
        fixture(Path(args.fixture[0]), args.fixture[1])
        return
    args.binary = args.binary.resolve()
    args.output = args.output.resolve()
    args.output.mkdir()
    checks = []
    for mode, stdin, expected in [('form-accept', '{"color":"blue"}\n', 'blue'), ('form-retry', '{"color":17}\n{"color":"green"}\n', 'green'), ('form-decline', 'decline\n', 'decline'), ('form-cancel', '', 'cancel'), ('secret-form', '', 'sensitive elicitation'), ('url-accept', 'y\n', 'accept'), ('url-decline', 'n\n', 'decline'), ('sampling-accept', 'y\ny\n', 'isolated-sampling-result'), ('sampling-decline', 'n\n', 'sampling declined'), ('sampling-share-decline', 'y\nn\n', 'sharing declined'), ('form-accept-modern', '{"color":"blue"}\n', 'blue'), ('sampling-accept-modern', 'y\ny\n', 'isolated-sampling-result'), ('form-accept-tui', '', 'blue'), ('unsolicited', '', 'unsolicited'), ('url-insecure', '', 'HTTPS'), ('form-invalid', '{}\n{}\n{}\n', 'three times'), ('sampling-disabled', '', 'does not support CreateMessage')]:
        checks.append(interaction_case(args.binary, args.output, mode, stdin, expected))
        write(args.output / 'report.json', checks)
    checks.append(oauth_case(args.binary, args.output))
    for mode, stdin, expected in [('secret-camel', '', 'sensitive elicitation'), ('schema-additional', '', 'flat elicitation'), ('schema-pattern', '', 'flat elicitation'), ('schema-composition', '', 'flat elicitation'), ('form-extra', '{"color":"blue","apiKey":"fixture-only"}\ndecline\n', 'decline'), ('titled-enum', '{"color":"blue"}\n', 'blue'), ('titled-array', '{"color":["blue"]}\n', 'blue')]:
        checks.append(interaction_case(args.binary, args.output, mode, stdin, expected))
    for mode, stdin, expected in [('format-password', '', 'unsupported format'), ('form-integer-exact', '{"value":9007199254740993}\n', '9007199254740993'), ('form-integer-exact-modern', '{"value":9007199254740993}\n', '9007199254740993'), ('form-integer-fraction', '{"value":9007199254740993.5}\n{"value":42}\n', '42')]:
        checks.append(interaction_case(args.binary, args.output, mode, stdin, expected))
    write(args.output / 'report.json', checks)
    write(args.output / 'checksums.json', {str(p.relative_to(args.output)): hashlib.sha256(p.read_bytes()).hexdigest() for p in args.output.rglob('*') if p.is_file() and 'home' not in p.relative_to(args.output).parts})
    print(json.dumps(checks, indent=2))


if __name__ == '__main__':
    main()
