#!/usr/bin/env python3
"""OAuth failure matrix, using actual Meldra processes and loopback OAuth/MCP.
Before changes these can fail: logout during in-flight refresh resurrects cache;
concurrent processes reuse a rotated refresh token; old clients ignore relogin,
removed cache or config changes; canceled requests persist a later refresh;
OAuth MCP calls ignore tool_timeout_sec > 30; environment client secrets leak
into saved oauth2.Config, removed/rotated environment values are ignored; failed
callback binding destroys valid login; stale requests reach the wire before auth.
No real service or secrets used.
"""
import argparse
import base64
import socket
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import threading
import time
import hashlib
import queue
import urllib.parse
import urllib.request

spec = importlib.util.spec_from_file_location('base', Path(__file__).with_name('mcp-auth-interaction-e2e.py'))
base = importlib.util.module_from_spec(spec)
spec.loader.exec_module(base)


class Handler(base.Handler):
    def do_POST(self):
        server = self.server
        raw = self.rfile.read(int(self.headers.get('Content-Length', 0)))
        self.rfile = io.BytesIO(raw)
        if self.path == '/token' and server.oversized_token:
            server.token_serial += 1
            self.reply({'access_token': 'fixture-access-' + str(server.token_serial), 'refresh_token': 'x' * ((1 << 20) - 512), 'token_type': 'Bearer', 'expires_in': 3600})
            return
        if self.path == '/register':
            server.registrations += 1
        if self.path == '/register' and server.dynamic_secret:
            self.reply(dict(json.loads(raw), client_id='dynamic-fixture', client_secret=server.dynamic_secret, token_endpoint_auth_method='client_secret_basic'), 201)
            return
        if self.path == '/token' and server.expected_secret:
            fields = urllib.parse.parse_qs(raw.decode())
            supplied = fields.get('client_secret', [''])[0]
            header = self.headers.get('Authorization', '')
            if header.startswith('Basic '):
                supplied = urllib.parse.unquote(base64.b64decode(header[6:]).decode().split(':', 1)[1])
            server.secret_matches.append(supplied == server.expected_secret)
            if supplied != server.expected_secret:
                self.reply({'error': 'invalid_client'}, 401)
                return
        if self.path == '/token' and b'grant_type=refresh_token' in raw:
            server.refresh_entered.set()
            server.refresh_release.wait(15)
        if self.path == '/mcp':
            body = json.loads(raw)
            if body.get('method') == 'tools/call':
                server.tool_requests += 1
            if self.headers.get('Authorization') == 'Bearer fixture-access-' + str(server.token_serial) and server.token_serial:
                method = body.get('method')
                if method == 'initialize':
                    if server.fail_initialize:
                        self.reply({'jsonrpc': '2.0', 'id': body['id'], 'error': {'code': -32603, 'message': 'fixture connection failure after token exchange'}})
                        return
                    self.reply({'jsonrpc': '2.0', 'id': body['id'], 'result': {'protocolVersion': '2025-11-25', 'serverInfo': {'name': 'regression', 'version': '1'}, 'capabilities': {'tools': {}}}})
                    return
                if method == 'tools/list':
                    self.reply({'jsonrpc': '2.0', 'id': body['id'], 'result': {'tools': [{'name': 'echo', 'description': 'fixture', 'inputSchema': {'type': 'object', 'properties': {}}}]}})
                    return
                if method == 'tools/call':
                    server.tool_calls += 1
                    if server.tool_delay:
                        time.sleep(server.tool_delay)
                    self.reply({'jsonrpc': '2.0', 'id': body['id'], 'result': {'content': [{'type': 'text', 'text': 'oauth-call-evidence'}]}})
                    return
        if self.path.startswith('/v1'):
            body = json.loads(raw)
            if body.get('tools') and not body.get('stream') and not server.main_calls:
                server.provider_entered.set()
                server.provider_release.wait(15)
        try:
            super().do_POST()
        except (BrokenPipeError, ConnectionResetError):
            pass


base.Handler = Handler


def start(binary, workspace, env):
    return subprocess.Popen([str(binary), '--workspace', str(workspace), '--prompt', 'OAuth regression fixture', '--auto-approve'], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, env=env)


def finish(process, directory, name):
    stdout, stderr = process.communicate('', timeout=50)
    (directory / (name + '.txt')).write_text(stdout + stderr)
    assert 'DATA RACE' not in stderr, stderr
    return process.returncode, stdout + stderr


def prepare(binary, output, name):
    directory, home, workspace, server, env = base.setup(output, name)
    server.refresh_entered, server.refresh_release = threading.Event(), threading.Event()
    server.provider_entered, server.provider_release = threading.Event(), threading.Event()
    server.refresh_release.set()
    server.provider_release.set()
    server.tool_calls = server.tool_requests = 0
    server.expected_secret = server.dynamic_secret = None
    server.secret_matches = []
    server.tool_delay = 0
    server.registrations = 0
    server.fail_initialize = False
    server.oversized_token = False
    config = {'url': server.url + '/mcp', 'oauth': {}}
    base.set_config(home, config)
    assert base.login(binary, env, server, directory) == 0
    return directory, home, workspace, server, env, config


def expire(home):
    path = home / 'mcp-oauth-fixture.json'
    saved = json.loads(path.read_text())
    saved['token']['expiry'] = '2000-01-01T00:00:00Z'
    base.write(path, saved)
    return saved


def run_case(binary, output, name):
    directory, home, workspace, server, env, config = prepare(binary, output, name)
    processes = []
    evidence = {'name': name}
    path = home / 'mcp-oauth-fixture.json'
    try:
        if name in ('concurrent-refresh', 'logout-refresh', 'cancel-refresh'):
            saved = expire(home)
            server.refresh_release.clear()
            if name == 'cancel-refresh':
                base.set_config(home, dict(config, startup_timeout_sec=1))
            first = start(binary, workspace, env)
            processes.append(first)
            assert server.refresh_entered.wait(10), 'refresh never started'
            if name == 'concurrent-refresh':
                second_workspace = directory / 'workspace-second'
                second_workspace.mkdir()
                second = start(binary, second_workspace, env)
                processes.append(second)
                time.sleep(0.3)
                server.refresh_release.set()
                first_code, first_output = finish(first, directory, 'first')
                second_code, second_output = finish(second, directory, 'second')
                assert first_code == second_code == 0, first_output + second_output
                assert 'unavailable' not in first_output + second_output
                grants = [r['grant_type'][0] for r in server.token_requests]
                assert grants.count('refresh_token') == 1, grants
                refreshed = json.loads(path.read_text())
                assert refreshed['token']['refresh_token'] != saved['token']['refresh_token']
                evidence['refresh_requests'] = 1
            elif name == 'logout-refresh':
                logout = subprocess.Popen([str(binary), 'mcp', 'logout', 'fixture'], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, env=env)
                processes.append(logout)
                time.sleep(0.2)
                evidence['logout_waited_for_refresh'] = logout.poll() is None
                server.refresh_release.set()
                stdout, stderr = logout.communicate(timeout=10)
                assert logout.returncode == 0, stdout + stderr
                finish(first, directory, 'chat')
                assert not path.exists(), 'logout cache resurrected after refresh'
                assert evidence['logout_waited_for_refresh'], 'logout did not serialize with refresh'
                evidence['cache_absent_after_logout'] = True
            else:
                # Keep stdin and process alive beyond startup cancellation;
                # detached cleanup must not initiate a second token refresh.
                time.sleep(2)
                server.refresh_release.set()
                time.sleep(0.5)
                current = json.loads(path.read_text())
                assert current['token'] == saved['token'], 'canceled refresh changed token cache'
                finish(first, directory, 'chat')
                evidence['canceled_refresh_not_persisted'] = True
        elif name in ('relogin-generation', 'missing-cache', 'changed-config'):
            server.provider_release.clear()
            first = start(binary, workspace, env)
            processes.append(first)
            assert server.provider_entered.wait(10), 'model request did not reach gate'
            if name == 'relogin-generation':
                before = json.loads(path.read_text()).get('generation')
                assert base.login(binary, env, server, directory, suffix='-again') == 0
                after = json.loads(path.read_text()).get('generation')
                assert before and before != after, 'login generation was not rotated'
            elif name == 'missing-cache':
                path.unlink()
            else:
                base.set_config(home, dict(config, oauth={'scopes': ['changed']}))
            server.provider_release.set()
            code, transcript = finish(first, directory, 'old-session')
            assert server.tool_requests == 0, 'stale session sent remote tool request before authorization'
            assert code != 0, 'stale session continued despite auth change'
            if name == 'missing-cache':
                assert not path.exists(), 'missing cache recreated'
            evidence['stale_session_blocked'] = True
        elif name == 'logout-pending-login':
            login_process = subprocess.Popen([str(binary), 'mcp', 'login', 'fixture'], stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, env=env)
            processes.append(login_process)
            lines = queue.Queue()
            captured = []
            def reader():
                for line in login_process.stdout:
                    captured.append(line)
                    lines.put(line)
            worker = threading.Thread(target=reader, daemon=True)
            worker.start()
            deadline = time.monotonic() + 15
            query = None
            while time.monotonic() < deadline:
                try:
                    line = lines.get(timeout=min(5, max(0.01, deadline-time.monotonic())))
                except queue.Empty:
                    continue
                if line.startswith(server.url + '/authorize?'):
                    query = urllib.parse.parse_qs(urllib.parse.urlparse(line.strip()).query)
                    break
            assert query is not None, captured
            server.challenge = query['code_challenge'][0]
            logout = subprocess.run([str(binary), 'mcp', 'logout', 'fixture'], capture_output=True, text=True, env=env, timeout=10)
            assert logout.returncode == 0 and not path.exists(), logout.stderr
            params = {'code': 'fixture-code', 'state': query['state'][0], 'iss': server.url}
            urllib.request.urlopen(query['redirect_uri'][0] + '?' + urllib.parse.urlencode(params), timeout=5).read()
            login_process.wait(timeout=10)
            worker.join(timeout=2)
            (directory / 'pending-login.txt').write_text(''.join(captured))
            assert login_process.returncode != 0, 'logged-out authorization was accepted'
            assert not path.exists(), 'pending browser login recreated credentials after logout'
            evidence['pending_login_rejected_after_logout'] = True
        elif name in ('relogin-bad-issuer', 'relogin-bad-pkce', 'relogin-cancel', 'relogin-connection-failure'):
            before = path.read_bytes()
            generation_path = Path(str(path) + '.generation')
            before_generation = generation_path.read_bytes()
            server.bad_pkce = name == 'relogin-bad-pkce'
            server.fail_initialize = name == 'relogin-connection-failure'
            assert base.login(binary, env, server, directory, suffix='-failed', bad_issuer=name == 'relogin-bad-issuer', cancel=name == 'relogin-cancel') != 0
            assert path.exists() and path.read_bytes() == before, 'failed re-login replaced or removed the active credentials'
            assert generation_path.read_bytes() == before_generation, 'failed re-login invalidated active clients'
            evidence['failed_relogin_preserved_session'] = True
        elif name in ('oversized-login-cache', 'oversized-refresh-cache'):
            server.dynamic_secret = server.expected_secret = 's' * 4096
            assert base.login(binary, env, server, directory, suffix='-large-client') == 0
            if name == 'oversized-refresh-cache':
                expire(home)
            before = path.read_bytes()
            generation_path = Path(str(path) + '.generation')
            before_generation = generation_path.read_bytes()
            server.oversized_token = True
            if name == 'oversized-login-cache':
                assert base.login(binary, env, server, directory, suffix='-oversized') != 0, 'oversized login reported success'
            else:
                code, transcript = finish(start(binary, workspace, env), directory, 'oversized-refresh')
                assert 'unavailable' in transcript, transcript
            assert path.read_bytes() == before and generation_path.read_bytes() == before_generation, 'oversized session destroyed the existing cache'
            evidence['oversized_cache_rejected_without_write'] = True
        elif name == 'logout-orphaned':
            base.write(home / 'mcp.json', {'mcpServers': {}})
            result = subprocess.run([str(binary), 'mcp', 'logout', 'fixture'], capture_output=True, text=True, env=env, timeout=10)
            assert result.returncode == 0 and not path.exists(), result.stderr
            evidence['orphaned_cache_removed'] = True
        elif name == 'chat-reauthorization':
            registrations = server.registrations
            server.token_serial += 1  # Reject the valid but revoked cached token.
            code, transcript = finish(start(binary, workspace, env), directory, 'reauthorization')
            assert 'unavailable' in transcript, transcript
            assert server.registrations == registrations, 'chat startup dynamically registered an unused OAuth client'
            evidence['implicit_registration_rejected'] = True
        elif name in ('environment-secret', 'dynamic-secret'):
            secrets = ['environment-private-login-4321', 'environment-private-rotated-9876']
            if name == 'environment-secret':
                server.expected_secret = secrets[0]
                env['MCP_FIXTURE_CLIENT_SECRET'] = secrets[0]
                config['oauth'] = {'client_id': 'registered-fixture', 'client_secret_env': 'MCP_FIXTURE_CLIENT_SECRET', 'issuer': server.url}
            else:
                server.dynamic_secret = server.expected_secret = 'dynamic-client-issued-secret'
            base.set_config(home, config)
            assert base.login(binary, env, server, directory, suffix='-secret') == 0
            saved = json.loads(path.read_text())
            if name == 'environment-secret':
                assert not saved['config']['ClientSecret'], 'environment secret persisted in oauth2.Config'
                expire(home)
                before = path.read_bytes()
                missing_env = dict(env)
                del missing_env['MCP_FIXTURE_CLIENT_SECRET']
                count = len(server.token_requests)
                generation_path = Path(str(path) + '.generation')
                before_generation = generation_path.read_bytes()
                failed_login = subprocess.run([str(binary), 'mcp', 'login', 'fixture'], capture_output=True, text=True, env=missing_env, timeout=10)
                assert failed_login.returncode != 0 and 'secret environment variable is missing' in failed_login.stderr, failed_login.stderr
                assert path.exists() and path.read_bytes() == before and generation_path.read_bytes() == before_generation, 'missing secret destroyed the active login'
                code, transcript = finish(start(binary, workspace, missing_env), directory, 'missing-secret')
                assert 'secret environment variable is missing' in transcript, transcript
                assert len(server.token_requests) == count and path.read_bytes() == before, 'missing environment secret reused persisted credentials'
                env['MCP_FIXTURE_CLIENT_SECRET'] = server.expected_secret = secrets[1]
            else:
                assert saved['config']['ClientSecret'] == server.dynamic_secret, 'DCR client secret lost'
                expire(home)
            code, transcript = finish(start(binary, workspace, env), directory, 'secret-refresh')
            assert code == 0 and 'unavailable' not in transcript, transcript
            assert server.secret_matches and all(server.secret_matches), 'client authentication used stale secret'
            assert server.token_requests[-1]['grant_type'] == ['refresh_token']
            if name == 'environment-secret':
                for artifact in directory.rglob('*'):
                    if artifact.is_file():
                        content = artifact.read_bytes()
                        assert all(secret.encode() not in content for secret in secrets), 'environment secret persisted: ' + str(artifact)
                evidence['environment_secret_not_persisted'] = True
                evidence['refresh_used_rotated_environment'] = True
            else:
                evidence['dynamic_client_secret_retained'] = True
        elif name == 'occupied-callback':
            reservation = socket.socket()
            reservation.bind(('127.0.0.1', 0))
            callback_port = reservation.getsockname()[1]
            reservation.close()
            config['oauth'] = {'callback_port': callback_port}
            base.set_config(home, config)
            assert base.login(binary, env, server, directory, suffix='-fixed-port') == 0
            before = path.read_bytes()
            generation_path = Path(str(path) + '.generation')
            before_generation = generation_path.read_bytes()
            occupied = socket.socket()
            occupied.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            occupied.bind(('127.0.0.1', callback_port))
            occupied.listen()
            try:
                result = subprocess.run([str(binary), 'mcp', 'login', 'fixture'], capture_output=True, text=True, env=env, timeout=10)
                (directory / 'occupied-port.txt').write_text(result.stdout + result.stderr)
                assert result.returncode != 0, 'occupied callback port accepted'
                assert path.read_bytes() == before and generation_path.read_bytes() == before_generation, 'callback bind failure changed valid session'
            finally:
                occupied.close()
            code, transcript = finish(start(binary, workspace, env), directory, 'retained-session')
            assert code == 0 and 'unavailable' not in transcript, transcript
            evidence['bind_failure_preserved_session'] = True
        elif name == 'long-tool':
            server.tool_delay = 31
            base.set_config(home, dict(config, tool_timeout_sec=40))
            first = start(binary, workspace, env)
            processes.append(first)
            code, transcript = finish(first, directory, 'chat')
            assert code == 0 and 'E2E complete' in transcript, transcript
            assert server.tool_calls == 1
            evidence['tool_completed_after_seconds'] = 31
        evidence['passed'] = True
        base.write(directory / 'evidence.json', evidence)
        return evidence
    finally:
        server.refresh_release.set()
        server.provider_release.set()
        for process in processes:
            if process.poll() is None:
                process.kill()
                process.wait()
        server.shutdown()
        server.server_close()


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--binary', required=True, type=Path)
    parser.add_argument('--output', required=True, type=Path)
    cases = ['concurrent-refresh', 'logout-refresh', 'cancel-refresh', 'relogin-generation', 'missing-cache', 'changed-config', 'logout-pending-login', 'environment-secret', 'dynamic-secret', 'occupied-callback', 'long-tool', 'relogin-bad-issuer', 'relogin-bad-pkce', 'relogin-cancel', 'relogin-connection-failure', 'logout-orphaned', 'chat-reauthorization', 'oversized-login-cache', 'oversized-refresh-cache']
    parser.add_argument('--case', choices=cases)
    args = parser.parse_args()
    args.binary, args.output = args.binary.resolve(), args.output.resolve()
    args.output.mkdir()
    checks = []
    for name in ([args.case] if args.case else cases):
        checks.append(run_case(args.binary, args.output, name))
        base.write(args.output / 'report.json', checks)
        print(name + ': passed', flush=True)
    base.write(args.output / 'checksums.json', {str(p.relative_to(args.output)): hashlib.sha256(p.read_bytes()).hexdigest() for p in args.output.rglob('*') if p.is_file() and 'home' not in p.relative_to(args.output).parts})


if __name__ == '__main__':
    main()
