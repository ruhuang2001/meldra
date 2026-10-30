#!/usr/bin/env python3
"""Real-binary loopback compaction E2E; failures specified first in the matrix."""
import argparse
import fcntl
import hashlib
import http.server
import importlib.util
import json
import os
from pathlib import Path
import platform
import re
import signal
import sqlite3
import subprocess
import sys
import time
import traceback

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location('compaction_capability', ROOT / 'scripts/capability-e2e.py')
cap = importlib.util.module_from_spec(spec)
spec.loader.exec_module(cap)
write, dump, Interactive = cap.write, cap.dump, cap.Interactive
image_spec = importlib.util.spec_from_file_location('compaction_images', ROOT / 'scripts/images-e2e.py')
images = importlib.util.module_from_spec(image_spec)
image_spec.loader.exec_module(images)


class Case(cap.Case):
    def __init__(self, *args, **kwargs):
        super().__init__(*args, **kwargs)
        self.env['MELDRA_DISABLE_UPDATE_CHECK'] = '1'
        self.configure(compact_keep_messages=2, compact_threshold_percent=0)
        case = self

        class Handler(http.server.BaseHTTPRequestHandler):
            def log_message(self, *_args):
                pass

            def do_POST(self):
                try:
                    assert self.path in ('/v1/responses', '/responses'), self.path
                    request = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
                    with case.condition:
                        case.requests.append(request)
                        index = len(case.requests)
                        assert case.steps, 'unexpected provider request'
                        step = case.steps.pop(0)
                        case.condition.notify_all()
                    if callable(step):
                        step = step(case, request)
                    if step.get('summary'):
                        assert not request.get('tools'), request
                        assert request.get('max_output_tokens') == 1024, request
                        assert request.get('model') == 'capability-fixture', request
                        assert not request.get('previous_response_id'), request
                        assert 'PRIVATE_REASONING_CANARY' not in json.dumps(request), request
                        assert case.env['OPENAI_API_KEY'] not in json.dumps(request), request
                    if step.get('http_error'):
                        body = json.dumps({'error': {'message': 'COMPACTION_PROVIDER_FAILURE', 'type': 'invalid_request_error'}}).encode()
                        self.send_response(400)
                        self.send_header('Content-Length', str(len(body)))
                        self.end_headers()
                        self.wfile.write(body)
                        return
                    self.send_response(200)
                    self.send_header('Content-Type', 'text/event-stream')
                    self.send_header('Connection', 'close')
                    self.end_headers()
                    if step.get('wait'):
                        self.wfile.write(b': COMPACTION_WAITING\n\n')
                        self.wfile.flush()
                        while not case.release.wait(0.05):
                            self.wfile.write(b': heartbeat\n\n')
                            self.wfile.flush()
                    output = [{'type': 'function_call', 'id': f'fc_{index}_{i}', 'call_id': f'call_{index}_{i}',
                               'name': name, 'arguments': json.dumps(arguments), 'status': 'completed'}
                              for i, (name, arguments) in enumerate(step.get('calls', []))]
                    if not output:
                        output = [{'type': 'message', 'id': f'msg_{index}', 'role': 'assistant', 'status': 'completed',
                                   'content': [{'type': 'output_text', 'text': step.get('text', f'READY_{index}'), 'annotations': []}]}]
                    if step.get('reasoning'):
                        output.insert(0, {'type': 'reasoning', 'id': f'reason_{index}',
                                          'summary': [{'type': 'summary_text', 'text': 'PRIVATE_REASONING_CANARY'}]})
                    response = {'id': f'resp_{index}', 'object': 'response', 'status': step.get('status', 'completed'),
                                'output': output, 'usage': {'input_tokens': 1, 'output_tokens': 1, 'total_tokens': 2}}
                    if response['status'] == 'incomplete':
                        response['incomplete_details'] = {'reason': 'max_output_tokens'}
                    event = {'type': 'response.' + response['status'], 'response': response}
                    self.wfile.write(('event: ' + event['type'] + '\ndata: ' + json.dumps(event) + '\n\n').encode())
                    self.wfile.flush()
                except (BrokenPipeError, ConnectionResetError):
                    case.cancelled.set()
                except Exception:
                    case.errors.append(traceback.format_exc())
                    try:
                        self.send_error(500)
                    except (BrokenPipeError, ConnectionResetError):
                        pass

        self.server.RequestHandlerClass = Handler

    def configure(self, **values):
        path = self.state / 'config.toml'
        data = path.read_text()
        for key, value in values.items():
            data = re.sub(r'^' + re.escape(key) + r'=.*\n', '', data, flags=re.M)
            data += key + '=' + json.dumps(value) + '\n'
        path.write_text(data)


def snapshot(case):
    paths = list((case.state / 'tasks/sessions').glob('*.json'))
    assert len(paths) == 1, paths
    return paths[0], json.loads(paths[0].read_text())


def stable_session(value):
    return {key: value.get(key) for key in ('summary', 'messages', 'previous_response_id', 'plan', 'mode',
                                            'context_compaction', 'compacted_through_sequence')}


def wait_snapshot(case, client, predicate, timeout=15):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            path, value = snapshot(case)
            if predicate(value):
                return path, value
        except (AssertionError, json.JSONDecodeError):
            pass
        client.read(0.03)
    raise AssertionError('snapshot did not reach expected state: ' + str(snapshot(case)))


def turn(case, client, text, marker, messages):
    client.send(text)
    client.wait(marker)
    return wait_snapshot(case, client, lambda value: len(value.get('messages', [])) >= messages)[1]


def ledger(case):
    record = case.audit()[0]
    page = record['events']
    while len(page) == 1000:
        cursor = page[-1]['sequence']
        result = case.run(['task', 'events', record['task']['id'], '--json', '--after', str(cursor)],
                          label='events-after-' + str(cursor))
        page = [json.loads(line) for line in result.stdout.splitlines() if line.strip()]
        assert all(event['sequence'] > cursor for event in page)
        record['events'].extend(page)
    dump(case.directory / 'task-records.json', [record])
    return record


def compactions(record):
    return [event['data'] for event in record['events'] if event['kind'] == 'context.compacted']


def artifact(case, ref):
    path = case.state / 'tasks/artifacts' / ref['id']
    raw = path.read_bytes()
    assert len(raw) == ref['size'] and hashlib.sha256(raw).hexdigest() == ref['sha256'] == ref['id'], ref
    return path, json.loads(raw)


def summary(text):
    return {'summary': True, 'text': text}


def tool_steps(text='FIRST_READY'):
    return [{'calls': [('read_file', {'path': 'note.txt'}),
                       ('update_plan', {'steps': ['Inspect the fixture', 'Verify preserved evidence']})]},
            {'text': text, 'reasoning': True}]


def seed(case, client):
    client.wait('FIRST_READY')
    wait_snapshot(case, client, lambda value: len(value.get('messages', [])) == 2)
    return turn(case, client, 'SECOND_ORIGINAL_REQUEST', 'SECOND_READY', 4)


def manual(binary, output, terminal=False):
    name = 'manual-pty' if terminal else 'manual-line'
    original = 'LONG_ORIGINAL_BEGIN ' + ('界abc ' * 4200 if not terminal else 'PTY_VERBATIM ') + ' LONG_ORIGINAL_END'
    steps = tool_steps() + [{'calls': [('read_file', {'path': 'note.txt'})]}, {'text': 'LONG_READY'},
                            {'text': 'REFERENCE_READY'}, summary('SUMMARY_ONE decisions and verification.'),
                            {'text': 'AFTER_FIRST_READY'}, summary('SUMMARY_TWO all prior decisions.'), {'text': 'RESUMED_READY'}]
    with Case(binary, output, name, steps) as case:
        write(case.workspace / 'note.txt', 'ORIGINAL_REFERENCE_CONTENT\n')
        with Interactive(case, terminal=terminal, flags=['--mode', 'plan']) as client:
            client.wait('FIRST_READY')
            wait_snapshot(case, client, lambda value: len(value.get('messages', [])) == 2)
            turn(case, client, original, 'LONG_READY', 4)
            before = turn(case, client, 'Keep this attachment @note.txt', 'REFERENCE_READY', 6)
            before_record = ledger(case)
            original_calls = before_record['tool_calls']
            original_artifacts = {path.name: hashlib.sha256(path.read_bytes()).hexdigest()
                                  for path in (case.state / 'tasks/artifacts').iterdir() if path.is_file()}
            dump(case.directory / 'before-session.json', before)
            dump(case.directory / 'before-calls.json', original_calls)
            client.send('/compact preserve verification')
            client.wait('Compacted')
            _, first = wait_snapshot(case, client, lambda value: value.get('summary') == 'SUMMARY_ONE decisions and verification.')
            assert len(first['messages']) == 2 and first['messages'] == before['messages'][-2:], first
            assert first['plan'] == before['plan'] and first['mode'] == 'plan'
            assert not first.get('previous_response_id')
            assert first['messages'][0]['references'], first
            first_record = ledger(case)
            events = compactions(first_record)
            assert len(events) == 1 and events[0]['trigger'] == 'manual'
            _, payload = artifact(case, events[0]['artifact'])
            assert payload['schema_version'] == 1 and payload['summary'] == first['summary']
            assert any(request['text'] == original for request in payload['requests']), payload
            assert len({request['sequence'] for request in payload['requests']}) == len(payload['requests'])
            assert {call['id'] for call in original_calls} <= set(payload['compacted_calls'])
            assert {call['id'] for call in original_calls} <= {call['id'] for call in payload['calls']}
            assert all(call['arguments_digest'] and call['status'] == 'succeeded' for call in payload['calls'])
            assert first_record['tool_calls'] == original_calls
            summary_request = next(request for request in case.requests if request.get('max_output_tokens') == 1024)
            prompt = json.loads(summary_request['input'])
            source_evidence = prompt['ToolEvidence']
            assert {call['id'] for call in original_calls} == {call['id'] for call in source_evidence}
            assert all(call['arguments'] and call['arguments_digest'] for call in source_evidence)
            assert any('ORIGINAL_REFERENCE_CONTENT' in call['output'] for call in source_evidence)
            inspected = first_record['compactions']
            assert len(inspected) == 1 and inspected[0]['compacted_calls'] == payload['compacted_calls']
            assert inspected[0]['tool_calls'] == original_calls
            assert inspected[0]['compacted_call_count'] == len(original_calls)
            assert 'PRIVATE_REASONING_CANARY' not in json.dumps(payload)
            task_id = first_record['task']['id']
            shown = case.run(['task', 'show', task_id], label='compacted-human').stdout
            assert all(call['id'] in shown for call in original_calls) and 'compaction' in shown.lower(), shown
            write(case.workspace / 'note.txt', 'MUTATED_REFERENCE_MUST_NOT_REPLACE_SNAPSHOT\n')
            turn(case, client, 'Continue after the first summary.', 'AFTER_FIRST_READY', 4)
            rebuilt = json.dumps(case.requests[-1], ensure_ascii=False)
            assert 'SUMMARY_ONE' in case.requests[-1]['instructions']
            assert '[Prior context summary' in case.requests[-1]['instructions']
            assert original in rebuilt and not case.requests[-1].get('previous_response_id')
            assert 'ORIGINAL_REFERENCE_CONTENT' in rebuilt and 'MUTATED_REFERENCE_MUST_NOT_REPLACE_SNAPSHOT' not in rebuilt
            assert all(call['id'] in rebuilt for call in original_calls), rebuilt
            after = len(client.transcript)
            client.send('/compact preserve all original requests')
            client.wait('Compacted', after=after)
            _, second = wait_snapshot(case, client, lambda value: value.get('summary') == 'SUMMARY_TWO all prior decisions.')
            assert second['plan'] == before['plan'] and second['mode'] == 'plan'
            assert not second.get('previous_response_id')
            client.finish()
        case.run(['resume', second['id'], '--prompt', 'Resume after repeated compaction.'], label='resume')
        rebuilt = json.dumps(case.requests[-1], ensure_ascii=False)
        assert 'SUMMARY_TWO' in case.requests[-1]['instructions']
        assert original in rebuilt and rebuilt.count('LONG_ORIGINAL_BEGIN') == 1
        assert 'ORIGINAL_REFERENCE_CONTENT' in rebuilt and 'MUTATED_REFERENCE_MUST_NOT_REPLACE_SNAPSHOT' not in rebuilt
        assert not case.requests[-1].get('previous_response_id')
        final_record = ledger(case)
        assert len(compactions(final_record)) == 2 and final_record['tool_calls'] == original_calls
        for digest, expected in original_artifacts.items():
            assert hashlib.sha256((case.state / 'tasks/artifacts' / digest).read_bytes()).hexdigest() == expected
        assert sorted(path.name for path in case.workspace.iterdir()) == ['note.txt']
        case.complete()


def wait_exit(client, timeout=15):
    deadline = time.monotonic() + timeout
    while client.process.poll() is None:
        assert time.monotonic() < deadline, 'application did not stop after failure'
        client.read(0.05)
    client.read()


def folded_image(binary, output):
    data = images.png()
    with Case(binary, output, 'folded-image', [{'text': 'FIRST_READY'}, {'text': 'IMAGE_READY'},
              {'text': 'TAIL_READY'}, summary('IMAGE_SUMMARY'), {'text': 'IMAGE_RESUMED'}]) as case:
        write(case.workspace / 'shot.png', data)
        with Interactive(case, flags=['--mode', 'plan']) as client:
            client.wait('FIRST_READY')
            wait_snapshot(case, client, lambda value: len(value.get('messages', [])) == 2)
            turn(case, client, 'Preserve the original image @shot.png', 'IMAGE_READY', 4)
            images.assert_image(case.requests[-1], data)
            turn(case, client, 'Keep this recent tail.', 'TAIL_READY', 6)
            client.send('/compact preserve attachment provenance')
            client.wait('Compacted')
            _, saved = wait_snapshot(case, client, lambda value: value.get('summary') == 'IMAGE_SUMMARY')
            assert not any(message.get('references') for message in saved['messages'])
            client.finish()
        (case.workspace / 'shot.png').unlink()
        case.run(['resume', saved['id'], '--prompt', 'Use the preserved image.'], label='image-resume')
        images.assert_image(case.requests[-1], data)
        images.assert_declarations(case.requests[-1], [data])
        assert len(images.images(case.requests[-1])) == 1
        assert (case.state / 'tasks/artifacts' / hashlib.sha256(data).hexdigest()).read_bytes() == data
        case.complete()


def folded_image_priority(binary, output):
    with Case(binary, output, 'folded-image-priority',
              [{'text': 'FIRST_READY'}, {'text': 'IMAGE0_READY'}, {'text': 'IMAGE1_READY'},
               {'text': 'IMAGE2_READY'}, {'text': 'TAIL_READY'}, summary('PRIORITY_SUMMARY'),
               {'text': 'PRIORITY_RESUMED'}]) as case:
        blobs = []
        for index, color in enumerate([(1, 2, 3), (4, 5, 6), (7, 8, 9)]):
            data = images.png(color)
            data += b'x' * ((1 << 20) - len(data))
            blobs.append(data)
            write(case.workspace / ('shot' + str(index) + '.png'), data)
        with Interactive(case, flags=['--mode', 'plan']) as client:
            client.wait('FIRST_READY')
            wait_snapshot(case, client, lambda value: len(value.get('messages', [])) == 2)
            for index in range(3):
                turn(case, client, '@shot' + str(index) + '.png', 'IMAGE' + str(index) + '_READY', 4 + index * 2)
            turn(case, client, 'Keep only this text tail.', 'TAIL_READY', 10)
            client.send('/compact')
            client.wait('Compacted')
            _, saved = wait_snapshot(case, client, lambda value: value.get('summary') == 'PRIORITY_SUMMARY')
            client.finish()
        case.run(['resume', saved['id'], '--prompt', 'Use newest image snapshots.'], label='resume')
        assert len(images.images(case.requests[-1])) == 2
        images.assert_image(case.requests[-1], blobs[2])
        images.assert_image(case.requests[-1], blobs[1])
        images.assert_declarations(case.requests[-1], [blobs[2], blobs[1]])
        case.complete()


def odd_tail(binary, output):
    with Case(binary, output, 'odd-tail', tool_steps() + [{'text': 'SECOND_READY'}, {'text': 'THIRD_READY'},
              summary('ODD_SUMMARY_ONE'), {'text': 'ODD_CONTINUED'}, summary('ODD_SUMMARY_TWO'), {'text': 'ODD_RESUMED'}]) as case:
        case.configure(compact_keep_messages=3)
        write(case.workspace / 'note.txt', 'odd tail fixture\n')
        with Interactive(case) as client:
            seed(case, client)
            before = turn(case, client, 'THIRD_ORIGINAL_REQUEST', 'THIRD_READY', 6)
            client.send('/compact')
            client.wait('Compacted')
            _, first = wait_snapshot(case, client, lambda value: value.get('summary') == 'ODD_SUMMARY_ONE')
            assert first['messages'] == before['messages'][-3:]
            turn(case, client, 'FOURTH_ORIGINAL_REQUEST', 'ODD_CONTINUED', 5)
            after = len(client.transcript)
            client.send('/compact')
            client.wait('Compacted', after=after)
            _, saved = wait_snapshot(case, client, lambda value: value.get('summary') == 'ODD_SUMMARY_TWO')
            client.finish()
        case.run(['resume', saved['id'], '--prompt', 'Check the odd tail.'], label='resume')
        text = json.dumps(case.requests[-1]).split('[Compacted execution evidence', 1)[-1]
        for original in ('Initial controlled request.', 'SECOND_ORIGINAL_REQUEST', 'THIRD_ORIGINAL_REQUEST', 'FOURTH_ORIGINAL_REQUEST'):
            assert text.count(original) == 1, (original, text)
        assert len(compactions(ledger(case))) == 2
        case.complete()


def steering(binary, output):
    with Case(binary, output, 'steering', [{'wait': True}, {'text': 'STEER_READY'}, {'text': 'NEXT_READY'},
              summary('STEER_SUMMARY_ONE'), {'text': 'STEER_CONTINUED'}, summary('STEER_SUMMARY_TWO'), {'text': 'STEER_RESUMED'}]) as case:
        write(case.workspace / 'note.txt', 'STEERING_REFERENCE_CONTENT\n')
        with Interactive(case) as client:
            case.wait_requests(1)
            client.send('/steer STEER_ORIGINAL_REQUEST @note.txt')
            assert case.cancelled.wait(10), 'steering did not cancel initial inference'
            client.wait('STEER_READY')
            wait_snapshot(case, client, lambda value: len(value.get('messages', [])) == 3)
            sql(case, "DELETE FROM events WHERE json_extract(CAST(record AS TEXT),'$.kind')='control.applied'")
            turn(case, client, 'NEXT_ORIGINAL_REQUEST', 'NEXT_READY', 5)
            client.send('/compact preserve steering')
            client.wait('Compacted')
            wait_snapshot(case, client, lambda value: value.get('summary') == 'STEER_SUMMARY_ONE')
            turn(case, client, 'FINAL_ORIGINAL_REQUEST', 'STEER_CONTINUED', 4)
            after = len(client.transcript)
            client.send('/compact preserve steering again')
            client.wait('Compacted', after=after)
            _, saved = wait_snapshot(case, client, lambda value: value.get('summary') == 'STEER_SUMMARY_TWO')
            client.finish()
        (case.workspace / 'note.txt').unlink()
        case.run(['resume', saved['id'], '--prompt', 'Resume steering history.'], label='resume')
        text = json.dumps(case.requests[-1]).split('[Compacted execution evidence', 1)[-1]
        for original in ('Initial controlled request.', 'STEER_ORIGINAL_REQUEST', 'NEXT_ORIGINAL_REQUEST', 'FINAL_ORIGINAL_REQUEST'):
            assert text.count(original) == 1, (original, text)
        assert 'STEERING_REFERENCE_CONTENT' in json.dumps(case.requests[-1])
        event = compactions(ledger(case))[-1]
        _, payload = artifact(case, event['artifact'])
        assert any(request.get('control_id') for request in payload['requests'])
        case.complete()


def compact_controls(binary, output):
    with Case(binary, output, 'early-compact-pty', [{'text': 'SHORT_READY'}, {'text': 'AFTER_NOOP_READY'}]) as case:
        with Interactive(case, terminal=True) as client:
            client.wait('SHORT_READY')
            wait_snapshot(case, client, lambda value: len(value.get('messages', [])) == 2)
            client.send('/compact')
            deadline = time.monotonic() + 2
            while time.monotonic() < deadline:
                client.read(0.03)
            client.send('Continue after no-op compaction.')
            client.wait('AFTER_NOOP_READY')
            client.finish()
        assert not compactions(ledger(case))
        case.complete()
    with Case(binary, output, 'threshold-summary-steering', [{'text': 'A' * 6500 + ' FIRST_READY'},
              {'text': 'B' * 6500 + ' SECOND_READY'}, dict(summary('UNUSED'), wait=True), {'text': 'THRESHOLD_STEER_FINISHED'}]) as case:
        case.configure(max_turn_context_bytes=24000, compact_threshold_percent=50)
        with Interactive(case) as client:
            client.wait('FIRST_READY')
            wait_snapshot(case, client, lambda value: len(value.get('messages', [])) == 2)
            turn(case, client, 'Second threshold request.', 'SECOND_READY', 4)
            client.send('Third threshold request.')
            case.wait_requests(3)
            client.send('/steer CORRECTION_DURING_AUTO_COMPACTION')
            assert case.cancelled.wait(10), 'steering did not cancel automatic summary inference'
            client.wait('THRESHOLD_STEER_FINISHED')
            assert 'CORRECTION_DURING_AUTO_COMPACTION' in json.dumps(case.requests[-1])
            client.finish()
        assert not compactions(ledger(case))
        assert not snapshot(case)[1].get('summary')
        case.complete()


def validation(binary, output):
    variants = {'empty': summary('  '), 'oversized': summary('s' * (32768 + 1)),
                'incomplete': dict(summary('TRUNCATED_SUMMARY'), status='incomplete'),
                'tool-bearing': dict(summary(''), calls=[cap.edit('FORBIDDEN.txt', 'must not execute')]),
                'provider-error': dict(summary(''), http_error=True),
                'credential-echo': summary('Do not retain offline-fixture-not-a-credential')}
    for name, response in variants.items():
        with Case(binary, output, 'invalid-' + name, tool_steps() + [{'text': 'SECOND_READY'}, response]) as case:
            write(case.workspace / 'note.txt', 'safe fixture\n')
            with Interactive(case) as client:
                before = seed(case, client)
                client.send('/compact')
                case.wait_requests(4)
                deadline = time.monotonic() + 10
                while client.process.poll() is None and time.monotonic() < deadline:
                    client.read(0.03)
                    transcript = client.transcript.decode(errors='replace').lower()
                    if any(word in transcript for word in ('compact failed', 'compaction failed', 'summary', 'provider_failure', 'incomplete')):
                        break
                if client.process.poll() is None:
                    client.process.stdin.close()
                wait_exit(client)
            assert stable_session(snapshot(case)[1]) == stable_session(before), name
            assert not (case.workspace / 'FORBIDDEN.txt').exists()
            assert not compactions(ledger(case))
            case.complete()


def sql(case, statement, parameters=()):
    with sqlite3.connect(case.state / 'tasks/tasks.db', timeout=10) as connection:
        connection.execute(statement, parameters)


def persistence(binary, output):
    for stage in ('artifact', 'snapshot', 'event'):
        def fail(case, _request):
            if stage == 'snapshot':
                path, value = snapshot(case)
                path.write_text(path.read_text() + '\n')
            else:
                kind = 'artifact.saved' if stage == 'artifact' else 'context.compacted'
                sql(case, "CREATE TRIGGER compaction_fault BEFORE INSERT ON events WHEN json_extract(CAST(NEW.record AS TEXT),'$.kind')='" + kind + "' BEGIN SELECT RAISE(ABORT,'COMPACTION_INJECTED_FAILURE'); END")
            return summary('PERSISTENCE_SUMMARY')
        with Case(binary, output, 'persistence-' + stage, tool_steps() + [{'text': 'SECOND_READY'}, fail]) as case:
            write(case.workspace / 'note.txt', 'safe fixture\n')
            with Interactive(case) as client:
                before = seed(case, client)
                dump(case.directory / 'before-session.json', before)
                client.send('/compact')
                case.wait_requests(4)
                wait_exit(client)
                assert client.process.returncode != 0, client.transcript.decode(errors='replace')
            after = snapshot(case)[1]
            dump(case.directory / 'after-session.json', after)
            assert not compactions(ledger(case))
            if stage == 'event':
                assert after['summary'] == 'PERSISTENCE_SUMMARY' and not after.get('previous_response_id')
                artifact(case, after['context_compaction'])
            else:
                assert stable_session(after) == stable_session(before), stage
            if stage != 'artifact':
                saved = [event for event in ledger(case)['events'] if event['kind'] == 'artifact.saved' and event['data']['name'] == 'context-compaction.json']
                assert len(saved) == 1
                artifact(case, saved[0]['data'])
            if stage == 'event':
                before_calls = ledger(case)['tool_calls']
                sql(case, 'DROP TRIGGER compaction_fault')
                case.steps.append({'text': 'EVENT_GAP_RESUMED'})
                count = len(case.requests)
                case.run(['resume', after['id'], '--prompt', 'Continue after audit event failure.'], label='event-gap-resume')
                assert len(case.requests) == count + 1 and case.requests[-1].get('tools')
                assert 'PERSISTENCE_SUMMARY' in case.requests[-1]['instructions']
                recovered = snapshot(case)[1]
                assert recovered['summary'] == after['summary'] and recovered['context_compaction'] == after['context_compaction']
                assert not compactions(ledger(case)) and ledger(case)['tool_calls'] == before_calls
            case.complete()


def corruption(binary, output):
    with Case(binary, output, 'corruption', tool_steps() + [{'text': 'SECOND_READY'}, summary('CORRUPTION_SUMMARY')]) as case:
        write(case.workspace / 'note.txt', 'safe fixture\n')
        with Interactive(case) as client:
            seed(case, client)
            client.send('/compact')
            client.wait('Compacted')
            _, saved = wait_snapshot(case, client, lambda value: value.get('summary') == 'CORRUPTION_SUMMARY')
            client.finish()
        path, _ = artifact(case, saved['context_compaction'])
        path.write_bytes(b'corrupted compaction evidence')
        before = stable_session(snapshot(case)[1])
        for mode in ('human', 'json'):
            args = ['task', 'show', saved['id']] + (['--json'] if mode == 'json' else [])
            rejected = case.run(args, success=False, label='corrupt-inspection-' + mode)
            assert 'artifact' in rejected.stderr.lower() and not rejected.stdout
        result = case.run(['resume', saved['id'], '--prompt', 'Must fail before inference.'], success=False, label='corrupt-resume')
        assert any(word in result.stderr.lower() for word in ('artifact', 'digest', 'compaction')), result.stderr
        assert stable_session(snapshot(case)[1]) == before
        case.complete()


def credential_input(binary, output):
    with Case(binary, output, 'credential-input', [{'text': 'FIRST_READY'}, {'text': 'SECRET_READY'}, {'text': 'TAIL_READY'}]) as case:
        with Interactive(case) as client:
            client.wait('FIRST_READY')
            wait_snapshot(case, client, lambda value: len(value.get('messages', [])) == 2)
            turn(case, client, 'Original request includes ' + case.env['OPENAI_API_KEY'], 'SECRET_READY', 4)
            before = turn(case, client, 'Retain this recent request.', 'TAIL_READY', 6)
            after = len(client.transcript)
            client.send('/compact')
            client.wait('Compaction failed', after=after)
            client.finish()
        assert stable_session(snapshot(case)[1]) == stable_session(before)
        assert not compactions(ledger(case))
        assert not any(event['kind'] == 'artifact.saved' and event['data']['name'] == 'context-compaction.json'
                       for event in ledger(case)['events'])
        case.complete()


def retained_call(binary, output):
    with Case(binary, output, 'retained-call', [{'text': 'FIRST_READY'},
              {'calls': [cap.edit('retained-operation.txt', 'RETAINED_OUTCOME_CONTENT')] + [('read_file', {'path': 'retained-operation.txt'}) for _ in range(39)]}, {'text': 'TAIL_READY'},
              summary('RETAINED_SUMMARY'), {'text': 'RETAINED_AFTER'}]) as case:
        with Interactive(case, flags=['--auto-approve']) as client:
            client.wait('FIRST_READY')
            wait_snapshot(case, client, lambda value: len(value.get('messages', [])) == 2)
            turn(case, client, 'Create the retained operation file. RETAINED_LONG_BEGIN ' + '界abc ' * 4200 + ' RETAINED_LONG_END', 'TAIL_READY', 4)
            before = ledger(case)
            call = before['tool_calls'][0]
            assert len(before['tool_calls']) == 40
            retained_original = snapshot(case)[1]['messages'][-2]['content']
            assert retained_original.endswith('[truncated]')
            assert call['result']['artifacts']
            client.send('/compact')
            client.wait('Compacted')
            wait_snapshot(case, client, lambda value: value.get('summary') == 'RETAINED_SUMMARY')
            client.send('Inspect retained execution evidence.')
            client.wait('RETAINED_AFTER')
            client.finish()
        wire = json.dumps(case.requests[-1], ensure_ascii=False)
        assert 'RETAINED_LONG_END' in wire and wire.count('RETAINED_LONG_BEGIN') == 1
        assert all(record['id'] in wire for record in before['tool_calls'])
        value = case.requests[-1]['input']
        blocks = [value] if isinstance(value, str) else [content['text'] for item in value
                  for content in item.get('content', []) if content.get('type') == 'input_text']
        entries = []
        for block in blocks:
            for line in block.splitlines():
                try:
                    entry = json.loads(line)
                except ValueError:
                    continue
                if isinstance(entry, dict) and entry.get('id') == call['id']:
                    entries.append(entry)
        assert len(entries) == 1, entries
        evidence = entries[0]
        assert evidence['name'] == call['name'] and evidence['status'] == call['status']
        assert json.loads(evidence['arguments']) == call['arguments']
        assert evidence['arguments_digest'] == call['parameter_hash']
        assert evidence['artifacts'] == call['result']['artifacts']
        assert evidence['output'] == call['result']['output']
        after = ledger(case)
        assert call['id'] not in (compactions(after)[0]['compacted_calls'] or [])
        assert before['tool_calls'] == after['tool_calls']
        for ref in call['result']['artifacts']:
            data = (case.state / 'tasks/artifacts' / ref['id']).read_bytes()
            assert hashlib.sha256(data).hexdigest() == ref['sha256'] and len(data) == ref['size']
        case.complete()


def kill_boundary(binary, output):
    lock = None
    def hold_snapshot(case, _request):
        nonlocal lock
        lock = (case.state / 'tasks/sessions/.save.lock').open('r+b')
        fcntl.flock(lock.fileno(), fcntl.LOCK_EX)
        return summary('KILLED_BEFORE_SNAPSHOT')
    with Case(binary, output, 'kill-before-snapshot', tool_steps() + [{'text': 'SECOND_READY'}, hold_snapshot,
              {'text': 'KILL_RESUMED'}]) as case:
        write(case.workspace / 'note.txt', 'kill boundary fixture\n')
        try:
            with Interactive(case) as client:
                before = seed(case, client)
                before_calls = ledger(case)['tool_calls']
                path, _ = snapshot(case)
                lock_path = path.parent / '.save.lock'
                lock_identity = lock_path.stat().st_ino
                client.send('/compact')
                case.wait_requests(4)
                deadline = time.monotonic() + 10
                saved = None
                while time.monotonic() < deadline:
                    with sqlite3.connect(case.state / 'tasks/tasks.db', timeout=2) as connection:
                        rows = connection.execute("SELECT record FROM events WHERE json_extract(CAST(record AS TEXT),'$.kind')='artifact.saved' AND json_extract(CAST(record AS TEXT),'$.data.name')='context-compaction.json'").fetchall()
                    if rows:
                        saved = json.loads(rows[-1][0])['data']
                        break
                    client.read(0.02)
                assert saved is not None, 'compaction artifact did not commit before snapshot lock'
                assert stable_session(snapshot(case)[1]) == stable_session(before)
                client.process.kill()
                wait_exit(client)
                assert client.process.returncode == -signal.SIGKILL
                assert lock_path.stat().st_ino == lock_identity
                fcntl.flock(lock.fileno(), fcntl.LOCK_UN)
                lock.close()
                lock = None
            artifact(case, saved)
            assert stable_session(snapshot(case)[1]) == stable_session(before)
            assert not compactions(ledger(case))
            count = len(case.requests)
            case.run(['resume', before['id'], '--prompt', 'Continue after process death.'], label='kill-resume')
            assert len(case.requests) == count + 1 and case.requests[-1].get('tools')
            assert 'KILLED_BEFORE_SNAPSHOT' not in json.dumps(case.requests[-1])
            assert ledger(case)['tool_calls'] == before_calls
            dump(case.directory / 'kill-boundary-evidence.json', {'signal': 'SIGKILL', 'artifact': saved,
                 'lock_inode_preserved': lock_identity, 'snapshot_unchanged_before_resume': True,
                 'resume_requests': len(case.requests) - count})
            case.complete()
        finally:
            if lock is not None:
                fcntl.flock(lock.fileno(), fcntl.LOCK_UN)
                lock.close()


def large_ledger(binary, output):
    calls = [('read_file', {'path': 'many-calls.txt'}) for _ in range(129)]
    with Case(binary, output, 'large-ledger', [{'calls': calls}, {'text': 'MANY_READY'}, {'text': 'TAIL_READY'},
              summary('MANY_SUMMARY'), {'text': 'MANY_AFTER'}]) as case:
        write(case.workspace / 'many-calls.txt', 'MANY_CALLS_CONTENT\n')
        with Interactive(case) as client:
            client.wait('MANY_READY')
            wait_snapshot(case, client, lambda value: len(value.get('messages', [])) == 2)
            before = ledger(case)
            original_calls = before['tool_calls']
            assert len(original_calls) == 129
            task_id = before['task']['id']
            with sqlite3.connect(case.state / 'tasks/tasks.db', timeout=10) as connection:
                cursor = connection.execute('SELECT MAX(seq) FROM events WHERE task_id=?', (task_id,)).fetchone()[0]
                rows = []
                for index in range(1001):
                    sequence = cursor + index + 1
                    event = {'schema_version': 1, 'id': 'padding' + str(index), 'sequence': sequence,
                             'task_id': task_id, 'kind': 'fixture.pagination',
                             'time': before['task']['updated_at'], 'data': {'index': index}}
                    rows.append((task_id, sequence, json.dumps(event)))
                connection.executemany('INSERT INTO events(task_id,seq,record) VALUES(?,?,?)', rows)
            turn(case, client, 'Retain a request beyond the first event page.', 'TAIL_READY', 4)
            client.send('/compact exercise large ledger')
            client.wait('Compacted')
            wait_snapshot(case, client, lambda value: value.get('summary') == 'MANY_SUMMARY')
            client.send('Continue after large ledger compaction.')
            client.wait('MANY_AFTER')
            client.finish()
        record = ledger(case)
        assert record['tool_calls'] == original_calls
        assert len(record['events']) > 1000
        event = compactions(record)[0]
        assert event['compacted_call_count'] == 129 and len(event['compacted_calls']) == 128
        assert len(json.dumps(event).encode()) < 65536
        _, payload = artifact(case, event['artifact'])
        expected = [call['id'] for call in original_calls]
        assert payload['compacted_calls'] == expected
        inspected = record['compactions']
        assert len(inspected) == 1 and inspected[0]['sequence'] > 1000
        assert inspected[0]['compacted_calls'] == expected and inspected[0]['tool_calls'] == original_calls
        assert all(call_id in json.dumps(case.requests[-1]) for call_id in expected)
        case.complete()


def busy_and_cancel(binary, output):
    with Case(binary, output, 'busy-command', [{'wait': True, 'text': 'BUSY_FINISHED'}]) as case:
        with Interactive(case) as client:
            case.wait_requests(1)
            client.send('/compact must remain idle only')
            client.wait('compact is available only while idle')
            assert len(case.requests) == 1 and not case.cancelled.is_set()
            case.release.set()
            client.wait('BUSY_FINISHED')
            client.finish()
        assert not compactions(ledger(case))
        case.complete()
    with Case(binary, output, 'cancel-summary', tool_steps() + [{'text': 'SECOND_READY'}, dict(summary('UNUSED'), wait=True)]) as case:
        write(case.workspace / 'note.txt', 'safe fixture\n')
        with Interactive(case) as client:
            before = seed(case, client)
            client.send('/compact')
            case.wait_requests(4)
            client.process.send_signal(signal.SIGINT)
            wait_exit(client)
        assert stable_session(snapshot(case)[1]) == stable_session(before)
        assert not compactions(ledger(case))
        case.complete()


def unresolved(binary, output):
    for kind in ('call', 'process', 'approval'):
        with Case(binary, output, 'unresolved-' + kind, tool_steps() + [{'text': 'SECOND_READY'}]) as case:
            write(case.workspace / 'note.txt', 'safe fixture\n')
            with Interactive(case) as client:
                before = seed(case, client)
                record = ledger(case)
                call = record['tool_calls'][0]
                if kind == 'call':
                    call['status'] = call['result']['status'] = 'unknown'
                    sql(case, 'UPDATE tool_calls SET status=?,record=? WHERE id=?', ('unknown', json.dumps(call), call['id']))
                elif kind == 'process':
                    value = {'id': 'fixture-process', 'task_id': call['task_id'], 'run_id': call['run_id'],
                             'tool_call_id': call['id'], 'owner_epoch': 'fixture', 'launch_hash': call['parameter_hash'],
                             'executable': 'fixture', 'directory': str(case.workspace), 'timeout_seconds': 30,
                             'state': 'unknown', 'effects': 'unknown', 'started_at': call['started_at']}
                    sql(case, 'INSERT INTO processes(id,task_id,run_id,call_id,state,effects,record) VALUES(?,?,?,?,?,?,?)',
                        (value['id'], call['task_id'], call['run_id'], call['id'], 'unknown', 'unknown', json.dumps(value)))
                else:
                    value = {'id': 'fixture-approval', 'task_id': call['task_id'], 'run_id': call['run_id'],
                             'tool_call_id': call['id'], 'operation': 'write', 'parameter_hash': call['parameter_hash'],
                             'decision': 'pending', 'scope': 'tool_call', 'created_at': call['planned_at']}
                    sql(case, 'INSERT INTO approvals(id,task_id,run_id,call_id,decision,record) VALUES(?,?,?,?,?,?)',
                        (value['id'], call['task_id'], call['run_id'], call['id'], 'pending', json.dumps(value)))
                after = len(client.transcript)
                client.send('/compact')
                client.wait('meldra task show', after=after)
                client.finish()
            assert stable_session(snapshot(case)[1]) == stable_session(before)
            record = ledger(case)
            assert not compactions(record)
            if kind == 'call':
                assert record['tool_calls'][0]['status'] == 'unknown'
            if kind == 'process':
                assert record['processes'][0]['effects'] == 'unknown'
            assert '1' in (case.directory / 'interactive-transcript.txt').read_text()
            case.complete()


def threshold(binary, output):
    for name, custom, native, percent, expected in [('disabled', 131072, 131072, 0, False),
                                                  ('custom-denominator', 24000, 131072, 50, True),
                                                  ('native-cap', 131072, 24000, 50, True)]:
        steps = [{'text': 'A' * 6500 + ' FIRST_READY'}, {'text': 'B' * 6500 + ' SECOND_READY'}]
        if expected:
            steps.append(summary('THRESHOLD_SUMMARY'))
        steps.append({'text': 'THRESHOLD_FINISHED'})
        with Case(binary, output, 'threshold-' + name, steps) as case:
            case.configure(max_custom_turn_input_bytes=custom, max_turn_context_bytes=native, compact_threshold_percent=percent)
            with Interactive(case) as client:
                client.wait('FIRST_READY')
                wait_snapshot(case, client, lambda value: len(value.get('messages', [])) == 2)
                turn(case, client, 'Second threshold request.', 'SECOND_READY', 4)
                client.send('Third threshold request.')
                client.wait('THRESHOLD_FINISHED')
                client.finish()
            events = compactions(ledger(case))
            assert bool(events) == expected, (name, events)
            if expected:
                assert events[0]['trigger'] == 'threshold' and events[0]['from_bytes'] >= min(custom, native) * percent // 100
                assert events[0]['to_bytes'] < events[0]['from_bytes']
                assert 'THRESHOLD_SUMMARY' in case.requests[-1]['instructions']
            case.complete()
    with Case(binary, output, 'threshold-no-foldable-history', [{'text': 'FIRST_READY'}, {'text': 'UNCHANGED_READY'}]) as case:
        case.configure(max_turn_context_bytes=1024, compact_threshold_percent=1)
        with Interactive(case) as client:
            client.wait('FIRST_READY')
            wait_snapshot(case, client, lambda value: len(value.get('messages', [])) == 2)
            client.send('Nothing older than the retained tail.')
            client.wait('UNCHANGED_READY')
            client.finish()
        assert not compactions(ledger(case))
        case.complete()


def config(binary, output):
    with Case(binary, output, 'config') as case:
        result = dict(line.split('=', 1) for line in case.run(['config', 'show']).stdout.splitlines() if '=' in line)
        assert result['compact_keep_messages'] == '2' and result['compact_threshold_percent'] == '0'
        assert result['max_turn_context_bytes'] == '4194304'
        for key, values in [('compact_keep_messages', [1, 65]), ('compact_threshold_percent', [-1, 101]),
                            ('max_turn_context_bytes', [0, 67108865])]:
            for value in values:
                original = (case.state / 'config.toml').read_text()
                case.configure(**{key: value})
                result = case.run(['config', 'show'], success=False, label=key + '-' + str(value))
                assert key in result.stderr, result.stderr
                (case.state / 'config.toml').write_text(original)
        assert not case.requests


def native_binary(output):
    directory = output / 'native-fixture'
    directory.mkdir()
    source = (ROOT / 'internal/app/agent.go').read_text()
    replacement, count = re.subn(r'func isCustomBaseURL\(baseURL string\) bool \{.*?\n\}',
                                'func isCustomBaseURL(baseURL string) bool { return false }', source, count=1, flags=re.S)
    assert count == 1
    replacement += '\nfunc init() { if os.Getenv("MELDRA_E2E_FIXTURE_PROBE") == "compaction-native" { fmt.Println("meldra-e2e-overlay:compaction-native"); os.Exit(0) } }\n'
    path = write(directory / 'agent.go', replacement)
    subprocess.run(['gofmt', '-w', str(path)], check=True)
    overlay = directory / 'overlay.json'
    dump(overlay, {'Replace': {str(ROOT / 'internal/app/agent.go'): str(path)}})
    fixture = directory / 'meldra-native'
    env = dict(os.environ, CGO_ENABLED='0', PWD=str(ROOT))
    build = subprocess.run(['go', 'build', '-overlay', str(overlay), '-o', str(fixture), '.'], cwd=ROOT, env=env, capture_output=True, text=True)
    write(directory / 'build.txt', build.stdout + build.stderr)
    assert build.returncode == 0, build.stderr
    with Case(fixture, directory, 'probe') as case:
        case.env['MELDRA_E2E_FIXTURE_PROBE'] = 'compaction-native'
        probe = case.run(['--version'])
        assert probe.stdout.strip() == 'meldra-e2e-overlay:compaction-native' and not probe.stderr
    dump(directory / 'provenance.json', {'binary_sha256': hashlib.sha256(fixture.read_bytes()).hexdigest(),
         'source_sha256': hashlib.sha256(source.encode()).hexdigest(), 'overlay': str(overlay),
         'boundary': 'Only custom endpoint classification is overridden; Responses transport and compaction execute unchanged against loopback.'})
    return fixture


def native(binary, output):
    fixture = native_binary(output)
    with Case(binary, output, 'native-production-probe') as case:
        case.env['MELDRA_E2E_FIXTURE_PROBE'] = 'compaction-native'
        probe = case.run(['--version'])
        assert probe.stdout.strip() != 'meldra-e2e-overlay:compaction-native'
    with Case(fixture, output, 'native-continuation', tool_steps() + [{'text': 'SECOND_READY'}, summary('NATIVE_SUMMARY'), {'text': 'NATIVE_AFTER'}, {'text': 'NATIVE_SECOND_AFTER'}]) as case:
        write(case.workspace / 'note.txt', 'native fixture\n')
        with Interactive(case) as client:
            seed(case, client)
            assert case.requests[1].get('previous_response_id') == 'resp_1'
            assert case.requests[2].get('previous_response_id') == 'resp_2'
            client.send('/compact')
            client.wait('Compacted')
            _, saved = wait_snapshot(case, client, lambda value: value.get('summary') == 'NATIVE_SUMMARY')
            assert not saved.get('previous_response_id')
            client.send('Continue natively.')
            client.wait('NATIVE_AFTER')
            wait_snapshot(case, client, lambda value: value.get('previous_response_id') == 'resp_5')
            assert not case.requests[-1].get('previous_response_id')
            client.send('Continue the active native chain.')
            client.wait('NATIVE_SECOND_AFTER')
            client.finish()
        assert case.requests[-1].get('previous_response_id') == 'resp_5'
        assert 'Compacted execution evidence' not in json.dumps(case.requests[-1]['input'])
        assert 'SECOND_ORIGINAL_REQUEST' not in json.dumps(case.requests[-1]['input'])
        assert 'NATIVE_SUMMARY' in case.requests[-1]['instructions']
        case.complete()
    with Case(fixture, output, 'native-threshold', [{'text': 'A' * 6500 + ' FIRST_READY'},
              {'text': 'B' * 6500 + ' SECOND_READY'}, summary('NATIVE_THRESHOLD_SUMMARY'), {'text': 'NATIVE_THRESHOLD_AFTER'}]) as case:
        case.configure(max_turn_context_bytes=24000, compact_threshold_percent=50)
        with Interactive(case) as client:
            client.wait('FIRST_READY')
            wait_snapshot(case, client, lambda value: len(value.get('messages', [])) == 2)
            turn(case, client, 'Second native request.', 'SECOND_READY', 4)
            assert case.requests[1].get('previous_response_id') == 'resp_1'
            client.send('Third native request.')
            client.wait('NATIVE_THRESHOLD_AFTER')
            client.finish()
        events = compactions(ledger(case))
        assert len(events) == 1 and events[0]['trigger'] == 'threshold' and events[0]['from_bytes'] >= 12000
        assert not case.requests[-1].get('previous_response_id')
        assert 'NATIVE_THRESHOLD_SUMMARY' in case.requests[-1]['instructions']
        case.complete()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--only', action='append')
    args = parser.parse_args()
    os.environ['MELDRA_DISABLE_UPDATE_CHECK'] = '1'
    binary, output = args.binary.resolve(), args.output.resolve()
    output.mkdir(parents=True, exist_ok=False)
    scenarios = {'manual-line': manual, 'manual-pty': lambda b, o: manual(b, o, True), 'folded-image': folded_image,
                 'folded-image-priority': folded_image_priority, 'odd-tail': odd_tail, 'steering': steering, 'compact-controls': compact_controls,
                 'validation': validation, 'persistence': persistence, 'corruption': corruption,
                 'credential-input': credential_input, 'retained-call': retained_call,
                 'kill-boundary': kill_boundary, 'large-ledger': large_ledger,
                 'busy-and-cancel': busy_and_cancel, 'unresolved': unresolved, 'threshold': threshold,
                 'config': config, 'native': native}
    if args.only:
        unknown = set(args.only) - scenarios.keys()
        if unknown:
            parser.error('unknown scenarios: ' + ', '.join(sorted(unknown)))
        scenarios = {name: scenarios[name] for name in args.only}
    sources = sorted([*ROOT.glob('internal/**/*.go'), ROOT / 'go.mod', ROOT / 'go.sum', Path(__file__).resolve(),
                      ROOT / 'scripts/capability-e2e.py', ROOT / 'scripts/images-e2e.py', ROOT / 'docs/context-compaction-failure-matrix.md'])
    report = {'passed': False, 'live_model': False, 'platform': platform.platform(),
              'binary_sha256': hashlib.sha256(binary.read_bytes()).hexdigest(),
              'source_sha256': {str(path.relative_to(ROOT)): hashlib.sha256(path.read_bytes()).hexdigest() for path in sources},
              'repeat_command': ['python3', str(Path(__file__).resolve()), '--binary', str(binary), '--output', '<fresh-output-directory>'],
              'scenarios': []}
    for name, scenario in scenarios.items():
        result, started = {'name': name, 'passed': False}, time.monotonic()
        try:
            scenario(binary, output)
            result['passed'] = True
        except Exception:
            result['error'] = traceback.format_exc()
        result['elapsed_seconds'] = round(time.monotonic() - started, 3)
        report['scenarios'].append(result)
        dump(output / 'report.json', report)
        print(('PASS ' if result['passed'] else 'FAIL ') + name, flush=True)
        if not result['passed']:
            print(result['error'], file=sys.stderr)
    report['passed'] = all(result['passed'] for result in report['scenarios'])
    dump(output / 'report.json', report)
    dump(output / 'checksums.json', {str(path.relative_to(output)): hashlib.sha256(path.read_bytes()).hexdigest()
         for path in sorted(output.rglob('*')) if path.is_file()})
    raise SystemExit(0 if report['passed'] else 1)


if __name__ == '__main__':
    main()
