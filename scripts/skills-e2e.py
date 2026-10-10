#!/usr/bin/env python3
"""Build and exercise Meldra against a deterministic loopback Responses SSE server."""
import argparse
import hashlib
import http.server
import json
import os
from pathlib import Path
import shutil
import stat
import subprocess
import sys
import tempfile
import threading
import traceback

ROOT = Path(__file__).resolve().parents[1]
BODY, REFERENCE, FRESH, SECRET = [f'E2E_PRIVATE_{s}_738af' for s in ('BODY', 'REFERENCE', 'FRESH', 'FORBIDDEN')]


def write(path, contents):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(contents if isinstance(contents, bytes) else contents.encode())
    return path


def dump(path, value):
    write(path, json.dumps(value, indent=2, ensure_ascii=False) + '\n')


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


class Harness:
    def __init__(self, output):
        self.output, self.binary = output, output / 'meldra'
        self.report = {'passed': False, 'checks': [], 'commands': []}
        self.runtime = Path(tempfile.mkdtemp(prefix='meldra-skills-e2e-')).resolve()
        self.home, self.state, self.workspace, self.tmp = [self.runtime / p for p in ('home', 'state', 'workspace', 'tmp')]
        for path in (self.home, self.state, self.workspace, self.tmp):
            path.mkdir(mode=0o700)
        self.env = {'PATH': os.environ.get('PATH', '/usr/bin:/bin'), 'HOME': str(self.home),
                    'MELDRA_HOME': str(self.state), 'TMPDIR': str(self.tmp),
                    'XDG_CACHE_HOME': str(self.home / '.cache'), 'NO_COLOR': '1', 'TERM': 'dumb', 'LANG': 'C.UTF-8'}
        self.requests, self.responses, self.server_errors, self.plan = [], [], [], []
        self.server = None

    def check(self, name, condition, detail=None):
        self.report['checks'].append({'name': name, 'passed': bool(condition), 'detail': detail})
        if not condition:
            raise AssertionError(f'{name}: {detail}')

    def command(self, label, args, stdin='', env=None):
        result = subprocess.run([str(self.binary), *args], cwd=self.workspace, env=env or self.env,
                                input=stdin, text=True, capture_output=True, timeout=90)
        write(self.output / (label + '.stdout.txt'), result.stdout)
        write(self.output / (label + '.stderr.txt'), result.stderr)
        self.report['commands'].append({'label': label, 'args': args, 'returncode': result.returncode})
        self.check(label + ' exits successfully', result.returncode == 0, result.stderr[-2000:])
        return result.stdout

    def skill(self, root, name, description='A skill fixture.', body='body'):
        return write(root / name / 'SKILL.md', f'---\nname: {name}\ndescription: {json.dumps(description)}\n---\n{body}\n')

    def catalog(self, label, workspace=None, env=None):
        value = json.loads(self.command(label, ['skills', '--workspace', str(workspace or self.workspace), '--json'], env=env))
        dump(self.output / (label + '.json'), value)
        self.check(label + ' JSON shape', isinstance(value.get('skills'), list) and isinstance(value.get('warnings'), list))
        return value

    def build(self):
        result = subprocess.run(['go', 'build', '-o', str(self.binary), '.'], cwd=ROOT, text=True, capture_output=True, timeout=180)
        write(self.output / 'build.stdout.txt', result.stdout)
        write(self.output / 'build.stderr.txt', result.stderr)
        self.check('real binary builds', result.returncode == 0, result.stderr[-3000:])
        paths = [*ROOT.glob('*.go'), *(ROOT / 'internal').rglob('*.go'), ROOT / 'go.mod', ROOT / 'go.sum',
                 Path(__file__), ROOT / 'docs/skills-failure-matrix.md']
        self.report['source_sha256'] = {str(p.relative_to(ROOT)): digest(p) for p in sorted(paths)}
        self.report['binary_sha256'] = digest(self.binary)
        self.report['git_head'] = subprocess.run(['git', 'rev-parse', 'HEAD'], cwd=ROOT, capture_output=True, text=True).stdout.strip()

    def discovery(self):
        roots = [self.workspace / '.meldra/skills', self.workspace / '.agents/skills', self.state / 'skills', self.home / '.agents/skills']
        self.demo = self.skill(roots[0], 'e2e-demo', 'E2E preferred skill metadata.', BODY)
        for number, root in enumerate(roots):
            self.skill(root, f'origin-{number}', f'Source {number}.', f'GLOBAL_ORIGIN_{number}_BODY')
            self.skill(root, 'same-name', f'Priority {number}.')
        write(roots[0] / 'quoted/SKILL.md', '---\r\nname: "quoted"\r\ndescription: "A colon: and # hash"\r\nlicense: MIT\r\nmetadata:\r\n  owner: example\r\n---\r\nQUOTED_PRIVATE_BODY\r\n')
        write(roots[0] / 'folded/SKILL.md', '---\nname: folded\ndescription: >-\n  First line\n  second line.\n---\nFOLDED_PRIVATE_BODY\n')
        bad = {'wrong-name': 'name: mismatch\ndescription: valid',
               'bad--name': 'name: bad--name\ndescription: valid',
               'Bad-case': 'name: Bad-case\ndescription: valid',
               'empty-description': 'name: empty-description\ndescription: ""',
               'missing-description': 'name: missing-description',
               'long-description': 'name: long-description\ndescription: ' + 'd' * 1025,
               'duplicate-key': 'name: duplicate-key\nname: duplicate-key\ndescription: duplicate',
               'broken-yaml': 'name: broken-yaml\ndescription: [unclosed',
               'not-string': 'name: not-string\ndescription: [a, b]',
               'large-header': 'name: large-header\ndescription: valid\nextra: ' + 'x' * 17000}
        for name, header in bad.items():
            write(roots[0] / name / 'SKILL.md', f'---\n{header}\n---\n{SECRET}\n')
        self.skill(roots[0], 'large-file', body='x' * (128 * 1024))
        self.skill(roots[0], 'a' * 65)
        write(roots[0] / 'missing-header/SKILL.md', 'No metadata.')
        outside = self.runtime / 'outside'
        self.skill(outside, 'linked-directory', body=SECRET)
        (roots[0] / 'linked-directory').symlink_to(outside / 'linked-directory', target_is_directory=True)
        (roots[0] / 'linked-file').mkdir()
        (roots[0] / 'linked-file/SKILL.md').symlink_to(outside / 'linked-directory/SKILL.md')
        hardlinked = self.skill(outside, 'hardlinked-metadata', body=SECRET)
        (roots[0] / 'hardlinked-metadata').mkdir()
        os.link(hardlinked, roots[0] / 'hardlinked-metadata/SKILL.md')
        self.skill(roots[0] / 'nested', 'not-immediate')
        (roots[0] / 'fifo-metadata').mkdir()
        os.mkfifo(roots[0] / 'fifo-metadata/SKILL.md')
        value = self.catalog('discovery')
        expected = sorted(['e2e-demo', 'same-name', 'quoted', 'folded', *[f'origin-{i}' for i in range(4)]])
        self.check('only valid immediate skills sorted', [s['name'] for s in value['skills']] == expected, value['skills'])
        by_name = {s['name']: s for s in value['skills']}
        self.check('higher-priority duplicate wins', by_name['same-name']['description'] == 'Priority 0.')
        self.check('quoted YAML and CRLF', by_name['quoted']['description'] == 'A colon: and # hash')
        self.check('folded YAML', by_name['folded']['description'] == 'First line second line.')
        self.check('catalog paths identify SKILL.md', all(Path(s['path']).is_absolute() and Path(s['path']).name == 'SKILL.md' for s in value['skills']))
        warnings = json.dumps(value['warnings']).lower()
        self.check('invalid, duplicate, and symlink diagnostics', all(w in warnings for w in ('duplicate', 'wrong-name', 'large-file', 'large-header')) and ('symlink' in warnings or 'symbolic link' in warnings), value['warnings'])
        self.check('listing needs no configuration', not (self.state / 'config.toml').exists() and not (self.state / 'credentials.env').exists())
        self.check('hardlinked metadata is rejected with diagnostic', 'hardlinked-metadata' not in by_name and 'hard link' in warnings, value['warnings'])
        self.command('discovery-human', ['skills', '--workspace', str(self.workspace)])
        for name, target in (('home-alias', self.home), ('state-alias', self.state)):
            (self.runtime / name).symlink_to(target, target_is_directory=True)
        alias_env = dict(self.env, HOME=str(self.runtime / 'home-alias'), MELDRA_HOME=str(self.runtime / 'state-alias'))
        aliased = self.catalog('trusted-home-aliases', env=alias_env)
        self.check('trusted home aliases preserve canonical catalog and package confinement', aliased == value, aliased)
        for label, count, description in (('candidate-limit', 280, 'small'), ('metadata-limit', 100, 'm' * 1000)):
            base = self.runtime / label
            workspace, home, state = [base / part for part in ('workspace', 'home', 'state')]
            for path in (workspace, home, state):
                path.mkdir(parents=True, mode=0o700)
            for i in range(count):
                self.skill(workspace / '.meldra/skills', f'fixture-{i:03d}', description)
            env = dict(self.env, HOME=str(home), MELDRA_HOME=str(state))
            catalog = self.catalog(label, workspace, env)
            self.check(label + ' is bounded', (0 if label == 'candidate-limit' else 1) <= len(catalog['skills']) <= (256 if label == 'candidate-limit' else 65) and bool(catalog['warnings']), len(catalog['skills']))

    def presentation(self):
        for flag in ('-h', '--help'):
            self.check('skills ' + flag + ' prints usage', 'Usage: meldra skills' in self.command('help-' + flag.lstrip('-'), ['skills', flag]))
        base = self.runtime / 'presentation'
        home, state, workspace = [base / part for part in ('home', 'state', 'workspace\nforged-row\x1b[31m')]
        for directory in (home, state, workspace):
            directory.mkdir(parents=True, mode=0o700)
        description = 'First line\nsecond line.\x1b]0;forged-title\a'
        path = self.skill(workspace / '.agents/skills', 'demo', description)
        (path.parent.parent / 'broken\nforged-warning').mkdir()
        env = dict(self.env, HOME=str(home), MELDRA_HOME=str(state), XDG_CACHE_HOME=str(home / '.cache'))
        catalog = self.catalog('presentation-json', workspace, env)
        self.check('JSON retains exact metadata and paths', len(catalog['skills']) == 1 and catalog['skills'][0]['description'] == description and catalog['skills'][0]['path'] == str(path), catalog)
        human = self.command('presentation-human', ['skills', '--workspace', str(workspace)], env=env)
        lines = human.splitlines()
        self.check('untrusted paths and warnings cannot add listing rows', len(lines) == 3 and lines[0].startswith('Warning: ') and lines[1] == 'demo\tFirst line second line.' and lines[2].startswith('  '), lines)
        self.check('human listing removes terminal controls', '\x1b' not in human and '\a' not in human, human)
        self.presentation_workspace, self.presentation_state, self.presentation_env = workspace, state, env

    def start_server(self):
        harness = self

        class Handler(http.server.BaseHTTPRequestHandler):
            def log_message(self, *_args):
                pass

            def do_POST(self):
                try:
                    request = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
                    harness.requests.append(request)
                    index = len(harness.requests)
                    if self.path not in ('/v1/responses', '/responses'):
                        raise AssertionError('unexpected HTTP path ' + self.path)
                    if not harness.plan:
                        raise AssertionError('unexpected provider request')
                    step = harness.plan.pop(0)
                    if step.get('declined_check'):
                        harness.check('declined script creates no file', not (harness.workspace / 'skill-output.txt').exists())
                    output = []
                    for j, (name, arguments) in enumerate(step.get('calls', [])):
                        output.append({'type': 'function_call', 'id': f'fc_{index}_{j}', 'call_id': f'call_{index}_{j}',
                                       'name': name, 'arguments': json.dumps(arguments), 'status': 'completed'})
                    if not output:
                        output = [{'type': 'message', 'id': f'msg_{index}', 'role': 'assistant', 'status': 'completed',
                                   'content': [{'type': 'output_text', 'text': 'Skills E2E complete.', 'annotations': []}]}]
                    event = {'type': 'response.completed', 'response': {'id': f'resp_{index}', 'object': 'response',
                             'status': 'completed', 'output': output}}
                    harness.responses.append(event)
                    payload = ('event: response.completed\ndata: ' + json.dumps(event) + '\n\n').encode()
                    self.send_response(200)
                    self.send_header('Content-Type', 'text/event-stream')
                    self.send_header('Content-Length', str(len(payload)))
                    self.end_headers()
                    self.wfile.write(payload)
                except Exception:
                    harness.server_errors.append(traceback.format_exc())
                    self.send_error(500)

        self.server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        config = write(self.state / 'config.toml', f'model = "e2e-model"\nbase_url = "http://127.0.0.1:{self.server.server_port}/v1"\nallow_insecure_base_url = true\n')
        config.chmod(0o600)
        self.env['OPENAI_API_KEY'] = 'dummy-e2e-not-a-real-credential'

    def audit(self, label, env=None):
        tasks = json.loads(self.command(label + '-tasks', ['tasks', '--json'], env=env))
        records = []
        for task in tasks:
            record = json.loads(self.command(label + '-task-' + task['id'], ['task', 'show', task['id'], '--json'], env=env))
            records.append(record)
            self.command(label + '-events-' + task['id'], ['task', 'events', task['id'], '--json'], env=env)
        dump(self.output / (label + '-task-records.json'), records)
        return records

    def interaction(self):
        demo_dir = self.demo.parent
        write(demo_dir / 'references/guide.md', REFERENCE)
        write(demo_dir / 'scripts/make.py', 'from pathlib import Path\nPath("skill-output.txt").write_text("approved skill script\\n")\nprint("skill script executed")\n')
        os.mkfifo(demo_dir / 'fifo-resource')
        write(demo_dir / 'binary.bin', b'\xff\x00binary')
        write(demo_dir / 'too-large.txt', b'x' * (128 * 1024 + 1))
        write(demo_dir / '.git/config', SECRET)
        write(self.state / 'private.txt', SECRET)
        credentials = write(self.state / 'credentials.env', '# ' + SECRET + '\n')
        credentials.chmod(0o600)
        os.link(credentials, demo_dir / 'credentials-hardlink.txt')
        (demo_dir / 'state-link').symlink_to(self.state / 'private.txt')
        (demo_dir / 'references/link.md').symlink_to(self.runtime / 'outside/linked-directory/SKILL.md')
        (demo_dir / 'linked-dir').symlink_to(self.runtime / 'outside', target_is_directory=True)
        read = lambda path, name='e2e-demo': ('read_skill', {'name': name, 'path': path})
        invalid = [read(None, 'unknown-skill')] + [read(p) for p in ('../SKILL.md', str(self.state / 'private.txt'), '.git/config', 'binary.bin', 'too-large.txt', 'state-link', 'references/link.md', 'linked-dir/linked-directory/SKILL.md', 'references', 'fifo-resource', 'credentials-hardlink.txt')]
        invalid.append(('read_file', {'path': str(self.state / 'private.txt'), 'offset': None, 'limit': None}))
        command = ('run_command', {'command': 'python3', 'args': ['.meldra/skills/e2e-demo/scripts/make.py'], 'timeout': 10})
        self.plan = [{'calls': [read(None)]}, {'calls': [read('references/guide.md'), read(None, 'origin-2'), read(None, 'origin-3')]},
                     {'calls': invalid}, {'calls': [command]}, {'calls': [command], 'declined_check': True}, {}]
        self.start_server()
        config = write(self.presentation_state / 'config.toml', f'model = "e2e-model"\nbase_url = "http://127.0.0.1:{self.server.server_port}/v1"\nallow_insecure_base_url = true\n')
        config.chmod(0o600)
        self.presentation_env['OPENAI_API_KEY'] = 'dummy-e2e-not-a-real-credential'
        self.plan = [{}]
        presentation = self.command('presentation-chat', ['--workspace', str(self.presentation_workspace), '--prompt', 'Inspect the skill catalog.'], env=self.presentation_env)
        warning = presentation.split('Warning: ', 1)[1].split('Chat with Meldra', 1)[0]
        self.check('chat skill warning remains on one line', warning.count('\n') == 1 and 'forged-warning' in warning and '\x1b' not in warning, warning)
        empty_workspace = self.runtime / 'empty-workspace'
        empty_workspace.mkdir(mode=0o700)
        empty_home = self.runtime / 'empty-home'
        empty_state = self.runtime / 'empty-state'
        empty_home.mkdir(mode=0o700)
        empty_state.mkdir(mode=0o700)
        empty_env = dict(self.env, HOME=str(empty_home), MELDRA_HOME=str(empty_state))
        empty_config = write(empty_state / 'config.toml', f'model = "e2e-model"\nbase_url = "http://127.0.0.1:{self.server.server_port}/v1"\nallow_insecure_base_url = true\n')
        empty_config.chmod(0o600)
        self.plan = [{}]
        empty_start = len(self.requests)
        self.command('empty-catalog-chat', ['--workspace', str(empty_workspace), '--prompt', 'Use $missing-skill if it matches.'], env=empty_env)
        empty_request = self.requests[empty_start]
        self.check('empty catalog keeps missing-skill guidance', 'explicitly requested skill is missing' in empty_request.get('instructions', '') and '$name' in empty_request.get('instructions', ''))
        self.check('empty catalog has no read tool', not any(t.get('name') == 'read_skill' for t in empty_request.get('tools', [])))
        self.plan = [{'calls': [read(None)]}, {'calls': [read('references/guide.md'), read(None, 'origin-2'), read(None, 'origin-3')]},
                     {'calls': invalid}, {'calls': [command]}, {'calls': [command], 'declined_check': True}, {}]
        chat_start = len(self.requests)
        self.command('chat', ['--workspace', str(self.workspace), '--prompt', 'Use $e2e-demo. Read its reference, then run the script only with approval.'], 'n\ny\n')
        self.check('script follows both approval decisions', not self.plan and not self.server_errors, self.server_errors)
        initial = self.requests[chat_start]
        instructions = initial.get('instructions', '')
        self.check('initial request exposes metadata', 'E2E preferred skill metadata.' in instructions and str(self.demo) in instructions)
        self.check('initial request excludes bodies and references', all(marker not in json.dumps(initial) for marker in (BODY, REFERENCE, 'QUOTED_PRIVATE_BODY', 'FOLDED_PRIVATE_BODY')))
        self.check('explicit skill activation guidance', 'read_skill' in instructions and '$' in instructions)
        schema = next((tool for tool in initial.get('tools', []) if tool.get('name') == 'read_skill'), {})
        parameters = schema.get('parameters', {})
        self.check('read_skill schema requires name and nullable path', set(parameters.get('required', [])) == {'name', 'path'} and 'null' in json.dumps(parameters.get('properties', {}).get('path', {})), schema)
        self.check('body loaded only after tool call', BODY in json.dumps(self.requests[chat_start + 1]))
        self.check('reference loaded after requested', REFERENCE in json.dumps(self.requests[chat_start + 2]))
        self.check('native and user global skill bodies are readable', all(f'GLOBAL_ORIGIN_{i}_BODY' in json.dumps(self.requests[chat_start + 2]) for i in (2, 3)))
        self.check('forbidden contents never reach provider', SECRET not in json.dumps(self.requests))
        self.check('approved script output exists', (self.workspace / 'skill-output.txt').read_text() == 'approved skill script\n')
        records = self.audit('first')
        calls = [call for record in records for call in record.get('tool_calls', [])]
        read_calls = [call for call in calls if call['name'] == 'read_skill']
        self.check('every skill read is audited as read', len(read_calls) == 16 and all(call['effect'] == 'read' for call in read_calls), [(c['name'], c['effect'], c['status']) for c in calls])
        self.check('valid reads succeed and adversarial reads fail', sum(c['status'] == 'succeeded' for c in read_calls) == 4 and sum(c['status'] == 'failed' for c in read_calls) == 12)
        hardlinked_calls = [c for c in read_calls if 'credentials-hardlink.txt' in json.dumps(c['arguments'])]
        self.check('hardlinked credentials read fails without audit disclosure', len(hardlinked_calls) == 1 and hardlinked_calls[0]['status'] == 'failed' and SECRET not in json.dumps(calls), hardlinked_calls)
        self.check('existing read_file stays confined', all(c['status'] == 'failed' for c in calls if c['name'] == 'read_file'))
        approvals = [a for record in records for a in record.get('approvals', [])]
        command_ids = {c['id'] for c in calls if c['name'] == 'run_command'}
        self.check('only execution requires approval', len(approvals) == 2 and {a['decision'] for a in approvals} == {'declined', 'approved'} and all(a['tool_call_id'] in command_ids for a in approvals), approvals)
        self.verify_artifacts(read_calls)
        session_id = records[0]['task']['session_id']
        self.skill(self.demo.parent.parent, 'e2e-demo', 'E2E refreshed skill metadata.', FRESH)
        before_resume = len(self.requests)
        self.plan = [{'calls': [read(None)]}, {}]
        self.command('resume', ['resume', session_id, '--prompt', 'Use $e2e-demo again and reread its current content.'])
        self.check('resume finishes planned flow', not self.plan and not self.server_errors, self.server_errors)
        self.check('resume rediscovers metadata', 'E2E refreshed skill metadata.' in self.requests[before_resume].get('instructions', ''))
        self.check('resume loads fresh body', FRESH in json.dumps(self.requests[-1]))
        records = self.audit('resumed')
        calls = [c for record in records for c in record.get('tool_calls', []) if c['name'] == 'read_skill']
        self.check('resume retains old and fresh audited reads', BODY in json.dumps(calls) and FRESH in json.dumps(calls))
        self.verify_artifacts(calls)

    def protected_boundaries(self):
        for label in ('nested-state', 'cache-source', 'cache-resource'):
            base = self.runtime / label
            home, workspace, state = [base / name for name in ('home', 'workspace', 'state')]
            if label == 'nested-state':
                demo_root = home / '.agents/skills'
                state = demo_root / 'demo/state'
            elif label == 'cache-source':
                cache = home / ('Library/Caches' if sys.platform == 'darwin' else '.cache')
                workspace = cache / 'meldra/workspace'
                demo_root = workspace / '.meldra/skills'
            else:
                demo_root = workspace / '.meldra/skills'
                home = demo_root / 'demo'
            for directory in (home, workspace, state):
                directory.mkdir(parents=True, exist_ok=True, mode=0o700)
            skill_path = self.skill(demo_root, 'demo', 'Protected-boundary demo metadata.', 'BOUNDARY_DEMO_BODY')
            cache = home / ('Library/Caches' if sys.platform == 'darwin' else '.cache')
            env = dict(self.env, HOME=str(home), MELDRA_HOME=str(state), XDG_CACHE_HOME=str(cache))
            config = write(state / 'config.toml', f'model = "e2e-model"\nbase_url = "http://127.0.0.1:{self.server.server_port}/v1"\nallow_insecure_base_url = true\n')
            config.chmod(0o600)
            credentials = write(state / 'credentials.env', '# ' + SECRET + '\n')
            credentials.chmod(0o600)
            if label == 'nested-state':
                self.skill(state / 'skills', 'native-demo', 'Native protected-root package.', 'NATIVE_NESTED_BODY')
                forbidden = 'state/credentials.env'
            elif label == 'cache-resource':
                write(cache / 'meldra/private.txt', SECRET)
                forbidden = str((cache / 'meldra/private.txt').relative_to(skill_path.parent))
            else:
                forbidden = None
            write(skill_path.parent / '.git/config', SECRET)
            value = self.catalog(label + '-catalog', workspace, env)
            names = {s['name'] for s in value['skills']}
            self.check(label + ' CLI catalog protection', ('demo' in names) == (label != 'cache-source'), names == {'demo', 'native-demo'} if label == 'nested-state' else sorted(names))
            read = lambda name, path: ('read_skill', {'name': name, 'path': path})
            requested = [read('demo', forbidden)]
            if label != 'cache-source':
                requested += [read('demo', '.git/config'), read('demo', '../SKILL.md'), read('demo', None)]
            if label == 'nested-state':
                requested += [read('native-demo', None)]
            start = len(self.requests)
            self.plan = [{'calls': requested}, {}]
            self.command(label + '-chat', ['--workspace', str(workspace), '--prompt', 'Inspect the available skill using read_skill.'], env=env)
            self.check(label + ' completes bounded tool flow', not self.plan and not self.server_errors, self.server_errors)
            instructions = self.requests[start].get('instructions', '')
            self.check(label + ' runtime matches CLI catalog', ('Protected-boundary demo metadata.' in instructions) == ('demo' in names))
            self.check(label + ' never discloses protected sentinel', SECRET not in json.dumps(self.requests[start:]))
            records = self.audit(label, env)
            calls = [c for record in records for c in record.get('tool_calls', []) if c['name'] == 'read_skill']
            failed = 1 if label == 'cache-source' else 3
            self.check(label + ' blocks protected resources and traversal', len(calls) == len(requested) and sum(c['status'] == 'failed' for c in calls) == failed, [(c['arguments'], c['status']) for c in calls])
            self.check(label + ' read-only audit contains no secret', all(c['effect'] == 'read' for c in calls) and SECRET not in json.dumps(calls))
            self.check(label + ' reads need no approval', not any(record.get('approvals') for record in records))
            if label == 'nested-state':
                self.check('nested native state skills remain readable', 'NATIVE_NESTED_BODY' in json.dumps(self.requests[-1]))
            self.verify_artifacts(calls, state)

    def verify_artifacts(self, calls, state=None):
        refs = [ref for call in calls if call['status'] == 'succeeded' for ref in call.get('result', {}).get('artifacts', [])]
        self.check('successful reads retain artifact references', len(refs) >= sum(c['status'] == 'succeeded' for c in calls))
        for ref in refs:
            candidates = list((state or self.state).rglob(ref['id']))
            self.check('artifact digest ' + ref['id'], len(candidates) == 1 and digest(candidates[0]) == ref['sha256'] and candidates[0].stat().st_size == ref['size'])

    def finish(self):
        if self.server:
            self.server.shutdown()
            self.server.server_close()
        dump(self.output / 'http-requests.json', self.requests)
        dump(self.output / 'http-responses.json', self.responses)
        dump(self.output / 'server-errors.json', self.server_errors)
        dump(self.output / 'assertions.json', self.report)
        copied_inodes = {}
        def copy_fixture(source, destination):
            if stat.S_ISFIFO(os.stat(source, follow_symlinks=False).st_mode):
                os.mkfifo(destination)
                return destination
            info = os.stat(source, follow_symlinks=False)
            identity = (info.st_dev, info.st_ino)
            if info.st_nlink > 1 and identity in copied_inodes:
                os.link(copied_inodes[identity], destination)
                return destination
            copied_inodes[identity] = destination
            return shutil.copy2(source, destination)
        shutil.copytree(self.runtime, self.output / 'sandbox', symlinks=True, copy_function=copy_fixture)
        shutil.rmtree(self.runtime)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', type=Path, default=ROOT / 'dist/skills-e2e', help='new or empty directory for repeatable evidence')
    args = parser.parse_args()
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=True)
    if any(output.iterdir()):
        parser.error('output directory must be empty; choose a new directory for each run')
    harness = Harness(output)
    try:
        harness.build()
        harness.discovery()
        harness.presentation()
        harness.interaction()
        harness.protected_boundaries()
        harness.report['passed'] = True
    except Exception:
        harness.report['error'] = traceback.format_exc()
        print(harness.report['error'], file=sys.stderr)
    finally:
        harness.finish()
    print(f"{'PASS' if harness.report['passed'] else 'FAIL'}: {len(harness.report['checks'])} checks; evidence: {output}")
    return 0 if harness.report['passed'] else 1


if __name__ == '__main__':
    sys.exit(main())
