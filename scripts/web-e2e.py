#!/usr/bin/env python3
"""Real-binary web tools verification, with repeatable offline evidence."""
import argparse
import hashlib
import http.server
import importlib.util
import json
import os
from pathlib import Path
import select
import signal
import ssl
import subprocess
import threading
import time
import traceback

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location('capability', Path(__file__).with_name('capability-e2e.py'))
capability = importlib.util.module_from_spec(spec)
spec.loader.exec_module(capability)
Case, dump, write = capability.Case, capability.dump, capability.write
KEY = 'deepseek-offline-key-never-persist'

OVERLAY = r'''package app
import ("context"; "crypto/tls"; "crypto/x509"; "fmt"; "net"; "net/netip"; "os"; "strings"; "sync/atomic"; "time")
var fixtureLookups atomic.Int32
func fixtureLog(line string) { f, err := os.OpenFile(os.Getenv("WEB_FIXTURE_LOG"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600); if err == nil { defer f.Close(); fmt.Fprintln(f, line) } }
func webLookup(ctx context.Context, host string) ([]netip.Addr,error) {
 fixtureLog("lookup "+host)
 if host == "mixed.example" { return []netip.Addr{netip.MustParseAddr("93.184.216.34"),netip.MustParseAddr("127.0.0.1")},nil }
 if host == "private.example" || host == "rebind.example" && fixtureLookups.Add(1)>1 { return []netip.Addr{netip.MustParseAddr("10.0.0.1")},nil }
 if host == "dns-error.example" { return nil,fmt.Errorf("fixture DNS failure") }
 if host == "empty.example" { return nil,nil }
 return []netip.Addr{netip.MustParseAddr("93.184.216.34")},nil
}
func webDial(ctx context.Context, network,address string) (net.Conn,error) {
 fixtureLog("dial "+address)
 host,port,err:=net.SplitHostPort(address); if err!=nil || host!="93.184.216.34" { return nil,fmt.Errorf("fixture refuses unvalidated destination") }
 if port=="443" { port=os.Getenv("WEB_FIXTURE_TLS_PORT") }
 d:=net.Dialer{Timeout:time.Second}; return d.DialContext(ctx,network,net.JoinHostPort("127.0.0.1",port))
}
func webTLSConfig() *tls.Config {
 pem,err:=os.ReadFile(os.Getenv("WEB_FIXTURE_CERT")); if err!=nil { panic(err) }
 roots:=x509.NewCertPool(); if !roots.AppendCertsFromPEM(pem) { panic("invalid fixture certificate") }
 return &tls.Config{MinVersion:tls.VersionTLS12,RootCAs:roots,ServerName:strings.TrimSpace("api.deepseek.com")}
}
'''

class Origin:
    def __init__(self, output):
        self.calls = []
        self.lock = threading.Lock()
        origin = self
        class Handler(http.server.BaseHTTPRequestHandler):
            def log_message(self, *_args):
                pass
            def reply(self, status, body=b'', mime='text/plain', headers=None):
                if isinstance(body, str):
                    body = body.encode()
                self.send_response(status)
                if mime:
                    self.send_header('Content-Type', mime)
                self.send_header('Content-Length', str(len(body)))
                for key, value in (headers or {}).items():
                    self.send_header(key, value)
                self.end_headers()
                try:
                    self.wfile.write(body)
                except (BrokenPipeError, ConnectionResetError):
                    pass
            def do_GET(self):
                with origin.lock:
                    origin.calls.append({'path': self.path, 'host': self.headers.get('Host'), 'authorization': self.headers.get('Authorization'), 'cookie': self.headers.get('Cookie')})
                if self.path.startswith('/redirect/'):
                    count = int(self.path.rsplit('/', 1)[1])
                    self.reply(302, headers={'Location': '/redirect/' + str(count - 1) if count else '/html', 'Set-Cookie': 'secret=must-not-forward'})
                elif self.path == '/private-redirect':
                    self.reply(302, headers={'Location': 'http://127.0.0.1:' + str(origin.http.server_port) + '/must-not-reach'})
                elif self.path == '/credential-redirect':
                    self.reply(302, headers={'Location': 'http://user:password@public.example:' + str(origin.http.server_port) + '/must-not-reach'})
                elif self.path == '/html':
                    self.reply(200, '<html><head><title>Fixture title</title><style>HIDDEN_STYLE</style></head><body><h1>VISIBLE_MARKER &amp; text</h1><script>HIDDEN_SCRIPT</script><!-- HIDDEN_COMMENT --><p>Next paragraph</p></body></html>', 'text/html; charset=utf-8')
                elif self.path == '/json':
                    self.reply(200, '{"marker":"JSON_MARKER"}', 'application/problem+json')
                elif self.path == '/big':
                    self.reply(200, b'Z' * ((1 << 20) + 30000))
                elif self.path == '/binary':
                    self.reply(200, b'BINARY_SECRET', 'application/octet-stream')
                elif self.path == '/missing-mime':
                    self.reply(200, b'NO_MIME', None)
                elif self.path == '/error':
                    self.reply(404, 'NOT_FOUND_MARKER')
                elif self.path == '/drop':
                    self.connection.shutdown(2)
                    self.connection.close()
                elif self.path == '/slow':
                    time.sleep(3)
                    self.reply(200, 'slow')
                else:
                    self.reply(200, 'TEXT_MARKER')
            def do_POST(self):
                body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
                with origin.lock:
                    origin.calls.append({'path': self.path, 'body': body, 'authorized_with_independent_key': self.headers.get('Authorization') == 'Bearer ' + KEY})
                mode = body.get('query', '') or body.get('url', '').rsplit('/', 1)[-1]
                if mode in ('401', '429', '500'):
                    self.reply(int(mode), KEY + ' do not leak', 'application/json')
                elif mode == 'redirect':
                    self.reply(302, headers={'Location': 'https://public.example/credential-exfiltration'})
                elif mode == 'malformed':
                    self.reply(200, 'not json ' + KEY, 'application/json')
                elif mode == 'missing':
                    self.reply(200, '{}', 'application/json')
                elif mode == 'bad-results':
                    self.reply(200, '{"results":[{}]}', 'application/json')
                elif mode == 'big':
                    self.reply(200, json.dumps({'title': 'big', 'content': 'X' * (1 << 20)}), 'application/json')
                elif self.path.endswith('/fetch'):
                    self.reply(200, json.dumps({'title': 'Hosted title', 'content': '# HOSTED_MARKER\nMarkdown ' + KEY}), 'application/json')
                else:
                    self.reply(200, json.dumps({'results': [{'title': 'Result', 'url': 'https://source.example/article', 'snippet': 'SNIPPET_MARKER', 'summary': 'SUMMARY_MARKER', 'content': 'CONTENT_MARKER' if body.get('need_content') else '', 'rerank_score': 2.4, 'site_name': None, 'site_icon': None, 'date': None, 'authority': 3}]}), 'application/json')
        self.http = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        self.https = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        self.cert = output / 'fixture-cert.pem'
        private = output / 'fixture-key.pem'
        subprocess.run(['openssl', 'req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-keyout', str(private), '-out', str(self.cert), '-days', '1', '-subj', '/CN=api.deepseek.com', '-addext', 'subjectAltName=DNS:api.deepseek.com'], check=True, capture_output=True)
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.load_cert_chain(self.cert, private)
        self.https.socket = context.wrap_socket(self.https.socket, server_side=True)
        for server in (self.http, self.https):
            threading.Thread(target=server.serve_forever, daemon=True).start()
    def url(self, path, host='public.example'):
        return f'http://{host}:{self.http.server_port}{path}'
    def attach(self, case, hosted=False):
        case.env.update(WEB_FIXTURE_CERT=str(self.cert), WEB_FIXTURE_TLS_PORT=str(self.https.server_port), WEB_FIXTURE_LOG=str(case.directory / 'network.log'), MELDRA_DISABLE_UPDATE_CHECK='1', GORACE='atexit_sleep_ms=0')
        case.env['HTTP_PROXY'] = case.env['HTTPS_PROXY'] = 'http://127.0.0.1:1'
        if hosted:
            case.env['DEEPSEEK_API_KEY'] = KEY
            with (case.state / 'config.toml').open('a') as stream:
                stream.write('web.fetch_backend="deepseek"\nweb.search_backend="deepseek"\n')
    def close(self):
        for server in (self.http, self.https):
            server.shutdown()
            server.server_close()

def calls(case):
    records = case.audit()
    return [call for record in records for call in record['tool_calls']], records

def fixture_binary(output):
    replacement = write(output / 'web_network_fixture.go', OVERLAY)
    overlay = output / 'overlay.json'
    dump(overlay, {'Replace': {str(ROOT / 'internal/app/web_network.go'): str(replacement)}})
    binary = output / 'meldra-web-fixture'
    result = subprocess.run(['go', 'build', '-race', '-overlay', str(overlay), '-o', str(binary), '.'], cwd=ROOT, capture_output=True, text=True)
    write(output / 'fixture-build.txt', result.stdout + result.stderr)
    assert result.returncode == 0, result.stderr
    dump(output / 'fixture-provenance.json', {'binary_sha256': hashlib.sha256(binary.read_bytes()).hexdigest(), 'overlay': str(overlay), 'boundary': 'Only DNS/raw dial/TLS trust replaced; all public-address validation and policy execute unchanged.'})
    return binary

def security(binary, output, origin):
    bad = ['file:///etc/passwd', 'http://user:secret@public.example/', 'https://public.example/#fragment', 'http://localhost/', 'http://service.internal/', 'http://127.0.0.1/', 'http://10.2.3.4/', 'http://172.20.0.1/', 'http://192.168.1.1/', 'http://169.254.169.254/', 'http://100.64.0.1/', 'http://0.0.0.0/', 'http://224.0.0.1/', 'http://198.18.0.1/', 'http://[::1]/', 'http://[::ffff:127.0.0.1]/', 'http://[fc00::1]/', 'http://[fe80::1]/', 'http://[2002:7f00:1::]/', 'http://public.example:0/']
    with Case(binary, output, 'production-address-denials', [{'calls': [('web_fetch', {'url': url}) for url in bad]}, {}]) as case:
        case.chat(flags=['--auto-approve'])
        ledger, records = calls(case)
        assert len(ledger) == len(bad) and all(call['result']['status'] == 'failed' and call['effect'] == 'external_read' for call in ledger), ledger
        catalog = [item['name'] for item in case.requests[0]['tools']]
        assert 'web_fetch' in catalog and 'web_search' not in catalog, catalog
        assert not any(record.get('approvals') for record in records)
        case.complete()

def builtin(binary, output, origin):
    scenarios = [('html', '/html', 'succeeded', 'VISIBLE_MARKER'), ('json', '/json', 'succeeded', 'JSON_MARKER'), ('text', '/text', 'succeeded', 'TEXT_MARKER'), ('five-redirects', '/redirect/4', 'succeeded', 'VISIBLE_MARKER'), ('six-redirects', '/redirect/5', 'failed', 'five exceeded'), ('private-redirect', '/private-redirect', 'failed', 'non-public'), ('credential-redirect', '/credential-redirect', 'failed', 'without credentials'), ('binary', '/binary', 'failed', 'MIME'), ('missing-mime', '/missing-mime', 'failed', 'MIME'), ('http-error', '/error', 'failed', 'NOT_FOUND_MARKER'), ('large', '/big', 'succeeded', 'truncated')]
    for name, path, status, marker in scenarios:
        start = len(origin.calls)
        with Case(binary, output, 'builtin-' + name, [{'calls': [('web_fetch', {'url': origin.url(path)})]}, {}]) as case:
            origin.attach(case)
            case.chat(flags=['--auto-approve', '--mode', 'plan'])
            ledger, records = calls(case)
            call = ledger[0]
            assert call['result']['status'] == status, call
            assert marker in json.dumps(call['result']), call
            assert call['effect'] == 'external_read'
            assert records[0]['approvals'][0]['decision'] == 'approved'
            assert records[0]['approvals'][0]['operation'] == 'external_read'
            assert call['result'].get('artifacts'), call
            artifact = call['result']['artifacts'][0]
            payload = (case.state / 'tasks/artifacts' / artifact['sha256']).read_bytes()
            assert hashlib.sha256(payload).hexdigest() == artifact['sha256']
            assert artifact['sha256'] in call['result']['output']
            if name == 'html':
                assert b'HIDDEN_SCRIPT' not in payload and b'HIDDEN_STYLE' not in payload and b'HIDDEN_COMMENT' not in payload
            if name == 'large':
                assert call['result']['truncated'] and len(payload) <= (1 << 20) + 1024
            if name == 'five-redirects':
                assert len(origin.calls) - start == 6 and 'final_url' in call['result']['output'] and '/html' in call['result']['output']
            assert all(item.get('authorization') is None and item.get('cookie') is None for item in origin.calls[start:])
            case.complete()

def approval_and_dns(binary, output, origin):
    for permissions in ('interactive', 'workspace-edit'):
        with Case(binary, output, 'approval-' + permissions, [{'calls': [('web_fetch', {'url': origin.url('/text')})]}, {}]) as case:
            origin.attach(case)
            before = len(origin.calls)
            case.chat(flags=['--permissions', permissions], stdin='n\n')
            ledger, records = calls(case)
            assert ledger[0]['result']['status'] == 'declined' and not (case.directory / 'network.log').exists()
            assert len(origin.calls) == before
            case.complete()
    for host in ('private.example', 'mixed.example', 'dns-error.example', 'empty.example', 'rebind.example'):
        with Case(binary, output, 'dns-' + host.split('.')[0], [{'calls': [('web_fetch', {'url': origin.url('/redirect/0' if host == 'rebind.example' else '/text', host)})]}, {}]) as case:
            origin.attach(case)
            before = len(origin.calls)
            result = case.chat(flags=['--auto-approve'], success=host != 'rebind.example')
            ledger, records = calls(case)
            network = (case.directory / 'network.log').read_text()
            assert network.count('dial ') == (1 if host == 'rebind.example' else 0), network
            assert len(origin.calls) - before == (1 if host == 'rebind.example' else 0)
            assert ledger[0]['result']['status'] in ('failed', 'unknown')
            if host != 'rebind.example':
                case.complete()
    with Case(binary, output, 'lost-response', [{'calls': [('web_fetch', {'url': origin.url('/drop')})]}]) as case:
        origin.attach(case)
        case.chat(flags=['--auto-approve'], success=False)
        ledger, records = calls(case)
        assert ledger[0]['result']['status'] == 'unknown', ledger
        assert len(case.requests) == 1
        case.complete()
    with Case(binary, output, 'cancelled-response', [{'calls': [('web_fetch', {'url': origin.url('/slow')})]}]) as case:
        origin.attach(case)
        before = len(origin.calls)
        process = subprocess.Popen([str(binary), '--workspace', str(case.workspace), '--prompt', 'Fetch fixture', '--auto-approve'], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=case.env)
        case.processes.append(process)
        deadline = time.monotonic() + 10
        while len(origin.calls) == before:
            assert time.monotonic() < deadline, 'request was not dispatched'
            time.sleep(0.02)
        process.send_signal(signal.SIGTERM)
        stdout, stderr = process.communicate(timeout=10)
        write(case.directory / 'cancel.stdout.txt', stdout)
        write(case.directory / 'cancel.stderr.txt', stderr)
        ledger, records = calls(case)
        assert ledger[0]['result']['status'] == 'unknown', ledger
        assert len(case.requests) == 1 and len(origin.calls) == before + 1
        result = case.run(['task', 'resume', records[0]['task']['id'], '--prompt', 'Resume explicitly'], success=False, label='resume')
        assert 'unresolved' in result.stderr or 'unknown' in result.stderr, result.stderr
        assert len(origin.calls) == before + 1 and len(case.requests) == 1
        case.complete()

def hosted(binary, output, origin):
    for name in ('good', '401', '429', '500', 'redirect', 'malformed', 'missing', 'big'):
        with Case(binary, output, 'deepseek-' + name, [{'calls': [('web_fetch', {'url': 'https://public.example/' + name})]}, {}]) as case:
            origin.attach(case, True)
            case.chat(flags=['--auto-approve'])
            ledger, records = calls(case)
            call = ledger[0]
            assert call['result']['status'] == ('succeeded' if name == 'good' else 'failed'), call
            assert KEY not in json.dumps(records) and KEY not in json.dumps(case.requests)
            assert 'Third-party disclosure' in json.dumps(records[0]['approvals'])
            if name == 'good':
                assert 'HOSTED_MARKER' in call['result']['output'] and '"final_url":null' in call['result']['output'] and '"api_status_code":200' in call['result']['output']
            if name == 'big':
                assert call['result']['truncated']
            assert origin.calls[-1]['authorized_with_independent_key']
            case.complete()
    for query, count, status in [('valid query', None, 'succeeded'), ('x' * 129, 10, 'failed'), ('   ', 10, 'failed'), ('query', 0, 'failed'), ('query', 51, 'failed'), ('bad-results', 10, 'failed')]:
        name = 'search-' + str(count) + '-' + str(len(query))
        with Case(binary, output, name, [{'calls': [('web_search', {'query': query, 'count': count, 'need_content': True})]}, {}]) as case:
            origin.attach(case, True)
            case.chat(flags=['--auto-approve'])
            ledger, records = calls(case)
            assert ledger[0]['result']['status'] == status, ledger
            if status == 'succeeded':
                assert 'CONTENT_MARKER' in ledger[0]['result']['output']
                assert origin.calls[-1]['body'] == {'query': query, 'count': 10, 'need_content': True}
            case.complete()

def config(binary, output, origin):
    for index, setting in enumerate(('web.fetch_backend="invalid"', 'web.search_backend="builtin"', 'web.fetch_backend="deepseek"', 'web.search_backend="deepseek"', 'web.seach_backend="deepseek"')):
        with Case(binary, output, 'config-' + str(index)) as case:
            with (case.state / 'config.toml').open('a') as stream:
                stream.write(setting + '\n')
            result = case.chat(success=False)
            assert not case.requests and ('backend' in result.stderr or 'DEEPSEEK_API_KEY' in result.stderr)
    with Case(binary, output, 'config-show-redaction') as case:
        origin.attach(case, True)
        result = case.run(['config', 'show'])
        assert 'deepseek_api_key=********' in result.stdout and KEY not in result.stdout + result.stderr
    with Case(binary, output, 'config-file-key', [{'calls': [('web_fetch', {'url': 'https://public.example/good'})]}, {}]) as case:
        origin.attach(case, True)
        del case.env['DEEPSEEK_API_KEY']
        write(case.state / 'credentials.env', 'DEEPSEEK_API_KEY="' + KEY + '"\n').chmod(0o600)
        # Exercise the [web] section, independently from dotted top-level keys.
        config_path = case.state / 'config.toml'
        config_path.write_text(config_path.read_text().replace('web.fetch_backend=', '[web]\nfetch_backend=').replace('web.search_backend=', 'search_backend='))
        case.chat(flags=['--auto-approve'])
        ledger, records = calls(case)
        assert ledger[0]['result']['status'] == 'succeeded' and origin.calls[-1]['authorized_with_independent_key']
        case.complete()

def mcp_approval(binary, output, origin):
    for latest in (False, True):
        for accept in (False, True):
            with Case(binary, output, f'mcp-{latest}-{accept}') as case:
                origin.attach(case, True)
                process = subprocess.Popen([str(binary), 'mcp', 'serve', '--workspace', str(case.workspace), '--mode', 'plan'], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=case.env)
                case.processes.append(process)
                transcript, buffer, sequence = [], bytearray(), 0
                def send(value):
                    transcript.append({'direction': 'client', 'message': value})
                    process.stdin.write((json.dumps(value) + '\n').encode())
                    process.stdin.flush()
                def receive():
                    deadline = time.monotonic() + 15
                    while b'\n' not in buffer:
                        assert time.monotonic() < deadline, 'MCP response timeout'
                        if select.select([process.stdout], [], [], 0.1)[0]:
                            chunk = os.read(process.stdout.fileno(), 65536)
                            assert chunk, 'MCP closed stdout'
                            buffer.extend(chunk)
                    line, _, rest = buffer.partition(b'\n')
                    buffer[:] = rest
                    value = json.loads(line)
                    transcript.append({'direction': 'server', 'message': value})
                    return value
                def request(method, params):
                    nonlocal sequence
                    sequence += 1
                    identity = sequence
                    if latest:
                        params['_meta'] = {'io.modelcontextprotocol/protocolVersion': '2026-07-28', 'io.modelcontextprotocol/clientInfo': {'name': 'web-e2e', 'version': '1'}, 'io.modelcontextprotocol/clientCapabilities': {'elicitation': {'form': {}}}}
                    send({'jsonrpc': '2.0', 'id': identity, 'method': method, 'params': params})
                    while True:
                        value = receive()
                        if value.get('method') == 'elicitation/create':
                            assert 'Third-party disclosure' in json.dumps(value) and 'api.deepseek.com' in json.dumps(value)
                            send({'jsonrpc': '2.0', 'id': value['id'], 'result': {'action': 'accept' if accept else 'decline', 'content': {'approve': accept}}})
                        elif value.get('id') == identity:
                            return value
                try:
                    request('server/discover' if latest else 'initialize', {'protocolVersion': '2025-11-25', 'clientInfo': {'name': 'web-e2e', 'version': '1'}, 'capabilities': {'elicitation': {'form': {}}}})
                    if not latest:
                        send({'jsonrpc': '2.0', 'method': 'notifications/initialized'})
                    catalog = request('tools/list', {})
                    assert 'web_search' in json.dumps(catalog) and 'web_fetch' in json.dumps(catalog)
                    params = {'name': 'web_fetch', 'arguments': {'url': 'https://public.example/good'}}
                    before = len(origin.calls)
                    result = request('tools/call', params)['result']
                    if latest:
                        assert result['resultType'] == 'input_required' and 'Third-party disclosure' in json.dumps(result)
                        assert len(origin.calls) == before
                        params.update(requestState=result['requestState'], inputResponses={'approval': {'action': 'accept' if accept else 'decline', 'content': {'approve': accept}}})
                        result = request('tools/call', params)['result']
                        replay = request('tools/call', params)['result']
                        assert replay.get('isError')
                        altered = {'name': 'web_fetch', 'arguments': {'url': 'https://public.example/good'}}
                        challenge = request('tools/call', altered)['result']
                        altered.update(requestState=challenge['requestState'], inputResponses={'approval': {'action': 'accept', 'content': {'approve': True}}})
                        altered['arguments'] = {'url': 'https://changed.example/good'}
                        assert request('tools/call', altered)['result'].get('isError')
                    assert bool(result.get('isError')) != accept, result
                    assert len(origin.calls) - before == (1 if accept else 0)
                    process.stdin.close()
                    process.wait(timeout=10)
                    errors = process.stderr.read().decode()
                    write(case.directory / 'mcp.stderr.txt', errors)
                    assert not errors, errors
                    ledger, records = calls(case)
                    assert ledger[0]['effect'] == 'external_read'
                    assert ledger[0]['result']['status'] == ('succeeded' if accept else 'declined')
                    assert records[0]['approvals'][0]['operation'] == 'external_read'
                    assert KEY not in json.dumps(records)
                finally:
                    dump(case.directory / 'mcp-transcript.json', transcript)

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', type=Path, required=True)
    parser.add_argument('--output', type=Path, default=Path('.artifacts/web-e2e'))
    args = parser.parse_args()
    args.output = args.output.resolve()
    args.output.mkdir(parents=True, exist_ok=True)
    binary = args.binary.resolve()
    origin = Origin(args.output)
    report = []
    try:
        fixture = fixture_binary(args.output)
        for name, function, target in [('production-security', security, binary), ('configuration', config, fixture), ('builtin', builtin, fixture), ('approval-dns-cancellation', approval_and_dns, fixture), ('hosted', hosted, fixture), ('mcp-approval', mcp_approval, fixture)]:
            started = time.monotonic()
            try:
                function(target, args.output, origin)
                report.append({'name': name, 'passed': True, 'seconds': round(time.monotonic() - started, 2)})
            except Exception:
                report.append({'name': name, 'passed': False, 'error': traceback.format_exc()})
                raise
            finally:
                dump(args.output / 'report.json', report)
    finally:
        origin.close()
        dump(args.output / 'origin-requests.json', origin.calls)
    print(json.dumps(report, indent=2))

if __name__ == '__main__':
    main()
