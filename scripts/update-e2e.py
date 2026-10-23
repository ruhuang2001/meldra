#!/usr/bin/env python3
"""Exercise real CLI updates with gated loopback releases and saved sessions.

The build overlay changes only two release URLs. See the failure expectations in
scripts/update-e2e-failure-matrix.md. Evidence is retained in a fresh --output directory.
"""
import argparse
import errno
import hashlib
import http.server
import importlib.util
import io
import json
import os
from pathlib import Path
import pty
import select
import shlex
import shutil
import signal
import subprocess
import sys
import tarfile
import threading
import time
import traceback

ROOT = Path(__file__).resolve().parents[1]
OLD, NEW = 'v0.4.0', 'v0.4.1'


def write(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(value if isinstance(value, bytes) else value.encode())
    return path


def dump(path, value):
    write(path, json.dumps(value, indent=2) + '\n')


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


class ReleaseFixture:
    def __init__(self):
        self.requests, self.errors, self.current = [], [], None
        self.archive, self.archive_name = b'', ''
        fixture = self

        class Handler(http.server.BaseHTTPRequestHandler):
            def log_message(self, *_args):
                pass

            def do_GET(self):
                scenario = fixture.current
                try:
                    assert scenario is not None, 'release request outside a scenario'
                    base = '/download/' + scenario['latest'] + '/'
                    if self.path == '/latest':
                        stage = 'metadata'
                        payload = json.dumps({'tag_name': scenario['latest'], 'draft': False, 'prerelease': False}).encode()
                    elif self.path == base + 'meldra_checksums.txt':
                        stage = 'checksums'
                        checksum = '0' * 64 if scenario['bad_checksum'] else hashlib.sha256(fixture.archive).hexdigest()
                        payload = (checksum + '  ' + fixture.archive_name + '\n').encode()
                    elif self.path == base + fixture.archive_name:
                        stage, payload = 'archive', fixture.archive
                    else:
                        raise AssertionError('unexpected release request: ' + self.path)
                    record = {'case': scenario['name'], 'path': self.path, 'stage': stage,
                              'received_at': time.monotonic(), 'completed_at': None}
                    fixture.requests.append(record)
                    scenario['entered'][stage].set()
                    if stage in scenario['gated']:
                        assert scenario['gates'][stage].wait(25), stage + ' gate timed out'
                    status = 503 if stage == 'metadata' and scenario['metadata_error'] else 200
                    record['status'] = status
                    self.send_response(status)
                    self.send_header('Content-Length', str(len(payload)))
                    self.end_headers()
                    self.wfile.write(payload)
                    self.wfile.flush()
                    record['completed_at'] = time.monotonic()
                except (BrokenPipeError, ConnectionResetError):
                    record['client_disconnected'] = True
                except Exception:
                    fixture.errors.append(traceback.format_exc())
                    self.send_error(500)

        self.server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.url = 'http://127.0.0.1:' + str(self.server.server_port)

    def scenario(self, name, gated, latest, bad_checksum, metadata_error):
        stages = ('metadata', 'checksums', 'archive')
        self.current = {'name': name, 'latest': latest, 'gated': gated,
                        'bad_checksum': bad_checksum, 'metadata_error': metadata_error,
                        'entered': {s: threading.Event() for s in stages},
                        'gates': {s: threading.Event() for s in stages}}
        return self.current

    def close(self):
        if self.current:
            for gate in self.current['gates'].values():
                gate.set()
        self.server.shutdown()
        self.server.server_close()


class UpdateProcess:
    def __init__(self, case, terminal):
        self.case, self.terminal, self.output = case, terminal, bytearray()
        if terminal:
            self.master, slave = pty.openpty()
            self.process = subprocess.Popen([str(case.binary), 'update'], cwd=case.workspace, env=case.env,
                                            stdin=slave, stdout=slave, stderr=slave, start_new_session=True)
            os.close(slave)
            self.fd = self.master
        else:
            self.process = subprocess.Popen([str(case.binary), 'update'], cwd=case.workspace, env=case.env,
                                            stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                                            stderr=subprocess.STDOUT, start_new_session=True)
            self.fd = self.process.stdout.fileno()
        self.closed = False

    def read(self, timeout=0.05):
        if self.closed or not select.select([self.fd], [], [], timeout)[0]:
            return
        try:
            chunk = os.read(self.fd, 65536)
        except OSError as error:
            if error.errno != errno.EIO:
                raise
            chunk = b''
        if not chunk:
            self.closed = True
        self.output.extend(chunk)

    def wait_text(self, text, timeout=5):
        deadline = time.monotonic() + timeout
        while text.encode() not in self.output:
            self.read()
            assert time.monotonic() < deadline, ('notice not visible before response', text, self.output)
            assert not self.closed, ('process ended before notice', text, self.output)

    def finish(self, timeout=10):
        deadline = time.monotonic() + timeout
        while self.process.poll() is None or not self.closed:
            self.read()
            assert time.monotonic() < deadline, ('update did not finish', self.output)
        self.process.wait(timeout=1)
        return self.output.decode(errors='replace').replace('\r\n', '\n')

    def close(self):
        if self.process.poll() is None:
            self.process.kill()
            self.process.wait(timeout=5)
        self.read(0)
        write(self.case.directory / 'update.transcript.txt', bytes(self.output))
        dump(self.case.directory / 'update-process.json', {'argv': self.process.args,
             'terminal': self.terminal, 'returncode': self.process.returncode})
        if self.terminal:
            os.close(self.master)
        else:
            self.process.stdout.close()


class Harness:
    def __init__(self, output):
        self.output, self.fixture = output, ReleaseFixture()
        spec = importlib.util.spec_from_file_location('capability_e2e', ROOT / 'scripts/capability-e2e.py')
        module = importlib.util.module_from_spec(spec)
        previous_bytecode_setting = sys.dont_write_bytecode
        sys.dont_write_bytecode = True
        try:
            spec.loader.exec_module(module)
        finally:
            sys.dont_write_bytecode = previous_bytecode_setting
        self.Case = module.Case
        self.report = {'passed': False, 'checks': [], 'builds': [], 'cases': []}

    def check(self, label, condition, detail=None):
        self.report['checks'].append({'name': label, 'passed': bool(condition), 'detail': detail})
        assert condition, (label, detail)

    def build(self):
        source = ROOT / 'internal/app/update.go'
        original = source.read_text()
        overlay = original
        substitutions = {
            'https://api.github.com/repos/ruhuang2001/meldra/releases/latest': self.fixture.url + '/latest',
            'https://github.com/ruhuang2001/meldra/releases/download': self.fixture.url + '/download',
        }
        for old, new in substitutions.items():
            self.check('release endpoint occurs exactly once: ' + old, overlay.count(old) == 1)
            overlay = overlay.replace(old, new)
        overlay_source = write(self.output / 'build-overlay/update.go', overlay)
        overlay_json = self.output / 'build-overlay/overlay.json'
        dump(overlay_json, {'Replace': {str(source): str(overlay_source)}})
        dump(self.output / 'fixture-metadata.json', {'release_base_url': self.fixture.url,
             'endpoint_substitutions': substitutions, 'source_sha256': digest(source),
             'overlay_sha256': digest(overlay_source), 'old_version': OLD, 'new_version': NEW})
        self.binaries = {}
        for version in (OLD, NEW):
            binary = self.output / ('meldra-' + version)
            args = ['go', 'build', '-trimpath', '-overlay', str(overlay_json), '-ldflags',
                    '-X main.version=' + version, '-o', str(binary), '.']
            result = subprocess.run(args, cwd=ROOT, env={**os.environ, 'CGO_ENABLED': '0'},
                                    text=True, capture_output=True, timeout=240)
            write(self.output / (version + '.build.stdout.txt'), result.stdout)
            write(self.output / (version + '.build.stderr.txt'), result.stderr)
            self.check(version + ' real CLI builds', result.returncode == 0, result.stderr[-2000:])
            self.binaries[version] = binary
            self.report['builds'].append({'version': version, 'argv': args, 'sha256': digest(binary)})
        goenv = subprocess.run(['go', 'env', 'GOOS', 'GOARCH', 'GOVERSION'], cwd=ROOT,
                               capture_output=True, text=True, check=True, timeout=30).stdout.splitlines()
        self.fixture.archive_name = 'meldra_' + {'darwin': 'Darwin', 'linux': 'Linux'}[goenv[0]] + '_' + goenv[1] + '.tar.gz'
        self.report['toolchain'] = goenv
        archive = io.BytesIO()
        with tarfile.open(fileobj=archive, mode='w:gz') as package:
            info = tarfile.TarInfo('meldra')
            info.size, info.mode, info.mtime = self.binaries[NEW].stat().st_size, 0o755, 0
            with self.binaries[NEW].open('rb') as binary:
                package.addfile(info, binary)
        self.fixture.archive = archive.getvalue()
        write(self.output / self.fixture.archive_name, self.fixture.archive)
        self.report['archive_sha256'] = hashlib.sha256(self.fixture.archive).hexdigest()
        sources = [*ROOT.glob('*.go'), *(ROOT / 'internal').rglob('*.go'), ROOT / 'go.mod', ROOT / 'go.sum', Path(__file__), ROOT / 'scripts/capability-e2e.py']
        self.report['source_sha256'] = {str(p.relative_to(ROOT)): digest(p) for p in sorted(sources)}

    def run_case(self, name, terminal=False, latest=NEW, bad_checksum=False,
                 metadata_error=False, cancel=False, session=False, current=OLD):
        name += '-pty' if terminal else '-pipe'
        directory = self.output / name
        binary = directory / 'bin/meldra'
        steps = [{'text': 'OLD_SESSION_REPLY'}, {'text': 'RESUMED_SESSION_REPLY'}] if session else []
        with self.Case(binary, self.output, name, steps) as case:
            case.env['MELDRA_DISABLE_UPDATE_CHECK'] = '1'
            binary.parent.mkdir()
            shutil.copy2(self.binaries[current], binary)
            self.check(name + ' starts at requested version', case.run(['version'], label='before-version').stdout.strip() == current)
            before_hash = digest(binary)
            saved = None
            if session:
                case.chat('OLD_SESSION_PROMPT', label='before-chat')
                snapshots = list((case.state / 'tasks/sessions').glob('*.json'))
                self.check(name + ' old binary creates one saved session', len(snapshots) == 1, [str(p) for p in snapshots])
                saved = json.loads(snapshots[0].read_text())
                dump(directory / 'before-session.json', saved)
                self.check(name + ' old session contains history', 'OLD_SESSION_PROMPT' in json.dumps(saved) and 'OLD_SESSION_REPLY' in json.dumps(saved))
            should_update = latest == NEW and current == OLD
            gated = ('metadata', 'checksums', 'archive') if should_update and not metadata_error else ('metadata',)
            scenario = self.fixture.scenario(name, gated, latest, bad_checksum, metadata_error)
            client = UpdateProcess(case, terminal)
            observations = []
            try:
                notices = [('metadata', 'Checking for updates (current: ' + current + ')...'),
                           ('checksums', 'Downloading checksums...'),
                           ('archive', 'Downloading ' + self.fixture.archive_name + '...')]
                for stage, notice in notices:
                    if stage not in gated:
                        continue
                    self.check(name + ' enters ' + stage, scenario['entered'][stage].wait(6))
                    client.wait_text(notice)
                    record = next(r for r in reversed(self.fixture.requests) if r['case'] == name and r['stage'] == stage)
                    observation = {'stage': stage, 'notice': notice, 'observed_at': time.monotonic(),
                                   'response_completed_at_observation': record['completed_at'],
                                   'process_running': client.process.poll() is None,
                                   'transcript': client.output.decode(errors='replace')}
                    observations.append(observation)
                    self.check(name + ' publishes progress before ' + stage + ' response',
                               record['completed_at'] is None and observation['process_running'], observation)
                    if cancel and stage == 'archive':
                        client.process.send_signal(signal.SIGINT)
                        transcript = client.finish()
                        self.check(name + ' cancellation exits before gate release', not scenario['gates'][stage].is_set())
                        break
                    scenario['gates'][stage].set()
                else:
                    transcript = client.finish()
                failed = bad_checksum or metadata_error or cancel
                self.check(name + ' exit status', (client.process.returncode != 0) == failed, client.process.returncode)
                self.check(name + ' plain newline output', '\x1b' not in transcript)
                installed = should_update and not failed
                expected_version = NEW if installed else current
                self.check(name + ' installed version', case.run(['version'], label='after-version').stdout.strip() == expected_version)
                after_hash = digest(binary)
                expected_hash = digest(self.binaries[NEW]) if installed else before_hash
                self.check(name + ' installed binary hash', after_hash == expected_hash)
                dump(directory / 'hashes.json', {'before': before_hash, 'after': after_hash, 'expected': expected_hash})
                if installed:
                    stages = ['Checking for updates (current: ' + OLD + ')...',
                              'Updating Meldra: ' + OLD + ' -> ' + NEW, 'Downloading checksums...',
                              'Downloading ' + self.fixture.archive_name + '...', 'Verifying download...',
                              'Installing update...', 'Updated Meldra: ' + OLD + ' -> ' + NEW + '.']
                    self.check(name + ' ordered success stages', all(s in transcript for s in stages) and
                               [transcript.index(s) for s in stages] == sorted(transcript.index(s) for s in stages), transcript)
                    self.check(name + ' removes obsolete session warning', 'existing sessions' not in transcript.lower() and 'new sessions' not in transcript.lower())
                elif not failed:
                    self.check(name + ' no-op versions', current in transcript and '(latest: ' + latest + ')' in transcript and 'no update installed.' in transcript, transcript)
                    self.check(name + ' no-op requests only metadata', [r['stage'] for r in self.fixture.requests if r['case'] == name] == ['metadata'])
                else:
                    self.check(name + ' failure never reports installation or success', 'Installing update...' not in transcript and 'Updated Meldra:' not in transcript, transcript)
                    if bad_checksum:
                        self.check(name + ' explains checksum failure', 'checksum mismatch' in transcript)
                    if metadata_error:
                        self.check(name + ' explains metadata failure', 'HTTP 503' in transcript)
                self.check(name + ' leaves no temporary replacement', not list(binary.parent.glob('.meldra.update-*')))
                if session:
                    case.run(['resume', saved['id'], '--prompt', 'RESUMED_SESSION_PROMPT'], label='resume-chat')
                    updated = json.loads((case.state / 'tasks/sessions' / (saved['id'] + '.json')).read_text())
                    dump(directory / 'after-session.json', updated)
                    self.check(name + ' resumes same session ID', updated['id'] == saved['id'])
                    history = json.dumps(case.requests[-1])
                    self.check(name + ' resumes with old conversation', all(s in history for s in ('OLD_SESSION_PROMPT', 'OLD_SESSION_REPLY', 'RESUMED_SESSION_PROMPT')), history)
                    self.check(name + ' persists resumed conversation', 'RESUMED_SESSION_REPLY' in json.dumps(updated))
                help_text = case.run(['update', '--help'], label='update-help').stdout
                self.check(name + ' help removes obsolete session warning', 'existing sessions' not in help_text.lower() and 'new sessions' not in help_text.lower())
                case.complete()
                self.report['cases'].append({'name': name, 'passed': True, 'terminal': terminal,
                                             'before_sha256': before_hash, 'after_sha256': after_hash})
            finally:
                for gate in scenario['gates'].values():
                    gate.set()
                client.close()
                dump(directory / 'gate-observations.json', observations)

    def run(self):
        self.build()
        for terminal in (False, True):
            self.run_case('success', terminal=terminal, session=not terminal)
        self.run_case('already-current', current=NEW)
        self.run_case('ahead-of-release', current=NEW, latest=OLD)
        self.run_case('metadata-failure', metadata_error=True)
        self.run_case('checksum-failure', bad_checksum=True)
        self.run_case('cancel-download', cancel=True)
        self.check('release fixture has no internal errors', not self.fixture.errors, self.fixture.errors)
        self.report['passed'] = True


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', type=Path, required=True, help='new directory for retained evidence')
    output = parser.parse_args().output.expanduser().resolve()
    output.mkdir(parents=True, exist_ok=False)
    replay = 'cd ' + shlex.quote(str(ROOT)) + '\npython3 scripts/update-e2e.py --output ' + shlex.quote(str(output) + '-replay') + '\n'
    write(output / 'replay.sh', '#!/bin/sh\nset -eu\n' + replay).chmod(0o755)
    harness = Harness(output)
    try:
        harness.run()
    except Exception:
        harness.report['error'] = traceback.format_exc()
        print(harness.report['error'], file=sys.stderr)
    finally:
        harness.fixture.close()
        dump(output / 'release-requests.json', harness.fixture.requests)
        dump(output / 'fixture-errors.json', harness.fixture.errors)
        dump(output / 'report.json', harness.report)
    print(json.dumps({'passed': harness.report['passed'], 'report': str(output / 'report.json')}))
    return 0 if harness.report['passed'] else 1


if __name__ == '__main__':
    sys.exit(main())
