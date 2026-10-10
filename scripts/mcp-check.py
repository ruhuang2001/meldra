#!/usr/bin/env python3
"""Run real-binary MCP suites and merge their coverage with go test coverage.

Failure cases, specified before implementation:
- a build, E2E suite, or covdata conversion fails: fail the check, retain evidence;
- empty/malformed profiles, mismatched modes/statement counts, negative counts:
  refuse the merge rather than manufacture coverage;
- existing evidence directories: choose a fresh directory, never overwrite them;
- suite reports claim failures or contain no checks: fail even after exit code 0;
- Go source changes during the run: reject coverage for mixed source revisions.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile


def merge_profiles(profiles):
    mode, blocks = None, {}
    for profile in profiles:
        lines = profile.splitlines()
        if len(lines) < 2 or lines[0] not in ('mode: set', 'mode: count', 'mode: atomic'):
            raise ValueError('empty or invalid coverage profile')
        if mode is not None and mode != lines[0]:
            raise ValueError('coverage modes differ')
        mode = lines[0]
        for line in lines[1:]:
            location, statements, count = line.rsplit(None, 2)
            statements, count = int(statements), int(count)
            if not re.fullmatch(r'.+:\d+\.\d+,\d+\.\d+', location) or statements < 0 or count < 0:
                raise ValueError('invalid coverage block: ' + line)
            previous = blocks.get(location)
            if previous and previous[0] != statements:
                raise ValueError('conflicting NumStmt for ' + location)
            total = count + (previous[1] if previous else 0)
            blocks[location] = (statements, min(total, 1) if mode == 'mode: set' else total)
    if not blocks:
        raise ValueError('no coverage blocks')
    return mode + '\n' + ''.join('%s %d %d\n' % (key, *blocks[key]) for key in sorted(blocks))


def self_check():
    block = 'meldra/example.go:1.1,2.2'
    assert merge_profiles(['mode: atomic\n' + block + ' 2 3\n', 'mode: atomic\n' + block + ' 2 4\n']) == 'mode: atomic\n' + block + ' 2 7\n'
    assert merge_profiles(['mode: set\n' + block + ' 2 1\n'] * 2).endswith(' 2 1\n')
    for profiles in ([], ['mode: atomic\n'], ['mode: atomic\n' + block + ' 2 -1\n'],
                     ['mode: atomic\n' + block + ' 2 1\n', 'mode: count\n' + block + ' 2 1\n'],
                     ['mode: atomic\n' + block + ' 2 1\n', 'mode: atomic\n' + block + ' 3 1\n']):
        try:
            merge_profiles(profiles)
        except ValueError:
            continue
        raise AssertionError('invalid coverage accepted')


def sha256(path):
    digest = hashlib.sha256()
    with path.open('rb') as stream:
        for chunk in iter(lambda: stream.read(1 << 20), b''):
            digest.update(chunk)
    return digest.hexdigest()


def sources():
    names = subprocess.check_output(['git', 'ls-files', '-z', '--cached', '--others', '--exclude-standard', '--', '*.go', 'go.mod', 'go.sum'])
    return {os.fsdecode(name): sha256(Path(os.fsdecode(name))) if Path(os.fsdecode(name)).is_file() else None
            for name in names.split(b'\0') if name}


def run(command, directory, label, env=None):
    print('+ ' + ' '.join(map(str, command)), flush=True)
    with (directory / (label + '.log')).open('w') as log:
        process = subprocess.Popen(command, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, env=env)
        try:
            for line in process.stdout:
                print(line, end='', flush=True)
                log.write(line)
            if process.wait() != 0:
                raise RuntimeError(label + ' failed; see ' + str(directory / (label + '.log')))
        finally:
            if process.poll() is None:
                process.terminate()
                try:
                    process.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--coverage', type=Path, default=Path('coverage.out'))
    parser.add_argument('--output-dir', '--dist', dest='output_dir', type=Path,
                        default=Path('.artifacts/checks'),
                        help='check evidence directory (separate from release artifacts)')
    parser.add_argument('--self-check', action='store_true')
    args = parser.parse_args()
    self_check()
    if args.self_check:
        print('coverage merge self-check passed')
        return
    os.chdir(Path(__file__).resolve().parent.parent)
    args.output_dir.mkdir(parents=True, exist_ok=True)
    directory = Path(tempfile.mkdtemp(prefix='mcp-check-', dir=args.output_dir.resolve()))
    covdir = directory / 'covdata'
    covdir.mkdir()
    summary = {'passed': False, 'directory': str(directory), 'sources': sources()}
    print('MCP check artifacts: ' + str(directory), flush=True)
    try:
        run(['go', 'test', '-race', '-coverpkg=./...', '-coverprofile=' + str(directory / 'tests.out'), './...'], directory, 'go-test')
        binary = directory / 'meldra'
        run(['go', 'build', '-race', '-cover', '-coverpkg=./...', '-covermode=atomic', '-o', str(binary), '.'], directory, 'go-build')
        env = dict(os.environ, GOCOVERDIR=str(covdir))
        for script in ('mcp-e2e', 'mcp-auth-interaction-e2e', 'mcp-cli-e2e', 'mcp-input-e2e', 'mcp-oauth-regression-e2e'):
            output = directory / script
            run([sys.executable, 'scripts/' + script + '.py', '--binary', str(binary), '--output', str(output)], directory, script, env)
            report = json.loads((output / 'report.json').read_text())
            checks = report['scenarios'] if isinstance(report, dict) else report
            if not checks or not all(check.get('passed') is True for check in checks):
                raise RuntimeError(script + ' did not report passing checks')
            summary[script] = len(checks)
        run(['go', 'tool', 'covdata', 'textfmt', '-i=' + str(covdir), '-o=' + str(directory / 'e2e.out')], directory, 'covdata')
        if sources() != summary['sources']:
            raise RuntimeError('Go sources changed while collecting coverage; run make check again')
        merged = merge_profiles([(directory / name).read_text() for name in ('tests.out', 'e2e.out')])
        (directory / 'coverage.out').write_text(merged)
        args.coverage.parent.mkdir(parents=True, exist_ok=True)
        with tempfile.NamedTemporaryFile(mode='w', dir=args.coverage.resolve().parent, delete=False) as temporary:
            temporary.write(merged)
        os.replace(temporary.name, args.coverage)
        summary['passed'] = True
    except BaseException as error:
        summary['error'] = str(error)
        raise
    finally:
        (directory / 'report.json').write_text(json.dumps(summary, indent=2) + '\n')
        checksums = {str(path.relative_to(directory)): sha256(path) for path in sorted(directory.rglob('*')) if path.is_file()}
        (directory / 'checksums.json').write_text(json.dumps(checksums, indent=2) + '\n')
        print('MCP check artifacts: ' + str(directory), flush=True)


if __name__ == '__main__':
    main()
