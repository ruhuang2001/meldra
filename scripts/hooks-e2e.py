#!/usr/bin/env python3
"""Verify lifecycle command hooks across TUI, line mode, stream-json, and MCP serve, offline."""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import traceback

spec = importlib.util.spec_from_file_location('capability', Path(__file__).with_name('capability-e2e.py'))
cap = importlib.util.module_from_spec(spec)
spec.loader.exec_module(cap)


def write_hooks(state, config):
    path = state / 'hooks.json'
    path.write_text(json.dumps(config, indent=2) + '\n', encoding='utf-8')
    path.chmod(0o600)
    return path


def write_script(directory, name, body):
    path = directory / name
    path.write_text('#!/usr/bin/env python3\n' + body.lstrip('\n'), encoding='utf-8')
    path.chmod(0o700)
    return path


def read_jsonl(path):
    if not path.exists():
        return []
    return [json.loads(line) for line in path.read_text(encoding='utf-8').splitlines() if line.strip()]


def external_memory_and_surfaces(binary, output):
    with cap.Case(binary, output, 'external-memory-and-surfaces', [{}, {}, {}, {'hold': True}, {}, {}]) as case:
        scripts = case.directory / 'scripts'
        scripts.mkdir(mode=0o700)
        log = case.directory / 'memory-hook.jsonl'
        repo_marker = case.directory / 'repo-hook-ran'

        for rel in ('.meldra/hooks.json', '.codex/hooks.json', '.claude/settings.json'):
            cap.write(case.workspace / rel, json.dumps({
                'hooks': {
                    'SessionStart': [{'hooks': [{'type': 'command', 'command': f'touch "{repo_marker}"'}]}],
                    'UserPromptSubmit': [{'hooks': [{'type': 'command', 'command': f'touch "{repo_marker}"'}]}],
                }
            }))

        codex_memory = write_script(scripts, 'codex_memory_hook.py', f"""
import json, sys
payload = json.load(sys.stdin)
with open({str(log)!r}, 'a', encoding='utf-8') as f:
    f.write(json.dumps(payload) + '\\n')
event = payload.get('hook_event_name')
sys.stderr.write('diagnostic-stderr-must-not-corrupt-transport\\n')
if event == 'SessionStart':
    src = payload.get('source', '')
    ctx = 'MEM_STARTUP_CONTEXT' if src == 'startup' else 'MEM_RESUME_CONTEXT'
    print(json.dumps({{'hookSpecificOutput': {{'hookEventName': 'SessionStart', 'additionalContext': ctx}}}}))
elif event == 'UserPromptSubmit':
    prompt = payload.get('prompt', '')
    print(json.dumps({{'hookSpecificOutput': {{'hookEventName': 'UserPromptSubmit', 'additionalContext': 'TURN_MEM:' + prompt[:24]}}}}))
""")

        write_hooks(case.state, {
            'description': 'Codex-style external memory hook fixture',
            'hooks': {
                'SessionStart': [{'matcher': 'startup|resume', 'hooks': [{'type': 'command', 'command': f'{sys.executable} {codex_memory}'}]}],
                'UserPromptSubmit': [{'hooks': [{'type': 'command', 'command': f'{sys.executable} {codex_memory}'}]}],
                'SessionEnd': [{'hooks': [{'type': 'command', 'command': f'{sys.executable} {codex_memory}'}]}],
            },
        })

        stream_run = case.chat('First stream task', flags=['--output-format=stream-json'], label='stream-startup')
        stream_lines = [json.loads(line) for line in stream_run.stdout.splitlines() if line.strip()]
        assert any(item['kind'] == 'session_end' and item['data']['status'] == 'succeeded' for item in stream_lines), stream_lines
        assert 'MEM_STARTUP_CONTEXT' in case.requests[0]['instructions'], case.requests[0]['instructions']
        assert 'TURN_MEM:First stream task' in case.requests[0]['instructions'], case.requests[0]['instructions']
        session_id = next(item['task_id'] for item in stream_lines if item.get('task_id'))

        resume_run = case.run(['--resume', session_id, '--prompt', 'Resumed task', '--output-format=stream-json'], label='stream-resume')
        resume_lines = [json.loads(line) for line in resume_run.stdout.splitlines() if line.strip()]
        assert any(item['kind'] == 'session_end' and item['data']['status'] == 'succeeded' for item in resume_lines), resume_lines
        assert 'MEM_RESUME_CONTEXT' in case.requests[1]['instructions'], case.requests[1]['instructions']
        assert 'MEM_STARTUP_CONTEXT' not in case.requests[1]['instructions'], case.requests[1]['instructions']
        assert 'TURN_MEM:Resumed task' in case.requests[1]['instructions'], case.requests[1]['instructions']

        with cap.Interactive(case, terminal=True) as tui:
            tui.wait('CAPABILITY_COMPLETE')
            tui.wait('Ready')
            tui.finish()
        assert 'MEM_STARTUP_CONTEXT' in case.requests[2]['instructions'], case.requests[2]['instructions']

        before_queue = len(read_jsonl(log))
        with cap.Interactive(case, terminal=False) as line:
            case.wait_requests(4)
            line.send('/queue Queued follow-up prompt')
            line.wait('Queued')
            case.release.set()
            case.wait_requests(5)
            line.wait('CAPABILITY_COMPLETE')
            line.finish()

        queued_events = [
            ev for ev in read_jsonl(log)[before_queue:]
            if ev.get('hook_event_name') == 'UserPromptSubmit' and ev.get('prompt') == 'Queued follow-up prompt'
        ]
        assert len(queued_events) == 1, queued_events

        case.steps.pop()
        events = read_jsonl(log)
        surfaces = {ev.get('meldra', {}).get('surface') for ev in events}
        assert {'stream-json', 'tui', 'line'}.issubset(surfaces), surfaces
        assert not repo_marker.exists(), 'untrusted repository hook config executed'
        case.complete()


def pretool_permission_and_boundaries(binary, output):
    with cap.Case(binary, output, 'pretool-permission-and-boundaries', []) as case:
        scripts = case.directory / 'scripts'
        scripts.mkdir(mode=0o700)
        cap.write(case.workspace / 'target.txt', 'ORIGINAL_CONTENT\n')

        claude_policy = write_script(scripts, 'claude_pretool_policy.py', """
import json, sys
payload = json.load(sys.stdin)
event = payload.get('hook_event_name')
tool_name = payload.get('tool_name', '')
tool_input = payload.get('tool_input') or {}
if event == 'PreToolUse' and tool_name == 'edit_file' and tool_input.get('path') == 'target.txt':
    sys.stderr.write('Claude-style guard blocked target.txt edit')
    sys.exit(2)
if event == 'PermissionRequest' and tool_name == 'edit_file':
    print(json.dumps({'hookSpecificOutput': {'hookEventName': 'PermissionRequest', 'decision': {'behavior': 'deny', 'message': 'PermissionRequest denied write'}}}))
""")

        slow_hook = write_script(scripts, 'slow_hook.py', """
import time
time.sleep(5)
""")

        unsupported_hook = write_script(scripts, 'unsupported_output.py', """
import json
print(json.dumps({'hookSpecificOutput': {'hookEventName': 'PreToolUse', 'updatedInput': {'path': 'other.txt'}}}))
""")

        write_hooks(case.state, {
            'hooks': {
                'PreToolUse': [{'matcher': 'edit_file', 'hooks': [{'type': 'command', 'command': f'{sys.executable} {claude_policy}'}]}],
            },
        })
        case.steps.extend([{'calls': [('edit_file', {'path': 'target.txt', 'old_str': 'ORIGINAL_CONTENT\n', 'new_str': 'MUTATED\n'})]}, {}])
        case.chat('Edit target', flags=['--output-format=stream-json'], label='pretool-deny')
        assert (case.workspace / 'target.txt').read_text(encoding='utf-8') == 'ORIGINAL_CONTENT\n'

        write_hooks(case.state, {
            'hooks': {
                'PreToolUse': [{'matcher': 'edit_file', 'hooks': [{'type': 'command', 'command': f'{sys.executable} {slow_hook}', 'timeout': 1, 'onError': 'block'}]}],
            },
        })
        case.steps.extend([{'calls': [('edit_file', {'path': 'target.txt', 'old_str': 'ORIGINAL_CONTENT\n', 'new_str': 'MUTATED_TIMEOUT\n'})]}, {}])
        case.chat('Edit target timeout', flags=['--output-format=stream-json'], label='pretool-timeout')
        assert (case.workspace / 'target.txt').read_text(encoding='utf-8') == 'ORIGINAL_CONTENT\n'

        write_hooks(case.state, {
            'hooks': {
                'PreToolUse': [{'matcher': 'edit_file', 'hooks': [{'type': 'command', 'command': f'{sys.executable} {unsupported_hook}'}]}],
            },
        })
        case.steps.extend([{'calls': [('edit_file', {'path': 'target.txt', 'old_str': 'ORIGINAL_CONTENT\n', 'new_str': 'MUTATED_REWRITE\n'})]}, {}])
        unsupported_run = case.chat('Edit target unsupported', flags=['--output-format=stream-json'], label='pretool-unsupported')
        assert 'updatedInput' in unsupported_run.stdout, unsupported_run.stdout
        assert (case.workspace / 'target.txt').read_text(encoding='utf-8') == 'ORIGINAL_CONTENT\n'

        records = case.audit()
        for record in records:
            for call in record['tool_calls']:
                assert call['status'] == 'declined', call
                assert not call.get('started_at'), call

        write_hooks(case.state, {
            'hooks': {
                'SessionStart': [{'hooks': [{'type': 'command', 'command': f'{sys.executable} {slow_hook}', 'timeout': 1, 'onError': 'ignore'}]}],
                'PreToolUse': [{'matcher': 'edit_file', 'hooks': [{'type': 'command', 'command': f'{sys.executable} -c "print(\'{{}}\')"'}]}],
            },
        })
        case.steps.extend([{'calls': [('edit_file', {'path': 'target.txt', 'old_str': 'ORIGINAL_CONTENT\n', 'new_str': 'APPROVAL_STILL_REQUIRED\n'})]}])
        pending = case.chat('Needs approval despite passing hook', flags=['--output-format=stream-json'], success=False, label='passing-hook-needs-approval')
        assert pending.returncode == 3, (pending.returncode, pending.stdout)
        assert (case.workspace / 'target.txt').read_text(encoding='utf-8') == 'ORIGINAL_CONTENT\n'

        case.steps.extend([{'calls': [('edit_file', {'path': 'target.txt', 'old_str': 'ORIGINAL_CONTENT\n', 'new_str': 'PLAN_BLOCKED\n'})]}, {}])
        case.chat('Plan cannot be bypassed', flags=['--mode', 'plan', '--output-format=stream-json'], label='passing-hook-plan-mode')
        assert (case.workspace / 'target.txt').read_text(encoding='utf-8') == 'ORIGINAL_CONTENT\n'

        write_hooks(case.state, {
            'hooks': {
                'PreToolUse': [{'hooks': [{'type': 'http', 'command': 'https://example.com'}]}],
            },
        })
        bad_cfg = case.run(['hooks'], success=False, label='bad-config-http')
        assert 'unsupported hook handler type' in bad_cfg.stderr, bad_cfg.stderr
        case.complete()


def runend_observer_and_dispositions(binary, output):
    with cap.Case(binary, output, 'runend-observer-and-dispositions', []) as case:
        scripts = case.directory / 'scripts'
        scripts.mkdir(mode=0o700)
        cap.write(case.workspace / 'item.txt', 'before\n')
        obs_log = case.directory / 'observer.jsonl'

        observer = write_script(scripts, 'observer.py', f"""
import json, subprocess, sys
payload = json.load(sys.stdin)
event = payload.get('hook_event_name')
record = {{'event': event, 'payload': payload}}
if event == 'RunEnd':
    task_id = payload['meldra']['task_id']
    shown = subprocess.check_output([{str(binary)!r}, 'task', 'show', task_id, '--json'], text=True)
    record['task_show'] = json.loads(shown)
with open({str(obs_log)!r}, 'a', encoding='utf-8') as f:
    f.write(json.dumps(record) + '\\n')
if event == 'RunEnd':
    print(json.dumps({{'decision': 'block', 'reason': 'observer must not continue'}}))
""")

        write_hooks(case.state, {
            'hooks': {
                'PermissionRequest': [{'hooks': [{'type': 'command', 'command': f'{sys.executable} {observer}'}]}],
                'PostToolUse': [{'hooks': [{'type': 'command', 'command': f'{sys.executable} {observer}'}]}],
                'PostToolUseFailure': [{'hooks': [{'type': 'command', 'command': f'{sys.executable} {observer}'}]}],
                'RunEnd': [{'hooks': [{'type': 'command', 'command': f'{sys.executable} {observer}'}]}],
            },
        })

        case.steps.extend([{'calls': [('read_file', {'path': 'missing-file.txt', 'offset': None, 'limit': None})]}, {}])
        case.chat('Read missing file', flags=['--output-format=stream-json'], label='known-failure')

        case.steps.extend([{'calls': [('edit_file', {'path': 'item.txt', 'old_str': 'before\n', 'new_str': 'after\n'})]}])
        pending = case.chat('Edit item', flags=['--output-format=stream-json'], success=False, label='pending-approval')
        assert pending.returncode == 3, (pending.returncode, pending.stdout)
        approval_ev = next(json.loads(line) for line in pending.stdout.splitlines() if json.loads(line)['kind'] == 'approval_required')
        task_id = approval_ev['task_id']
        detail_hash = approval_ev['data']['detail_hash']

        case.steps.extend([{}])
        resumed = case.run(['task', 'resume', task_id, '--approve-detail-hash', detail_hash], label='explicit-resume')
        assert resumed.returncode == 0, resumed.stdout
        assert (case.workspace / 'item.txt').read_text(encoding='utf-8') == 'after\n'
        permissions = [r['payload'] for r in read_jsonl(obs_log) if r['event'] == 'PermissionRequest']
        assert [(p['meldra']['run_id'], p['tool_name']) for p in permissions] == [(approval_ev['run_id'], 'edit_file')], permissions

        case.steps.extend([{'hold': True}])
        with cap.Interactive(case, terminal=False) as line:
            case.wait_requests(len(case.requests) + 1)
            line.send('/stop')
            line.wait('Turn stopped')
            line.finish()

        records = read_jsonl(obs_log)
        post_failures = [r for r in records if r['event'] == 'PostToolUseFailure']
        post_successes = [r for r in records if r['event'] == 'PostToolUse']
        run_ends = [r for r in records if r['event'] == 'RunEnd']
        assert len(post_failures) == 1 and post_failures[0]['payload']['tool_name'] == 'read_file', post_failures
        assert any(r['payload']['tool_name'] == 'edit_file' for r in post_successes), post_successes

        waiting = [r for r in run_ends if r['payload']['meldra']['run_status'] == 'waiting_approval']
        resumed_ends = [r for r in run_ends if r['payload']['meldra']['task_id'] == task_id and r['payload']['meldra']['run_status'] == 'succeeded']
        cancelled_ends = [r for r in run_ends if r['payload']['meldra']['run_status'] == 'cancelled']
        assert len(waiting) == 1, run_ends
        assert waiting[0]['task_show']['task']['status'] == 'waiting_approval', waiting[0]
        assert len(resumed_ends) == 1, run_ends
        assert resumed_ends[0]['payload']['meldra']['run_id'] != waiting[0]['payload']['meldra']['run_id'], (waiting[0], resumed_ends[0])
        assert resumed_ends[0]['task_show']['task']['status'] == 'completed', resumed_ends[0]
        assert len(cancelled_ends) == 1 and cancelled_ends[0]['task_show']['task']['status'] == 'cancelled', cancelled_ends

        before_inspect = len(read_jsonl(obs_log))
        case.run(['task', 'show', task_id, '--json'], label='inspect-no-replay')
        assert len(read_jsonl(obs_log)) == before_inspect
        case.complete()


def stop_validator_and_mcp_serve(binary, output):
    with cap.Case(binary, output, 'stop-validator-and-mcp-serve', [{}, {}]) as case:
        scripts = case.directory / 'scripts'
        scripts.mkdir(mode=0o700)
        cap.write(case.workspace / 'AGENTS.md', 'MCP hook check.\n')
        cap.write(case.workspace / 'hello.txt', 'unchanged\n')
        stop_log = case.directory / 'stop.jsonl'
        mcp_log = case.directory / 'mcp.jsonl'

        stop_validator = write_script(scripts, 'stop_validator.py', f"""
import json, sys
payload = json.load(sys.stdin)
with open({str(stop_log)!r}, 'a', encoding='utf-8') as f:
    f.write(json.dumps(payload) + '\\n')
print(json.dumps({{'decision': 'block', 'reason': 'Run final check before stopping.'}}))
""")

        mcp_hook = write_script(scripts, 'mcp_hook.py', f"""
import json, sys
payload = json.load(sys.stdin)
with open({str(mcp_log)!r}, 'a', encoding='utf-8') as f:
    f.write(json.dumps(payload) + '\\n')
sys.stderr.write('mcp hook stderr noise\\n')
if payload.get('hook_event_name') == 'PreToolUse' and payload.get('tool_name') == 'edit_file':
    print(json.dumps({{'hookSpecificOutput': {{'hookEventName': 'PreToolUse', 'permissionDecision': 'deny', 'permissionDecisionReason': 'blocked in MCP'}}}}))
""")

        write_hooks(case.state, {
            'hooks': {
                'Stop': [{'hooks': [{'type': 'command', 'command': f'{sys.executable} {stop_validator}'}]}],
            },
        })
        res = case.chat('Finish work', flags=['--output-format=stream-json'], label='stop-continue-once')
        lines = [json.loads(line) for line in res.stdout.splitlines() if line.strip()]
        assert any(item['kind'] == 'session_end' and item['data']['status'] == 'succeeded' for item in lines), lines
        stop_events = read_jsonl(stop_log)
        assert len(stop_events) == 2, stop_events
        assert stop_events[0]['stop_hook_active'] is False and stop_events[1]['stop_hook_active'] is True, stop_events
        assert len(case.requests) == 2, len(case.requests)

        write_hooks(case.state, {
            'hooks': {
                'SessionStart': [{'hooks': [{'type': 'command', 'command': f'{sys.executable} {mcp_hook}'}]}],
                'SessionEnd': [{'hooks': [{'type': 'command', 'command': f'{sys.executable} {mcp_hook}'}]}],
                'UserPromptSubmit': [{'hooks': [{'type': 'command', 'command': f'{sys.executable} {mcp_hook}'}]}],
                'Stop': [{'hooks': [{'type': 'command', 'command': f'{sys.executable} {mcp_hook}'}]}],
                'PreToolUse': [{'hooks': [{'type': 'command', 'command': f'{sys.executable} {mcp_hook}'}]}],
                'PostToolUse': [{'hooks': [{'type': 'command', 'command': f'{sys.executable} {mcp_hook}'}]}],
                'RunEnd': [{'hooks': [{'type': 'command', 'command': f'{sys.executable} {mcp_hook}'}]}],
            },
        })
        proc = subprocess.Popen(
            [str(binary), 'mcp', 'serve', '--workspace', str(case.workspace)],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            cwd=case.workspace, env=case.env, text=True,
        )
        try:
            def rpc(msg):
                proc.stdin.write(json.dumps(msg) + '\n')
                proc.stdin.flush()
                return json.loads(proc.stdout.readline())

            init = rpc({'jsonrpc': '2.0', 'id': 1, 'method': 'initialize', 'params': {'protocolVersion': '2025-11-25', 'clientInfo': {'name': 'hooks-e2e', 'version': '1'}, 'capabilities': {}}})
            assert 'result' in init, init
            proc.stdin.write(json.dumps({'jsonrpc': '2.0', 'method': 'notifications/initialized'}) + '\n')
            proc.stdin.flush()

            read_resp = rpc({'jsonrpc': '2.0', 'id': 2, 'method': 'tools/call', 'params': {'name': 'read_file', 'arguments': {'path': 'hello.txt', 'offset': None, 'limit': None}}})
            assert read_resp['result'].get('isError') is not True, read_resp

            edit_resp = rpc({'jsonrpc': '2.0', 'id': 3, 'method': 'tools/call', 'params': {'name': 'edit_file', 'arguments': {'path': 'hello.txt', 'old_str': 'unchanged', 'new_str': 'broken'}}})
            assert edit_resp['result'].get('isError') is True, edit_resp
            assert (case.workspace / 'hello.txt').read_text(encoding='utf-8') == 'unchanged\n'
        finally:
            proc.stdin.close()
            proc.wait(timeout=10)

        mcp_events = read_jsonl(mcp_log)
        names = [ev['hook_event_name'] for ev in mcp_events]
        assert not ({'SessionStart', 'SessionEnd', 'UserPromptSubmit', 'Stop'} & set(names)), names
        assert {'PreToolUse', 'PostToolUse', 'RunEnd'}.issubset(set(names)), names
        assert all(ev.get('meldra', {}).get('surface') == 'mcp' for ev in mcp_events), mcp_events
        case.complete()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    args.output.mkdir(parents=True, exist_ok=False)
    report = {'passed': False, 'scenarios': []}
    scenarios = [
        ('external-memory-and-surfaces', external_memory_and_surfaces),
        ('pretool-permission-and-boundaries', pretool_permission_and_boundaries),
        ('runend-observer-and-dispositions', runend_observer_and_dispositions),
        ('stop-validator-and-mcp-serve', stop_validator_and_mcp_serve),
    ]
    for name, scenario in scenarios:
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
