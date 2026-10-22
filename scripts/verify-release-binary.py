#!/usr/bin/env python3
"""Check actual release build metadata before GoReleaser uploads a binary.

Failure cases: missing metadata, development Go, unfixed toolchains, wrong
platform, and CGO builds must stop publication. Retain JSON on failure too.
"""
import argparse
import hashlib
import json
from pathlib import Path
import re
import subprocess


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', type=Path, required=True)
    parser.add_argument('--goos', choices=('darwin', 'linux'), required=True)
    parser.add_argument('--goarch', choices=('amd64', 'arm64'), required=True)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    report = {'binary': str(args.binary), 'target': args.goos + '/' + args.goarch,
              'advisory': 'GO-2026-6809', 'passed': False}
    try:
        report['sha256'] = hashlib.sha256(args.binary.read_bytes()).hexdigest()
        result = subprocess.run(['go', 'version', '-m', str(args.binary)],
                                capture_output=True, text=True, check=True)
        report['build_info'] = result.stdout
        lines = result.stdout.splitlines()
        match = re.fullmatch(r'go1[.]([0-9]+)[.]([0-9]+)', lines[0].rsplit(': ', 1)[-1]) if lines else None
        if not match:
            raise ValueError('release requires stable Go build metadata')
        minor, patch = map(int, match.groups())
        if not ((minor == 26 and patch >= 10) or (minor == 27 and patch >= 3) or minor > 27):
            raise ValueError('binary lacks the GO-2026-6809 toolchain fix')
        settings = {}
        for line in lines[1:]:
            fields = line.split(None, 1)
            if len(fields) == 2 and fields[0] == 'build' and '=' in fields[1]:
                key, value = fields[1].split('=', 1)
                settings[key] = value
        for key, expected in {'GOOS': args.goos, 'GOARCH': args.goarch, 'CGO_ENABLED': '0'}.items():
            if settings.get(key) != expected:
                raise ValueError(f'{key}: expected {expected}, got {settings.get(key)}')
        report['passed'] = True
    except (OSError, ValueError, subprocess.SubprocessError) as error:
        report['error'] = str(error)
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(report, indent=2) + chr(10))
    if not report['passed']:
        parser.exit(1, report['error'] + chr(10))
    print(f'Verified {args.binary}: {args.goos}/{args.goarch}, CGO disabled')


if __name__ == '__main__':
    main()
