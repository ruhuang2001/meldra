#!/usr/bin/env python3
"""Real-binary regression evidence for exact stream approval review findings."""
import argparse
import hashlib
import http.server
import importlib.util
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import threading
import time
import traceback

spec = importlib.util.spec_from_file_location('capability', Path(__file__).with_name('capability-e2e.py'))
capability = importlib.util.module_from_spec(spec)
spec.loader.exec_module(capability)
Case, write, dump = capability.Case, capability.write, capability.dump


def approval(result):
    assert result.returncode == 3, (result.returncode, result.stdout, result.stderr)
    return next(event for event in map(json.loads, result.stdout.splitlines()) if event['kind'] == 'approval_required')


def resume(case, event, **kwargs):
    return case.run(['task', 'resume', event['task_id'], '--approve-detail-hash', event['data']['detail_hash']], **kwargs)


def undo(binary, output, mutation='none'):
    steps = [{'calls': [('apply_patch', {'changes': [{'path': 'before.txt', 'old_str': 'BEFORE', 'new_str': 'AFTER'},
                                                    {'path': 'created/nested.txt', 'old_str': '', 'new_str': 'CREATED'}]})]},
             {'calls': [('undo_last_change', {})]}]
    with Case(binary, output, 'undo-' + mutation, steps) as case:
        write(case.workspace / 'before.txt', 'BEFORE')
        first = approval(case.chat(flags=['--output-format', 'stream-json'], success=False))
        second = approval(resume(case, first, success=False, label='apply-approved'))
        assert (case.workspace / 'before.txt').read_text() == 'AFTER'
        assert (case.workspace / 'created/nested.txt').read_text() == 'CREATED'
        if mutation == 'file':
            write(case.workspace / 'before.txt', 'USER EDIT')
        elif mutation == 'rules':
            write(case.workspace / 'AGENTS.md', 'New project rules.')
        elif mutation in ('missing', 'corrupt'):
            record = case.audit()[0]
            pending = next(a for a in record['approvals'] if a['decision'] == 'pending')
            ref = pending['detail']['undo_evidence']
            artifact = case.state / 'tasks/artifacts' / ref['id']
            if mutation == 'missing':
                artifact.unlink()
            else:
                artifact.write_bytes(b'corrupt undo evidence')
        if mutation == 'none':
            case.steps.append({})
        result = resume(case, second, success=mutation == 'none', label='undo-approved')
        if mutation == 'none':
            assert (case.workspace / 'before.txt').read_text() == 'BEFORE'
            assert not (case.workspace / 'created').exists()
        else:
            assert result.returncode == 1 and len(case.requests) == 2
            assert (case.workspace / 'before.txt').read_text() == ('USER EDIT' if mutation == 'file' else 'AFTER')
            assert (case.workspace / 'created/nested.txt').exists()
        case.audit()
        case.complete()


MCP_SCRIPT = r'''import json, os, sys
from pathlib import Path
label, log = sys.argv[1:3]
def record(method):
    with Path(log).open('a') as f: f.write(json.dumps({'method': method, 'label': label})+'\n')
record('startup')
for line in sys.stdin:
    request = json.loads(line)
    method = request.get('method')
    if 'id' not in request: continue
    if method == 'server/discover':
        print(json.dumps({'jsonrpc':'2.0','id':request['id'],'error':{'code':-32601,'message':'legacy'}}),flush=True)
        continue
    result = {}
    if method == 'initialize': result = {'protocolVersion':'2025-03-26','capabilities':{'tools':{},'resources':{}},'serverInfo':{'name':'stream-review','version':'1'}}
    elif method == 'tools/list': result = {'tools':[{'name':'echo','description':'Fixture','inputSchema':{'type':'object','properties':{'mode':{'type':'string'}}}}]}
    elif method == 'tools/call':
        record(method)
        result = {'content':[{'type':'text','text':'MCP_REPLAY_COMPLETE'}]}
    elif method == 'resources/read':
        record(method)
        result = {'contents':[{'uri':request['params']['uri'],'text':'RESOURCE_REPLAY_COMPLETE'}]}
    print(json.dumps({'jsonrpc':'2.0','id':request['id'],'result':result}),flush=True)
'''


def mcp(binary, output, variant):
    is_catalog = variant == 'catalog'
    args = {'action': 'read', 'uri': 'fixture://resource'} if is_catalog else ({'mode': ' spaced '} if variant == 'tool' else {})
    name = 'mcp_resources__fixture' if is_catalog else 'mcp__fixture__echo'
    with Case(binary, output, 'mcp-' + variant, [{'calls': [(name, args)]}]) as case:
        script = write(case.directory / 'server.py', MCP_SCRIPT)
        log = case.directory / 'mcp-operations.jsonl'
        config = {'command': sys.executable, 'args': [str(script), 'original', str(log)],
                  'env': {'FIXTURE_CONFIG_SECRET': 'PRIVATE_MCP_CONFIG_CANARY'}, 'env_vars': ['FIXTURE_INHERITED']}
        relative = variant in ('relative','relative-changed')
        if relative:
            executable = write(case.workspace / 'server', '#!/bin/sh\nexec '+sys.executable+' "$@"\n')
            executable.chmod(0o700)
            config['command'] = './server'
        case.env['FIXTURE_INHERITED'] = 'original'
        dump(case.state / 'mcp.json', {'mcpServers': {'fixture': config}})
        (case.state / 'mcp.json').chmod(0o600)
        if relative:
            first = approval(case.run(['--workspace', str(case.workspace), '--prompt', 'Review relative command.', '--output-format', 'stream-json'],
                                      success=False, label='relative-first', run_cwd=case.directory))
        else:
            first = approval(case.chat(flags=['--output-format', 'stream-json'], success=False))
        if variant == 'args': config['args'][1] = 'replacement'
        if variant == 'env': config['env']['FIXTURE_CONFIG_SECRET'] = 'CHANGED_SECRET_CANARY'
        if variant == 'inherited': case.env['FIXTURE_INHERITED'] = 'replacement'
        if variant == 'executable':
            replacement = write(case.directory / 'replacement', '#!/bin/sh\nexec '+sys.executable+' "$@"\n')
            replacement.chmod(0o700)
            config['command'] = str(replacement)
        if variant == 'fifo':
            fifo = case.directory / 'executable-fifo'
            os.mkfifo(fifo,0o700)
            config['command'] = str(fifo)
        if variant == 'relative-changed':
            write(executable, '#!/bin/sh\n# changed executable content\nexec '+sys.executable+' "$@"\n')
            executable.chmod(0o700)
        dump(case.state / 'mcp.json', {'mcpServers': {'fixture': config}})
        success = variant in ('tool', 'catalog', 'relative')
        if success: case.steps.append({})
        result = resume(case, first, success=success, label='mcp-resume', **({'run_cwd':case.directory} if relative else {}))
        records = case.audit()
        operations = [json.loads(line) for line in log.read_text().splitlines()]
        calls = [row for row in operations if row['method'] in ('tools/call', 'resources/read')]
        if success:
            assert len(calls) == 1 and calls[0]['label'] == 'original', operations
        else:
            assert result.returncode == 1 and not calls and len(case.requests) == 1, (result.stdout, operations)
            assert len([row for row in operations if row['method'] == 'startup']) == 1, 'changed server started before rejection'
        assert 'PRIVATE_MCP_CONFIG_CANARY' not in json.dumps(records)
        assert 'CHANGED_SECRET_CANARY' not in json.dumps(records)
        case.complete()


def cancellation(binary, output, signum):
    with Case(binary, output, 'signal-' + str(signum), [{'stream_wait': True}]) as case:
        process = subprocess.Popen([str(binary), '--prompt', 'Wait for cancellation.', '--output-format', 'stream-json'],
                                   cwd=case.workspace, env=case.env, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        case.processes.append(process)
        case.wait_requests(1)
        os.kill(process.pid, signum)
        stdout, stderr = process.communicate(timeout=10)
        write(case.directory / 'signal.stdout.jsonl', stdout)
        write(case.directory / 'signal.stderr.txt', stderr)
        events = [json.loads(line) for line in stdout.splitlines()]
        assert process.returncode == 1, (process.returncode, events)
        assert events[-1]['kind'] == 'session_end' and events[-1]['data'] == {'status':'cancelled','exit_reason':'cancelled'}, events
        assert next(event['data']['status'] for event in events if event['kind'] == 'turn_end') == 'cancelled'
        assert case.audit()[0]['task']['status'] == 'cancelled'
        case.complete()


def mcp_http(binary, output, variant):
    with Case(binary, output, 'mcp-http-' + variant, [{'calls': [('mcp__fixture__echo', {'mode': ' <unchanged> '})]}]) as case:
        requests = [[], []]
        servers = []
        for index in range(2):
            class Handler(http.server.BaseHTTPRequestHandler):
                def log_message(self, *_): pass
                def do_GET(self): self.send_error(405)
                def do_DELETE(self):
                    self.send_response(200)
                    self.end_headers()
                def do_POST(self):
                    request = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
                    requests[self.server.fixture_index].append(request)
                    method = request.get('method')
                    result = {}
                    if method == 'initialize': result = {'protocolVersion':'2025-03-26','capabilities':{'tools':{}},'serverInfo':{'name':'http-fixture','version':'1'}}
                    elif method == 'tools/list': result = {'tools':[{'name':'echo','inputSchema':{'type':'object','properties':{'mode':{'type':'string'}}}}]}
                    elif method == 'tools/call': result = {'content':[{'type':'text','text':'HTTP_REPLAY_COMPLETE'}]}
                    response = {'jsonrpc':'2.0','id':request.get('id'),'result':result}
                    if method == 'server/discover': response = {'jsonrpc':'2.0','id':request['id'],'error':{'code':-32601,'message':'legacy'}}
                    payload = json.dumps(response).encode()
                    self.send_response(200 if 'id' in request else 202)
                    self.send_header('Content-Type','application/json')
                    self.send_header('Content-Length',str(len(payload)))
                    self.end_headers()
                    self.wfile.write(payload)
            server = http.server.ThreadingHTTPServer(('127.0.0.1',0),Handler)
            server.fixture_index = index
            threading.Thread(target=server.serve_forever,daemon=True).start()
            servers.append(server)
        try:
            config = {'url':'http://127.0.0.1:'+str(servers[0].server_port)+'/mcp','bearer_token_env':'FIXTURE_AUTH'}
            if variant == 'oauth-generation':
                config.pop('bearer_token_env')
                config['oauth'] = {}
                write(case.state / 'mcp-oauth-fixture.json.generation','original-generation').chmod(0o600)
                binding = hashlib.sha256(json.dumps({'URL':config['url'],'OAuth':{}},separators=(',',':')).encode()).hexdigest()
                dump(case.state / 'mcp-oauth-fixture.json', {'Generation':'original-generation','Binding':binding,
                      'Config':{'ClientID':'fixture'},'Token':{'access_token':'OAUTH_PRIVATE_CANARY','token_type':'Bearer','expiry':'2099-01-01T00:00:00Z'}})
                (case.state / 'mcp-oauth-fixture.json').chmod(0o600)
            case.env['FIXTURE_AUTH'] = 'HTTP_PRIVATE_CANARY'
            dump(case.state / 'mcp.json', {'mcpServers': {'fixture':config}})
            (case.state / 'mcp.json').chmod(0o600)
            first = approval(case.chat(flags=['--output-format','stream-json'],success=False))
            before = len(requests[0])
            if variant == 'url': config['url'] = 'http://127.0.0.1:'+str(servers[1].server_port)+'/mcp'
            if variant == 'auth': case.env['FIXTURE_AUTH'] = 'REPLACED_HTTP_PRIVATE_CANARY'
            if variant == 'auth-name':
                config['bearer_token_env'] = 'OTHER_AUTH'
                case.env['OTHER_AUTH'] = case.env['FIXTURE_AUTH']
            if variant == 'oauth-generation':
                write(case.state / 'mcp-oauth-fixture.json.generation','replacement-generation').chmod(0o600)
            dump(case.state / 'mcp.json', {'mcpServers': {'fixture':config}})
            success = variant == 'same'
            if success: case.steps.append({})
            result = resume(case,first,success=success,label='http-resume')
            if success:
                assert len([r for r in requests[0] if r.get('method') == 'tools/call']) == 1
            else:
                assert result.returncode == 1 and len(requests[0]) == before and not requests[1]
            records = case.audit()
            assert 'HTTP_PRIVATE_CANARY' not in json.dumps(records)
            dump(case.directory / 'http-requests.json',requests)
            case.complete()
        finally:
            for server in servers:
                server.shutdown()
                server.server_close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--only', action='append')
    args = parser.parse_args()
    binary, output = args.binary.resolve(), args.output.resolve()
    output.mkdir(parents=True, exist_ok=False)
    scenarios = {**{'undo-' + variant: lambda v=variant: undo(binary, output, v) for variant in ('none','file','rules','missing','corrupt')},
                 **{'mcp-' + variant: lambda v=variant: mcp(binary, output, v) for variant in ('tool','catalog','args','env','inherited','executable','fifo','relative','relative-changed')},
                 **{'mcp-http-' + variant: lambda v=variant: mcp_http(binary, output, v) for variant in ('same','url','auth','auth-name','oauth-generation')},
                 **{'signal-' + label: lambda s=signum: cancellation(binary, output, s) for label, signum in [('int', signal.SIGINT),('term', signal.SIGTERM)]}}
    report = {'passed': False, 'binary_sha256': hashlib.sha256(binary.read_bytes()).hexdigest(), 'scenarios': []}
    for name, scenario in scenarios.items():
        if args.only and name not in args.only: continue
        result = {'name':name, 'passed':False}
        try:
            scenario()
            result['passed'] = True
        except Exception:
            result['error'] = traceback.format_exc()
        report['scenarios'].append(result)
        dump(output / 'report.json', report)
        print(('PASS ' if result['passed'] else 'FAIL ') + name, flush=True)
        if not result['passed']: print(result['error'], file=sys.stderr, flush=True)
    report['passed'] = bool(report['scenarios']) and all(row['passed'] for row in report['scenarios'])
    dump(output / 'report.json', report)
    dump(output / 'checksums.json', {str(path.relative_to(output)): hashlib.sha256(path.read_bytes()).hexdigest() for path in sorted(output.rglob('*')) if path.is_file()})
    raise SystemExit(0 if report['passed'] else 1)


if __name__ == '__main__':
    main()
