#!/usr/bin/env python3
"""Real CLI Responses compatibility checks with offline wire evidence.

Failure cases declared before changes: unsupported streaming must fall back only
before output; JSON responses to stream requests must work; custom continuation
must replay call IDs/results without previous_response_id; empty identities must
not dispatch; partial stream errors must not retry or lose visible text.
"""
import argparse
import http.server
import importlib.util
import json
from pathlib import Path
import threading
import traceback

spec = importlib.util.spec_from_file_location('capability', Path(__file__).with_name('capability-e2e.py'))
cap = importlib.util.module_from_spec(spec)
spec.loader.exec_module(cap)


def scenario(binary, output, mode):
    with cap.Case(binary, output, mode) as case:
        case.server.shutdown()
        case.server.server_close()
        class Handler(http.server.BaseHTTPRequestHandler):
            def log_message(self, *_):
                pass

            def do_POST(self):
                try:
                    assert self.path == '/v1/responses', self.path
                    request = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
                    case.requests.append(request)
                    index = len(case.requests)
                    assert not request.get('previous_response_id'), request
                    if mode == 'fallback' and index == 1:
                        assert request['stream'] is True
                        body = cap.text({'error': {'message': 'stream is not supported', 'type': 'invalid_request_error', 'param': 'stream', 'code': 'unsupported'}}).encode()
                        self.send_response(400)
                    elif mode == 'partial-error':
                        self.send_response(200)
                        self.send_header('Content-Type', 'text/event-stream')
                        self.end_headers()
                        events = [{'type': 'response.output_text.delta', 'item_id': 'msg', 'output_index': 0, 'content_index': 0, 'delta': 'PARTIAL_VISIBLE'},
                                  {'type': 'error', 'code': 'stream_unsupported', 'message': 'stream is not supported'}]
                        for event in events:
                            self.wfile.write(('event: ' + event['type'] + '\ndata: ' + cap.text(event) + '\n\n').encode())
                        self.wfile.flush()
                        return
                    else:
                        if mode == 'fallback':
                            assert not request.get('stream'), request
                        turn = index - (1 if mode == 'fallback' else 0)
                        if turn == 1:
                            items = [{'type': 'function_call', 'id': 'fc_read', 'call_id': 'call_read', 'name': 'read_file',
                                      'arguments': cap.text({'path': 'fixture.txt'}), 'status': 'completed'}]
                        else:
                            inputs = request['input']
                            assert any(item.get('type') == 'function_call' and item.get('call_id') == 'call_read' for item in inputs), inputs
                            assert any(item.get('type') == 'function_call_output' and item.get('call_id') == 'call_read' and 'PROVIDER_FIXTURE' in item['output'] for item in inputs), inputs
                            assert 'Read the fixture' in cap.text(inputs), inputs
                            items = [{'type': 'message', 'id': 'msg', 'role': 'assistant', 'status': 'completed',
                                      'content': [{'type': 'output_text', 'text': 'COMPAT_COMPLETE', 'annotations': []}]}]
                        response = {'id': '' if mode == 'missing-id' else 'resp_' + str(turn), 'object': 'response', 'status': 'completed', 'output': items}
                        body = cap.text(response).encode()
                        self.send_response(200)
                    self.send_header('Content-Type', 'application/json')
                    self.send_header('Content-Length', str(len(body)))
                    self.end_headers()
                    self.wfile.write(body)
                except Exception:
                    case.errors.append(traceback.format_exc())
                    self.send_error(500)

        case.server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        threading.Thread(target=case.server.serve_forever, daemon=True).start()
        cap.write(case.state / 'config.toml', f'model="compat-fixture"\nbase_url="http://127.0.0.1:{case.server.server_port}/v1"\nallow_insecure_base_url=true\n').chmod(0o600)
        cap.write(case.workspace / 'fixture.txt', 'PROVIDER_FIXTURE\n')
        result = case.chat('Read the fixture', flags=['--mode', 'plan'], success=mode not in ('partial-error', 'missing-id'))
        if mode == 'partial-error':
            assert len(case.requests) == 1, case.requests
            assert 'PARTIAL_VISIBLE' in result.stdout, result.stdout
            snapshots = list((case.state / 'tasks/sessions').glob('*.json'))
            assert 'PARTIAL_VISIBLE' in snapshots[0].read_text()
        elif mode == 'missing-id':
            assert len(case.requests) == 1
            assert not case.audit()[0].get('tool_calls'), 'missing response identity dispatched a tool'
        else:
            assert 'COMPAT_COMPLETE' in result.stdout
            assert len(case.requests) == (3 if mode == 'fallback' else 2)
            case.audit()
        assert not case.errors, case.errors


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    args.output.mkdir(parents=True, exist_ok=False)
    report = {'passed': False, 'scenarios': []}
    for mode in ('json-stream-response', 'fallback', 'partial-error', 'missing-id'):
        result = {'name': mode, 'passed': False}
        try:
            scenario(args.binary.resolve(), args.output.resolve(), mode)
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
