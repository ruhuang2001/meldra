#!/usr/bin/env python3
"""Exercise the source comment scanner against disposable source trees."""
import argparse
from collections import Counter
import json
from pathlib import Path
import subprocess
import sys
import tempfile

if not __debug__:
    raise SystemExit('Verification requires assertions; unset PYTHONOPTIMIZE and do not use -O.')

SCANNER = Path(__file__).resolve().with_name('source-comments.py')
BASELINE = 'scripts/source-comments-baseline.json'
FIXTURES = {
    '.gitignore': 'ignored/\n# patterns\n',
    'ignored/file.py': '# excluded\n',
    'go.mod': 'module sample\n\ngo 1.26\n\nrequire example.org/a v1.0.0 // indirect\n',
    'sample.go': '//go:build linux\n\npackage sample\nconst URL = "https://host/#fragment" // Go note\n',
    'sample_test.go': 'package sample\nfunc Example() {\n // Output:\n // hello\n}\n',
    'cgo.go': 'package sample\n/*\n#include <stdlib.h>\n*/\nimport "C"\n',
    'test.py': '#!/usr/bin/env python3\n# coding: utf-8\ns = "# literal"  # Python note\nx = 1  # type: int\n',
    'run.sh': 'echo "# string" # shell note\ncat <<\'END\'\n# heredoc data\nEND\n',
    '.githooks/pre-commit': '#!/bin/sh\n# hook note\n',
    'Makefile': 'LABEL = value # make variable\ntest:\n\t@echo "# data" # make recipe\n',
    'config.toml': 'message = "# data" # TOML note\n',
    'site.yaml': 'message: "# data" # YAML note\nblock: |\n  # text\n',
    'index.html': '<!-- markup -->\n<script>const x = "// data"; // HTML script\n</script>\n<style>/* HTML style */</style>\n<script type="application/json">"// literal"</script>\n',
    'style.css': '/* first\nsecond */\na {content: "/* literal */";}\n',
    'app.mjs': 'const link = "https://site"; // JS note\n',
    '.github/workflows/probe.yml': """jobs:
  run:
    steps:
      - run: |
          echo "# data" # workflow shell
      - shell: python
        run: |
          print("# data") # workflow python
      - uses: actions/github-script@v8
        with:
          script: |
            console.log('// text'); // workflow JS
""",
}
ORDINARY = {
    '.gitignore': ['# patterns'], 'sample.go': ['// Go note'], 'test.py': ['# Python note'],
    'run.sh': ['# shell note'], '.githooks/pre-commit': ['# hook note'],
    'Makefile': ['# make variable', '# make recipe'], 'config.toml': ['# TOML note'],
    'site.yaml': ['# YAML note'], 'index.html': ['<!-- markup -->', '// HTML script', '/* HTML style */'],
    'style.css': ['/* first\nsecond */'], 'app.mjs': ['// JS note'],
    '.github/workflows/probe.yml': ['# workflow shell', '# workflow python', '// workflow JS'],
}
DIRECTIVES = {
    'go.mod': ['// indirect'], 'sample.go': ['//go:build linux'],
    'sample_test.go': ['// Output:', '// hello'], 'cgo.go': ['/*\n#include <stdlib.h>\n*/'],
    'test.py': ['#!/usr/bin/env python3', '# coding: utf-8', '# type: int'],
    '.githooks/pre-commit': ['#!/bin/sh'],
}


def write(root, path, content):
    target = root / path
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(content, encoding='utf-8')


def scan(root, *options):
    return subprocess.run([sys.executable, '-B', str(SCANNER), '--root', str(root), *options],
                          text=True, capture_output=True, timeout=120)


def inventory(root, git):
    for name, content in FIXTURES.items():
        write(root, name, content)
    if git:
        subprocess.run(['git', 'init', '-q', str(root)], check=True)
        subprocess.run(['git', '-C', str(root), 'add', 'sample.go'], check=True)
    result = scan(root, '--inventory')
    assert result.returncode == 0, result.stderr
    records = json.loads(result.stdout)['comments']
    for kind, expected in [('ordinary', ORDINARY), ('directive', DIRECTIVES)]:
        wanted = Counter((path, text) for path, texts in expected.items() for text in texts)
        found = Counter((r['path'], r['text']) for r in records if r['kind'] == kind)
        assert wanted == found, f'{kind}: missing {wanted - found}, unexpected {found - wanted}'
    for record in records:
        source = FIXTURES[record['path']]
        offset = source.index(record['text'])
        first = source.count('\n', 0, offset) + 1
        last = first + record['text'].count('\n')
        assert (record['start_line'], record['end_line']) == (first, last), record
    css = next(r for r in records if r['path'] == 'style.css')
    assert css['standalone_lines'] == 2, css
    inline = next(r for r in records if r['text'] == '# Python note')
    assert inline['standalone_lines'] == 0, inline
    return {'records': len(records), 'tracked_and_untracked': git}


def baseline_check(root, comments, allowances, expected_status, message, reference=False):
    def populate(entries):
        write(root, BASELINE, json.dumps([{'path': path, 'text': text} for path, text in entries]))

    if reference:
        subprocess.run(['git', 'init', '-q', str(root)], check=True)
        populate([ORIGINAL])
        subprocess.run(['git', '-C', str(root), 'add', BASELINE], check=True)
        subprocess.run(['git', '-C', str(root), '-c', 'user.name=Fixture', '-c',
                        'user.email=fixture@example.invalid', '-c', 'commit.gpgSign=false',
                        '-c', 'core.hooksPath=/dev/null', 'commit', '-qm', 'fixture'], check=True)
    populate(allowances)
    for path in sorted({p for p, _ in comments}):
        write(root, path, ''.join(text + '\n' for p, text in comments if p == path))
    result = scan(root, *(['--base-ref', 'HEAD'] if reference else []))
    assert result.returncode == expected_status, f'exit {result.returncode}: {result.stderr}'
    assert message in result.stderr, result.stderr
    return {'exit_code': result.returncode, 'diagnostic': result.stderr.strip()}


ORIGINAL = ('main.py', '# old')
EDIT = ('main.py', '# edited')
COPY = ('copy.py', '# old')
CASES = [
    ('unchanged', [ORIGINAL], [ORIGINAL], 0, '', False),
    ('shrunken-against-base', [], [], 0, '', True),
    ('edited', [EDIT], [ORIGINAL], 1, 'main.py:1', False),
    ('copied-to-new-file', [ORIGINAL, COPY], [ORIGINAL], 1, 'copy.py:1', False),
    ('same-file-duplicate', [ORIGINAL, ORIGINAL], [ORIGINAL], 1, 'main.py:2', False),
    ('added', [EDIT], [], 1, 'main.py:1', False),
    ('stale-allowance', [], [ORIGINAL], 1, 'stale', False),
    ('new-baseline-entry', [ORIGINAL, COPY], [ORIGINAL, COPY], 1, 'copy.py', True),
    ('duplicated-baseline-entry', [ORIGINAL, ORIGINAL], [ORIGINAL, ORIGINAL], 1, 'main.py', True),
]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    scenarios = []
    with tempfile.TemporaryDirectory(prefix='meldra-comments-e2e-') as directory:
        cases = [('git-inventory', inventory, (True,)), ('gitless-inventory', inventory, (False,))]
        cases.extend((name, baseline_check, arguments) for name, *arguments in CASES)
        for name, function, arguments in cases:
            root = Path(directory) / name
            root.mkdir()
            try:
                evidence = function(root, *arguments)
                scenarios.append({'name': name, 'passed': True, **evidence})
            except Exception as error:
                scenarios.append({'name': name, 'passed': False, 'error': str(error)})
    passed = all(row['passed'] for row in scenarios)
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps({'passed': passed, 'scenarios': scenarios}, indent=2) + '\n')
    for row in scenarios:
        if not row['passed']:
            print(f"{row['name']}: {row['error']}", file=sys.stderr)
    print(f'Source comment E2E report: {args.output}')
    return 0 if passed else 1


if __name__ == '__main__':
    sys.exit(main())
