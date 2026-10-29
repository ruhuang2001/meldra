#!/usr/bin/env python3
"""Verify search and session metadata through the real CLI, offline."""
import argparse
import importlib.util
import json
from pathlib import Path
import traceback

spec = importlib.util.spec_from_file_location('capability', Path(__file__).with_name('capability-e2e.py'))
cap = importlib.util.module_from_spec(spec)
spec.loader.exec_module(cap)


def search(binary, output):
    def inspect(case, request):
        results = [item['output'] for item in request['input'] if item.get('type') == 'function_call_output']
        assert len(results) == 6, results
        assert 'item.txt:2:a.b' in results[0] and 'item.txt:1:acb' not in results[0], results
        assert 'item.txt:1:acb' in results[1] and 'item.txt:2:a.b' in results[1], results
        assert 'error' in results[2].lower(), results
        assert 'item.txt:3:你好42' in results[3], results
        assert '[truncated' in results[4] and len(results[4]) < 70000, results
        assert 'item.txt' not in results[5], results
        return {}
    inputs = [{'query': 'a.b', 'path': 'item.txt'},
              {'query': '^a.b$', 'path': 'item.txt', 'regex': True},
              {'query': '[', 'regex': True},
              {'query': '你好[0-9]+', 'regex': True, 'path': 'item.txt'},
              {'query': '.*', 'regex': True, 'path': 'long.txt', 'max_results': 1},
              {'query': '^a.b$', 'path': 'item.txt', 'regex': False}]
    with cap.Case(binary, output, 'regex-search', [{'calls': [('search_files', args) for args in inputs]}, inspect]) as case:
        cap.write(case.workspace / 'item.txt', 'acb\na.b\n你好42\n')
        cap.write(case.workspace / 'long.txt', 'x' * 200000 + '\n')
        case.chat(flags=['--mode', 'plan'])
        case.audit()
        case.complete()


def titles(binary, output):
    with cap.Case(binary, output, 'session-title', [{}, {}]) as case:
        prompt = '标题' * 35
        case.chat(prompt)
        snapshot = next((case.state / 'tasks/sessions').glob('*.json'))
        saved = json.loads(snapshot.read_text())
        assert saved['title'] == prompt[:60], saved
        listing = case.run(['sessions'], label='first-list')
        assert prompt[:60] in listing.stdout, listing.stdout
        case.run(['resume', saved['id'], '--prompt', 'A later request'], label='resume')
        assert json.loads(snapshot.read_text())['title'] == prompt[:60]
        assert prompt[:60] in case.run(['sessions'], label='resumed-list').stdout
        saved = json.loads(snapshot.read_text())
        del saved['title']
        cap.dump(snapshot, saved)
        assert saved['id'] in case.run(['sessions'], label='legacy-list').stdout
        case.complete()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    args.output.mkdir(parents=True, exist_ok=False)
    report = {'passed': False, 'scenarios': []}
    for name, scenario in [('regex-search', search), ('session-title', titles)]:
        result = {'name': name, 'passed': False}
        try:
            scenario(args.binary.resolve(), args.output.resolve())
            result['passed'] = True
        except Exception:
            result['error'] = traceback.format_exc()
        report['scenarios'].append(result)
        print(json.dumps(result), flush=True)
    report['passed'] = all(item['passed'] for item in report['scenarios'])
    cap.dump(args.output / 'report.json', report)
    raise SystemExit(0 if report['passed'] else 1)


if __name__ == '__main__':
    main()
