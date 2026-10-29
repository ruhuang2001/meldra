#!/usr/bin/env python3
"""Run real-binary MCP/capability suites and merge coverage with go test coverage.

Failure cases, specified before implementation:
- a build, E2E suite, or covdata conversion fails: fail the check, retain evidence;
- empty/malformed profiles, mismatched modes/statement counts, negative counts:
  refuse the merge rather than manufacture coverage;
- existing evidence directories: choose a fresh directory, never overwrite them;
- suite reports claim failures or contain no checks: fail even after exit code 0;
- Go source changes during the run: reject coverage for mixed source revisions.
- wrong/missing source roots: fail before testing; keep release verification
  scripts separate from immutable tagged application sources.
- concurrent suites must not share fixture state, logs, or coverage directories;
  merge coverage only after every suite has finished and validated its report.
- a parallel suite fails: retain all suites' evidence and fail the overall check.
- invalid worker counts: reject before starting subprocesses.
- fast mode must identify omitted gates and never publish a full coverage result
  or overwrite the last full coverage profile.
- web's race-instrumented fixture must be excluded from fast mode; Go compiler
  and module caches must be resolved before isolating suite HOME directories.
"""
import argparse
from concurrent.futures import ThreadPoolExecutor, as_completed
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import time


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
                print('[' + label + '] ' + line, end='', flush=True)
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
    parser.add_argument('--fast', action='store_true',
                        help='local feedback: ordinary Go tests and capability E2E only; no race or coverage gate')
    parser.add_argument('--e2e-workers', type=int, default=3,
                        help='maximum independent E2E suites in parallel (default: 3; use 1 for serial)')
    parser.add_argument('--source-root', type=Path, default=Path(__file__).resolve().parent.parent,
                        help='source checkout to verify (defaults to this script repository)')
    args = parser.parse_args()
    if args.e2e_workers < 1:
        parser.error('--e2e-workers must be positive')
    self_check()
    if args.self_check:
        print('coverage merge self-check passed')
        return
    os.chdir(args.source_root.resolve())
    if not Path('go.mod').is_file() or not Path('scripts/capability-e2e.py').is_file():
        parser.error('source root must be a Meldra source checkout')
    args.output_dir.mkdir(parents=True, exist_ok=True)
    directory = Path(tempfile.mkdtemp(prefix='mcp-check-', dir=args.output_dir.resolve()))
    covdir = directory / 'covdata'
    covdir.mkdir()
    started = time.monotonic()
    summary = {'passed': False, 'directory': str(directory), 'sources': sources(),
               'mode': 'fast' if args.fast else 'full', 'e2e_workers': args.e2e_workers,
               'omitted_checks': ['race', 'coverage', 'MCP E2E suites'] if args.fast else [],
               'suite_seconds': {}}
    print('MCP check artifacts: ' + str(directory), flush=True)
    try:
        # The release workflow also uses this driver against older immutable tags.
        if Path('scripts/swe-bench-lite.py').exists():
            adapter_output = directory / 'swe-bench-adapter-e2e'
            run([sys.executable, 'scripts/swe-bench-adapter-e2e.py', '--output', str(adapter_output)],
                directory, 'swe-bench-adapter-e2e')
            adapter_report = json.loads((adapter_output / 'report.json').read_text())
            if not adapter_report.get('passed'):
                raise RuntimeError('SWE-bench adapter E2E did not report passing checks')
            summary['swe-bench-adapter-e2e'] = len(adapter_report['scenarios'])
        else:
            summary['omitted_checks'].append('SWE-bench adapter absent from source revision')
        # Keep both full-suite gates without compounding race and coverage
        # overhead in deadline-sensitive child-process fixtures.
        if args.fast:
            run(['go', 'test', './...'], directory, 'go-test')
        else:
            run(['go', 'test', '-race', './...'], directory, 'go-test-race')
            run(['go', 'test', '-covermode=atomic', '-coverpkg=./...', '-coverprofile=' + str(directory / 'tests.out'), './...'], directory, 'go-test')
        binary = directory / 'meldra'
        build_flags = [] if args.fast else ['-race', '-cover', '-coverpkg=./...', '-covermode=atomic']
        build_env = dict(os.environ, CGO_ENABLED='0') if args.fast else None
        run(['go', 'build', *build_flags, '-o', str(binary), '.'], directory, 'go-build', build_env)
        scripts = ('capability-e2e', 'images-e2e', 'stream-review-e2e', 'commands-e2e') if args.fast else (
            'mcp-e2e', 'mcp-auth-interaction-e2e', 'mcp-cli-e2e', 'mcp-input-e2e',
            'mcp-oauth-regression-e2e', 'capability-e2e', 'quality-of-life-e2e', 'provider-compat-e2e', 'images-e2e', 'stream-review-e2e', 'commands-e2e')
        if not args.fast and Path('scripts/web-e2e.py').exists():
            scripts += ('web-e2e',)
        elif args.fast and Path('scripts/web-e2e.py').exists():
            summary['omitted_checks'].append('web E2E race fixture')
        # Build caches are concurrency-safe and contain no runtime configuration.
        # Resolve them before isolated HOME changes their implicit locations.
        go_caches = json.loads(subprocess.check_output(['go', 'env', '-json', 'GOCACHE', 'GOMODCACHE'], text=True))
        for key in ('GOCACHE', 'GOMODCACHE'):
            if not go_caches.get(key) or not Path(go_caches[key]).is_absolute():
                raise RuntimeError(key + ' must resolve to an absolute cache path')
        summary['go_build_caches'] = go_caches

        def suite(script):
            suite_started = time.monotonic()
            output = directory / script
            suite_covdir = covdir / script
            suite_covdir.mkdir()
            fixture = directory / (script + '-environment')
            paths = {key: fixture / name for key, name in (
                ('HOME', 'home'), ('XDG_CACHE_HOME', 'cache'),
                ('XDG_CONFIG_HOME', 'config'), ('XDG_STATE_HOME', 'state'), ('TMPDIR', 'tmp'))}
            for path in paths.values():
                path.mkdir(mode=0o700, parents=True)
            env = dict(os.environ, **go_caches, **{key: str(path) for key, path in paths.items()},
                       GOCOVERDIR=str(suite_covdir), PYTHONDONTWRITEBYTECODE='1',
                       MELDRA_DISABLE_UPDATE_CHECK='1')
            arguments = ['--controls', '--processes', '--stream-json'] if script == 'capability-e2e' else []
            run([sys.executable, 'scripts/' + script + '.py', '--binary', str(binary), '--output', str(output), *arguments], directory, script, env)
            report = json.loads((output / 'report.json').read_text())
            checks = report['scenarios'] if isinstance(report, dict) else report
            if not checks or not all(check.get('passed') is True for check in checks):
                raise RuntimeError(script + ' did not report passing checks')
            return len(checks), round(time.monotonic() - suite_started, 3)

        failures = []
        e2e_started = time.monotonic()
        # Only separate suite processes overlap. Each fixture owns its Agent,
        # workspace, HOME, log, ports, and coverage directory.
        with ThreadPoolExecutor(max_workers=args.e2e_workers) as executor:
            futures = {executor.submit(suite, script): script for script in scripts}
            for future in as_completed(futures):
                script = futures[future]
                try:
                    summary[script], summary['suite_seconds'][script] = future.result()
                except Exception as error:
                    failures.append(script + ': ' + str(error))
        summary['suite_failures'] = failures
        summary['e2e_elapsed_seconds'] = round(time.monotonic() - e2e_started, 3)
        if failures:
            raise RuntimeError('; '.join(failures))
        if sources() != summary['sources']:
            raise RuntimeError('Go sources changed while collecting coverage; run make check again')
        if not args.fast:
            inputs = ','.join(str(covdir / script) for script in scripts)
            run(['go', 'tool', 'covdata', 'textfmt', '-i=' + inputs, '-o=' + str(directory / 'e2e.out')], directory, 'covdata')
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
        summary['elapsed_seconds'] = round(time.monotonic() - started, 3)
        (directory / 'report.json').write_text(json.dumps(summary, indent=2) + '\n')
        checksums = {str(path.relative_to(directory)): sha256(path) for path in sorted(directory.rglob('*')) if path.is_file()}
        (directory / 'checksums.json').write_text(json.dumps(checksums, indent=2) + '\n')
        print('MCP check artifacts: ' + str(directory), flush=True)


if __name__ == '__main__':
    main()
