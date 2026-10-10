#!/usr/bin/env python3
"""Run Meldra's checks locally against staged files or a committed revision."""

import argparse
import math
import os
from pathlib import Path
import subprocess
import signal
import sys
import tempfile


DEFAULT_CHECK_TIMEOUT_SECONDS = 900


def git(*args):
    return subprocess.check_output(['git', *args])


def check_staged():
    subprocess.run(['git', 'diff', '--cached', '--check'], check=True)
    paths = git('diff', '--cached', '--name-only', '--diff-filter=ACMR', '-z', '--', '*.go')
    failed = False
    for raw_path in paths.split(b'\0'):
        if not raw_path:
            continue
        path = raw_path.decode('utf-8', errors='surrogateescape')
        contents = git('show', ':' + path)
        formatted = subprocess.run(['gofmt'], input=contents, capture_output=True)
        if formatted.returncode != 0 or formatted.stdout != contents:
            print(f'Staged Go file needs gofmt: {path!r}', file=sys.stderr)
            sys.stderr.buffer.write(formatted.stderr)
            failed = True
    return 1 if failed else 0


def run_local_check(snapshot, environment):
    value = environment.get('MELDRA_LOCAL_CI_TIMEOUT', str(DEFAULT_CHECK_TIMEOUT_SECONDS))
    try:
        timeout = float(value)
        if not math.isfinite(timeout) or timeout <= 0:
            raise ValueError
    except ValueError:
        print('MELDRA_LOCAL_CI_TIMEOUT must be a positive number of seconds', file=sys.stderr)
        return 1

    process = subprocess.Popen(['make', 'check'], cwd=snapshot, env=environment,
                               start_new_session=(os.name != 'nt'))
    def stop_processes():
        if os.name == 'nt':
            process.kill()
        else:
            try:
                os.killpg(process.pid, signal.SIGTERM)
            except ProcessLookupError:
                pass
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                pass
            finally:
                # Children can outlive make or ignore SIGTERM. Always clean
                # the entire group, even if make has already exited.
                try:
                    os.killpg(process.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
        process.wait()
    try:
        process.wait(timeout=timeout)
    except subprocess.TimeoutExpired:
        print(f'Local CI timed out after {timeout:g} seconds; terminating make and its children.', file=sys.stderr)
        stop_processes()
        return 124
    except KeyboardInterrupt:
        stop_processes()
        return 130
    return process.returncode


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument('--quick', action='store_true', help='check staged whitespace and Go formatting')
    mode.add_argument('--staged', action='store_true', help='run make check against the index (default)')
    mode.add_argument('--ref', help='run make check against this committed revision')
    args = parser.parse_args()
    os.chdir(git('rev-parse', '--show-toplevel').decode().strip())

    if args.quick:
        return check_staged()

    with tempfile.TemporaryDirectory(prefix='meldra-local-ci-') as directory:
        temporary = Path(directory).resolve()
        snapshot = temporary / 'source'
        snapshot.mkdir()
        if args.ref:
            revision = git('rev-parse', '--verify', '--end-of-options', args.ref + '^{commit}').decode().strip()
            archive = temporary / 'source.tar'
            with archive.open('wb') as output:
                subprocess.run(['git', 'archive', '--format=tar', revision], stdout=output, check=True)
            subprocess.run(['tar', '-xf', str(archive), '-C', str(snapshot)], check=True)
            archive.unlink()
            label = revision[:12]
        else:
            subprocess.run(['git', 'checkout-index', '--all', '--prefix=' + str(snapshot) + '/'], check=True)
            label = 'staged files'
        environment = os.environ.copy()
        for name in git('rev-parse', '--local-env-vars').decode().splitlines():
            environment.pop(name, None)
        print(f'Local CI: {label}; running make check on this computer.', flush=True)
        return run_local_check(snapshot, environment)


if __name__ == '__main__':
    try:
        sys.exit(main())
    except (OSError, subprocess.CalledProcessError) as error:
        print(f'Local check failed: {error}', file=sys.stderr)
        sys.exit(1)
