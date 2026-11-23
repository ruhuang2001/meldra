import argparse
from bisect import bisect_right
from collections import Counter, defaultdict
from difflib import SequenceMatcher
from functools import cache
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile

import yaml
from tree_sitter import Query, QueryCursor
from tree_sitter_language_pack import get_parser


EXTENSIONS = {
    '.go': 'go', '.py': 'python', '.sh': 'bash', '.bash': 'bash',
    '.yaml': 'yaml', '.yml': 'yaml', '.toml': 'toml', '.mk': 'make',
    '.js': 'javascript', '.mjs': 'javascript', '.cjs': 'javascript',
    '.html': 'html', '.htm': 'html', '.css': 'css',
}
NAMES = {'Makefile': 'make', 'GNUmakefile': 'make', 'makefile': 'make',
         'go.mod': 'gomod', '.gitignore': 'gitignore'}


def interpreter(path):
    with path.open('rb') as stream:
        line = stream.readline(256)
    if not line.startswith(b'#!'):
        return None
    for word in line[2:].split():
        name = word.rsplit(b'/', 1)[-1]
        if name in (b'bash', b'sh', b'dash', b'ksh'):
            return 'bash'
        if re.fullmatch(rb'python(?:[23](?:\.\d+)?)?', name):
            return 'python'
        if name == b'node':
            return 'javascript'
    return None


def source_paths(root):
    with tempfile.TemporaryDirectory() as temp:
        git = ['git', '-C', str(root)]
        if not (root / '.git').exists():
            subprocess.run(['git', 'init', '--quiet', '--bare', temp], check=True)
            git.extend(['--git-dir=' + temp, '--work-tree=' + str(root)])
        paths = subprocess.check_output([*git, 'ls-files', '-z', '--cached', '--others',
                                         '--exclude-standard', '--exclude=node_modules/',
                                         '--exclude=.venv/', '--exclude=__pycache__/'])
    for raw in sorted(set(paths.split(b'\0')) - {b''}):
        path = Path(os.fsdecode(raw))
        full = root / path
        if full.resolve() != full or not full.is_file():
            continue
        language = NAMES.get(path.name) or EXTENSIONS.get(path.suffix)
        if not language and not path.suffix:
            language = interpreter(full)
            if not language and path.parent == Path('.githooks'):
                language = 'bash'
        if language:
            yield path, language


def go_comments(root, paths):
    by_path = defaultdict(list)
    if paths:
        helper = Path(__file__).resolve().with_name('source-comments-go.go')
        proc = subprocess.run(['go', 'run', str(helper)], cwd=helper.parent,
                              input=json.dumps([str(root / path) for path in paths]),
                              capture_output=True, text=True)
        if proc.returncode:
            raise ValueError(proc.stderr.strip())
        for line in proc.stdout.splitlines():
            row = json.loads(line)
            by_path[Path(row['path']).relative_to(root)].append((row['offset'], row['semantic']))
    return by_path


@cache
def grammar(language):
    parser = get_parser(language)
    captures = ['comment']
    captures.extend({
        'javascript': ['hash_bang_line', 'html_comment'], 'make': ['recipe', 'text'],
        'html': ['raw_text'],
    }.get(language, []))
    query = Query(parser.language, ' '.join(f'({name}) @{name}' for name in captures))
    return parser, query


def directive(language, text, start, data):
    if language in ('bash', 'python', 'javascript') and start == 0 and text.startswith(b'#!'):
        return True
    if language == 'python':
        if re.match(rb'#\s*type:\s*\S', text):
            return True
        first = data.split(b'\n', 1)[0].lstrip()
        return (data.count(b'\n', 0, start) < 2 and (first.startswith(b'#') or not first)
                and re.match(rb'^[ \t\f]*#.*?coding[:=][ \t]*[-\w.]+', text))
    if language == 'bash':
        return re.match(rb'#\s*shellcheck\s+(?:disable|enable|shell|source|source-path|external-sources)=\S+', text)
    if language == 'go':
        return re.match(rb'//(?:go:[a-z][a-z0-9_]*\b| \+build\s|line \S+:[0-9]+'
                        rb'|export \w+$| Code generated .* DO NOT EDIT\.$)', text) or (
            text.startswith(b'/*line ') and re.search(rb':[0-9]+\*/$', text))
    return False


def html_language(node):
    tag = node.parent.named_children[0]
    kind = b''
    for attr in tag.named_children:
        if attr.type == 'attribute' and attr.named_children[0].text.lower() == b'type':
            kind = attr.named_children[-1].text.strip(b'\'"').lower()
    if node.parent.type == 'style_element':
        return 'css' if kind in (b'', b'text/css') else None
    if kind in (b'', b'module', b'text/javascript', b'application/javascript'):
        return 'javascript'
    return None


def syntax_comments(language, data):
    parser, query = grammar(language)
    tree = parser.parse(data)
    captures = QueryCursor(query).captures(tree.root_node)
    for name, nodes in captures.items():
        for node in nodes:
            start, end = node.start_byte, node.end_byte
            if name == 'text':
                if node.parent.type == 'variable_assignment':
                    value = bytearray(node.text)
                    for child in node.named_children:
                        left, right = child.start_byte - start, child.end_byte - start
                        value[left:right] = b' ' * (right - left)
                    match = re.search(rb'(?<!\\)(?:\\\\)*(#.*)', value, re.DOTALL)
                    if match:
                        yield start + match.start(1), end, False
                continue
            if name == 'recipe':
                value = re.sub(rb'^(?:;[ \t]*|\t)[@+\-]*', b'', node.text, flags=re.MULTILINE)
                yield from embedded_comments('bash', value, node.text, start)
                continue
            if name == 'raw_text':
                embedded = html_language(node)
                if embedded:
                    for a, b, semantic in syntax_comments(embedded, node.text):
                        yield start + a, start + b, semantic
                continue
            semantic = (language == 'gomod' and node.parent.type == 'require_spec'
                        and node.text.strip() == b'// indirect')
            yield start, end, semantic or directive(language, node.text, start, data)


def mapping(node):
    if isinstance(node, yaml.MappingNode):
        return {key.value: value for key, value in node.value if isinstance(key, yaml.ScalarNode)}
    return {}


def workflow_scalars(source):
    workflow = mapping(yaml.compose(source, Loader=yaml.BaseLoader))
    default_shell = mapping(mapping(workflow.get('defaults')).get('run')).get('shell')
    for job in mapping(workflow.get('jobs')).values():
        job = mapping(job)
        shell = mapping(mapping(job.get('defaults')).get('run')).get('shell', default_shell)
        steps = job.get('steps')
        if not isinstance(steps, yaml.SequenceNode):
            continue
        for step in steps.value:
            fields = mapping(step)
            run = fields.get('run')
            if isinstance(run, yaml.ScalarNode):
                interpreter = fields.get('shell', shell)
                name = interpreter.value.split()[0] if interpreter and interpreter.value else 'bash'
                if name in ('sh', 'bash'):
                    language = 'bash'
                elif re.fullmatch(r'python(?:[23](?:\.\d+)?)?', name):
                    language = 'python'
                elif name == 'node':
                    language = 'javascript'
                else:
                    raise ValueError(f'unsupported workflow shell {name!r} at line {run.start_mark.line + 1}')
                yield language, run
            action = fields.get('uses')
            script = mapping(fields.get('with')).get('script')
            if (isinstance(script, yaml.ScalarNode) and action
                    and action.value.lower().startswith('actions/github-script@')):
                yield 'javascript', script


def embedded_comments(language, value, raw, offset):
    tokens = list(syntax_comments(language, value))
    if not tokens:
        return
    opcodes = SequenceMatcher(None, value, raw, autojunk=False).get_opcodes()
    for start, end, semantic in tokens:
        spans = []
        for operation, a, b, raw_start, raw_end in opcodes:
            if a >= end or b <= start or a == b:
                continue
            if operation == 'equal':
                spans.append((raw_start + max(start - a, 0), raw_end - max(b - end, 0)))
            else:
                spans.append((raw_start, raw_end))
        if not spans or spans[0][0] == spans[-1][1]:
            raise ValueError(f'cannot locate embedded {language} comment at byte {offset}')
        yield offset + spans[0][0], offset + spans[-1][1], semantic


def workflow_comments(data):
    source = data.decode('utf-8')
    for language, node in workflow_scalars(source):
        offset = len(source[:node.start_mark.index].encode())
        raw = source[node.start_mark.index:node.end_mark.index].encode()
        if node.style in ('|', '>'):
            header, sep, raw = raw.partition(b'\n')
            offset += len(header) + len(sep)
        elif node.style in ('"', "'"):
            raw = raw[1:-1]
            offset += 1
        yield from embedded_comments(language, node.value.encode(), raw, offset)


def file_comments(path, language, data, go_spans):
    if language == 'go':
        for start, semantic in go_spans:
            if data.startswith(b'/*', start):
                end = data.index(b'*/', start + 2) + 2
            else:
                newline = data.find(b'\n', start)
                end = len(data) if newline == -1 else newline
            yield start, end, semantic or directive('go', data[start:end].rstrip(b'\r'), start, data)
    elif language == 'gitignore':
        for match in re.finditer(rb'^#.*', data, re.MULTILINE):
            yield match.start(), match.end(), False
    else:
        yield from syntax_comments(language, data)
        if language == 'yaml' and path.parent == Path('.github/workflows'):
            yield from workflow_comments(data)


def inventory(root):
    files = list(source_paths(root))
    go_spans = go_comments(root, [path for path, language in files if language == 'go'])
    records = []
    for path, language in files:
        data = (root / path).read_bytes()
        spans = sorted(file_comments(path, language, data, go_spans[path]))
        if not spans:
            continue
        lines = [0, *(match.end() for match in re.finditer(b'\n', data)), len(data) + 1]
        masked = bytearray(data)
        for start, end, _ in spans:
            masked[start:end] = b' ' * (end - start)
        counted = set()
        for start, end, semantic in spans:
            first, last = bisect_right(lines, start), bisect_right(lines, end - 1)
            standalone = 0
            for line in range(first, last + 1):
                if line not in counted and not masked[lines[line - 1]:lines[line]].strip():
                    standalone += 1
                    counted.add(line)
            records.append(dict(path=str(path), start_line=first, end_line=last,
                                text=data[start:end].decode().rstrip('\r'), standalone_lines=standalone,
                                kind='directive' if semantic else 'ordinary'))
    ordinary = [c for c in records if c['kind'] == 'ordinary']
    return dict(files_scanned=len(files), ordinary_comments=len(ordinary),
                directive_comments=len(records) - len(ordinary),
                standalone_lines=sum(c['standalone_lines'] for c in ordinary), comments=records)


def allowances(contents, label):
    entries = json.loads(contents)
    if not isinstance(entries, list) or any(not isinstance(entry, dict)
            or entry.keys() != {'path', 'text'} or not all(isinstance(v, str) for v in entry.values())
            for entry in entries):
        raise ValueError(f'{label} must be an array of path/text pairs')
    return Counter((entry['path'], entry['text']) for entry in entries)


def base_allowances(root, baseline, ref):
    git = ['git', '-C', str(root)]
    revision = subprocess.check_output([*git, 'rev-parse', '--verify', '--end-of-options',
                                        ref + '^{commit}'], text=True).strip()
    path = baseline.relative_to(root).as_posix()
    if not subprocess.check_output([*git, 'ls-tree', '-z', revision, '--', path]):
        return Counter()
    blob = revision + ':' + path
    return allowances(subprocess.check_output([*git, 'show', blob]), blob)


def check_baseline(records, remaining):
    passed = True
    for row in records:
        if row['kind'] == 'ordinary':
            key = row['path'], row['text']
            if remaining[key] > 0:
                remaining[key] -= 1
            else:
                print(f"{row['path']}:{row['start_line']}: new comment {row['text']!r}", file=sys.stderr)
                passed = False
    for (path, text), count in remaining.items():
        if count:
            print(f'{path}: remove {count} stale baseline occurrence(s) of {text!r}', file=sys.stderr)
            passed = False
    return passed


def main():
    cli = argparse.ArgumentParser(description='Inventory source comments and enforce the checked-in baseline.')
    cli.add_argument('--root', type=Path, default=Path('.'))
    cli.add_argument('--baseline', type=Path, default=Path('scripts/source-comments-baseline.json'))
    cli.add_argument('--base-ref', help='reject baseline additions since this Git commit')
    cli.add_argument('--inventory', action='store_true', help='print a JSON inventory and skip the baseline check')
    args = cli.parse_args()
    root = args.root.resolve()
    result = inventory(root)
    if args.inventory:
        print(json.dumps(result, ensure_ascii=False, indent=2))
        return 0
    baseline = root / args.baseline
    current = allowances(baseline.read_bytes(), baseline)
    if args.base_ref:
        added = current - base_allowances(root, baseline, args.base_ref)
        if added:
            for (path, text), count in added.items():
                print(f'{path}: added {count} baseline allowance(s) for {text!r}', file=sys.stderr)
            return 1
    if not check_baseline(result['comments'], current):
        return 1
    print(f"Source comment lint passed ({result['ordinary_comments']} ordinary comments).")
    return 0


if __name__ == '__main__':
    try:
        sys.exit(main())
    except (OSError, ValueError, subprocess.CalledProcessError, yaml.YAMLError) as error:
        sys.exit(f'Source comment lint failed: {error}')
