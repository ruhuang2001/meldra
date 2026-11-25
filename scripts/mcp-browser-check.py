#!/usr/bin/env python3
"""Opt-in real Chrome MCP screenshot check; no browser or account in default CI.

The offline provider drives navigation and capture deterministically. With
--live-config, its final request goes to that configured Responses model, with
only the synthetic page and screenshot, never repository content. Credentials
are read in memory and are not copied into the evidence directory.
"""
import argparse
import base64
import hashlib
import http.server
import importlib.util
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import threading
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location('capability_e2e', ROOT / 'scripts/capability-e2e.py')
cap = importlib.util.module_from_spec(spec)
spec.loader.exec_module(cap)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', type=Path, required=True)
    parser.add_argument('--server-script', type=Path, required=True, help='Installed Chrome DevTools MCP entrypoint')
    parser.add_argument('--node', default=shutil.which('node'))
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--expect-text-only', action='store_true', help='Baseline before MCP image support')
    parser.add_argument('--live-config', type=Path, help='Opt in to paid inference using this private Meldra home')
    args = parser.parse_args()
    if args.expect_text_only and args.live_config:
        parser.error('Do not send base64-as-text baseline to a paid model')
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=False)
    challenge = secrets.token_hex(3).upper()
    svg = f'''<svg xmlns="http://www.w3.org/2000/svg" width="880" height="500">
      <rect width="880" height="500" fill="#f5f1e8"/>
      <text x="50" y="70" font-size="25" font-family="sans-serif" fill="#242424">Meldra visual evidence</text>
      <polygon points="130,140 55,270 205,270" fill="#d12f38"/>
      <circle cx="430" cy="210" r="66" fill="#246bd6"/>
      <rect x="665" y="145" width="130" height="130" fill="#289352"/>
      <text x="50" y="380" font-size="30" font-family="sans-serif" fill="#242424">Capture code</text>
      <text x="350" y="385" font-size="52" font-family="monospace" fill="#242424">{challenge}</text>
    </svg>'''.encode()
    page = b'<!doctype html><html><head><title>Visual fixture</title></head><body style="margin:0"><img src="/board.svg" alt="Visual test board" width="880" height="500"></body></html>'

    class Page(http.server.BaseHTTPRequestHandler):
        def log_message(self, *_args):
            pass

        def do_GET(self):
            body, mime = (svg, 'image/svg+xml') if self.path == '/board.svg' else (page, 'text/html')
            self.send_response(200)
            self.send_header('Content-Type', mime)
            self.send_header('Content-Length', str(len(body)))
            self.end_headers()
            self.wfile.write(body)

    pages = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Page)
    thread = threading.Thread(target=pages.serve_forever, daemon=True)
    thread.start()
    report = {'passed': False, 'live_model': bool(args.live_config), 'challenge': challenge,
              'binary_sha256': hashlib.sha256(args.binary.read_bytes()).hexdigest(),
              'server_script': str(args.server_script.resolve()), 'baseline': args.expect_text_only}

    def latest_result(request):
        return [item for item in request['input'] if item.get('type') == 'function_call_output'][-1]

    def screenshot(_case, request):
        result = latest_result(request)
        match = re.search(r'(\d+): Visual fixture \(http://127\.0\.0\.1:', result['output'])
        assert match, ('new_page did not return a page ID', result)
        return {'calls': [('mcp__chrome__take_screenshot', {'pageId': int(match[1]), 'format': 'png'})]}

    def inspect(case, request):
        result = latest_result(request)
        parts = result['output']
        if args.expect_text_only:
            assert isinstance(parts, str)
            raw = json.loads(parts)
            encoded = next(part['data'] for part in raw['content'] if part['type'] == 'image')
            report['image_blocks'] = 0
        else:
            assert isinstance(parts, list), 'screenshot is still JSON text, not model-visible image content'
            images = [part for part in parts if part['type'] == 'input_image']
            assert len(images) == 1, images
            assert images[0]['image_url'].startswith('data:image/png;base64,')
            encoded = images[0]['image_url'].split(',', 1)[1]
            report['image_blocks'] = len(images)
        pixels = base64.b64decode(encoded, validate=True)
        assert pixels.startswith(b'\x89PNG\r\n\x1a\n')
        digest = hashlib.sha256(pixels).hexdigest()
        if not args.expect_text_only:
            assert (case.state / 'tasks/artifacts' / digest).read_bytes() == pixels
        cap.write(output / 'screenshot.png', pixels)
        report.update(screenshot_sha256=digest, screenshot_bytes=len(pixels), call_id=result['call_id'])
        if args.live_config:
            import tomllib

            config = tomllib.loads((args.live_config / 'config.toml').read_text())
            key = os.environ.get('OPENAI_API_KEY', '')
            credentials = args.live_config / 'credentials.env'
            if not key and credentials.is_file():
                for line in credentials.read_text().splitlines():
                    if line.startswith('OPENAI_API_KEY='):
                        key = line.partition('=')[2].strip().strip('"').strip("'")
            assert key, 'missing configured model key'
            endpoint = config.get('base_url', 'https://api.openai.com/v1').rstrip('/') + '/responses'
            assert endpoint.startswith('https://'), 'live endpoint must use HTTPS'
            payload = dict(request, model=config['model'], stream=False, tools=[], max_output_tokens=512)
            payload.pop('previous_response_id', None)
            remote = urllib.request.Request(endpoint, data=json.dumps(payload).encode(),
                headers={'Authorization': 'Bearer ' + key, 'Content-Type': 'application/json'})
            class NoRedirect(urllib.request.HTTPRedirectHandler):
                def redirect_request(self, req, fp, code, msg, headers, newurl):
                    return None

            with urllib.request.build_opener(NoRedirect).open(remote, timeout=120) as response:
                reply = json.load(response)
            answer = '\n'.join(part.get('text', '') for item in reply.get('output', [])
                               for part in item.get('content', []) if part.get('type') == 'output_text')
            report.update(model=config['model'], answer=answer)
            cap.write(output / 'model-answer.txt', answer + '\n')
            assert challenge in answer.upper(), ('model did not read the fresh visual code', answer)
            assert all(word in answer.lower() for word in ('red', 'triangle', 'blue', 'green', 'square')), answer
            assert 'circle' in answer.lower() or 'disk' in answer.lower(), answer
            return {'text': answer}
        return {'text': 'Screenshot transport verified.'}

    try:
        with cap.Case(args.binary.resolve(), output, 'browser', [
            {'calls': [('mcp__chrome__new_page', {'url': f'http://127.0.0.1:{pages.server_port}/'})]}, screenshot, inspect
        ]) as case:
            case.env['MELDRA_DISABLE_UPDATE_CHECK'] = '1'
            cap.dump(case.state / 'mcp.json', {'mcpServers': {'chrome': {
                'command': str(Path(args.node).resolve()),
                'args': [str(args.server_script.resolve()), '--headless', '--isolated', '--no-usage-statistics',
                         '--no-performance-crux', '--viewport=880x500', '--screenshot-format=png',
                         '--no-file-navigations', '--no-javascript-evaluation'],
                'startup_timeout_sec': 60, 'tool_timeout_sec': 60
            }}})
            (case.state / 'mcp.json').chmod(0o600)
            case.chat('Inspect the browser screenshot. State the shapes and colors from left to right, then transcribe the capture code. Answer in English. If no image is visible, say so; do not guess.',
                      flags=['--auto-approve', '--output-format', 'stream-json'], timeout=180)
            records = case.audit()
            calls = records[0]['tools'] if 'tools' in records[0] else records[0].get('tool_calls', [])
            assert len(calls) == 2, records[0].keys()
            assert all(call['status'] == 'succeeded' for call in calls), calls
            if not args.expect_text_only:
                assert encoded_not_in_records(records)
            case.complete()
            report['passed'] = True
    finally:
        pages.shutdown()
        pages.server_close()
        thread.join()
        cap.dump(output / 'report.json', report)
    print(json.dumps(report, indent=2))


def encoded_not_in_records(records):
    return 'data:image/' not in json.dumps(records) and 'iVBORw0KGgo' not in json.dumps(records)


if __name__ == '__main__':
    main()
