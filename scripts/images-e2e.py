#!/usr/bin/env python3
"""Real-binary image references: failure cases written before implementation."""
import argparse
import base64
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import struct
import sys
import time
import traceback
import zlib

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location('capability_e2e', ROOT / 'scripts/capability-e2e.py')
cap = importlib.util.module_from_spec(spec)
spec.loader.exec_module(cap)
write, dump, Interactive = cap.write, cap.dump, cap.Interactive


class Case(cap.Case):
    def __init__(self, *args, **kwargs):
        super().__init__(*args, **kwargs)
        self.env['MELDRA_DISABLE_UPDATE_CHECK'] = '1'


def png(rgb=(200, 10, 30), width=1, height=1):
    def chunk(kind, data):
        return struct.pack('>I', len(data)) + kind + data + struct.pack('>I', zlib.crc32(kind + data))
    header = struct.pack('>IIBBBBB', width, height, 8, 2, 0, 0, 0)
    return b'\x89PNG\r\n\x1a\n' + chunk(b'IHDR', header) + chunk(b'IDAT', zlib.compress(b'\0' + bytes(rgb))) + chunk(b'IEND', b'')


def images(request):
    value = request.get('input', [])
    if isinstance(value, str):
        return []
    return [content['image_url'] for item in value if item.get('role') == 'user'
            for content in item.get('content', []) if isinstance(content, dict) and content.get('type') == 'input_image']


def assert_image(request, data):
    expected = 'data:image/png;base64,' + base64.b64encode(data).decode()
    assert expected in images(request), (images(request), expected)


def audit_images(case, data):
    records = case.audit()
    encoded = base64.b64encode(data).decode()
    for path in (case.state / 'tasks/sessions').glob('*.json'):
        raw = path.read_text()
        assert encoded not in raw and 'data:image/' not in raw
        session = json.loads(raw)
        refs = [ref for msg in session['messages'] for ref in msg.get('references', []) if ref.get('mime_type')]
        assert any(ref['mime_type'] == 'image/png' and ref['digest'] == hashlib.sha256(data).hexdigest() for ref in refs), refs
    assert encoded not in json.dumps(records)
    digest = hashlib.sha256(data).hexdigest()
    assert (case.state / 'tasks/artifacts' / digest).read_bytes() == data
    return records


def submit_resume(binary, output):
    data = png()
    with Case(binary, output, 'submit-resume', [{'calls': [('read_file', {'path': 'note.txt'})]}, {}, {}]) as case:
        write(case.workspace / 'shot.png', data)
        write(case.workspace / 'note.txt', 'TEXT_ATTACHMENT_SENTINEL\n')
        case.chat('Compare @shot.png and @note.txt', flags=['--mode', 'plan'])
        assert_image(case.requests[0], data)
        assert_image(case.requests[1], data)
        assert 'TEXT_ATTACHMENT_SENTINEL' in json.dumps(case.requests[0])
        assert 'function_call_output' in json.dumps(case.requests[1])
        records = audit_images(case, data)
        task = records[0]['task']['id']
        (case.workspace / 'shot.png').unlink()
        case.run(['resume', task, '--prompt', 'Describe the saved image again.'], label='resume')
        assert_image(case.requests[-1], data)
        audit_images(case, data)
        case.complete()


def formats(binary, output):
    jpeg = base64.b64decode('/9j/2wCEAAgGBgcGBQgHBwcJCQgKDBQNDAsLDBkSEw8UHRofHh0aHBwgJC4nICIsIxwcKDcpLDAxNDQ0Hyc5PTgyPC4zNDIBCQkJDAsMGA0NGDIhHCEyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMv/AABEIAAEAAQMBIgACEQEDEQH/xAGiAAABBQEBAQEBAQAAAAAAAAAAAQIDBAUGBwgJCgsQAAIBAwMCBAMFBQQEAAABfQECAwAEEQUSITFBBhNRYQcicRQygZGhCCNCscEVUtHwJDNicoIJChYXGBkaJSYnKCkqNDU2Nzg5OkNERUZHSElKU1RVVldYWVpjZGVmZ2hpanN0dXZ3eHl6g4SFhoeIiYqSk5SVlpeYmZqio6Slpqeoqaqys7S1tre4ubrCw8TFxsfIycrS09TV1tfY2drh4uPk5ebn6Onq8fLz9PX29/j5+gEAAwEBAQEBAQEBAQAAAAAAAAECAwQFBgcICQoLEQACAQIEBAMEBwUEBAABAncAAQIDEQQFITEGEkFRB2FxEyIygQgUQpGhscEJIzNS8BVictEKFiQ04SXxFxgZGiYnKCkqNTY3ODk6Q0RFRkdISUpTVFVWV1hZWmNkZWZnaGlqc3R1dnd4eXqCg4SFhoeIiYqSk5SVlpeYmZqio6Slpqeoqaqys7S1tre4ubrCw8TFxsfIycrS09TV1tfY2dri4+Tl5ufo6ery8/T19vf4+fr/2gAMAwEAAhEDEQA/AOLooor5k/cT/9k=')
    with Case(binary, output, 'formats', [{}]) as case:
        write(case.workspace / 'photo.jpg', jpeg)
        write(case.workspace / 'image-without-extension', png())
        case.chat('Inspect @photo.jpg and @image-without-extension')
        assert 'data:image/jpeg;base64,' + base64.b64encode(jpeg).decode() in images(case.requests[0])
        assert_image(case.requests[0], png())
        audit_images(case, png())
        case.complete()


def rejected(binary, output):
    cases = {
        'range': ('shot.png', png(), '@shot.png:1'),
        'corrupt': ('shot.png', b'\x89PNG\r\n\x1a\nnot-a-png', '@shot.png'),
        'bad-crc': ('shot.png', png()[:-8] + b'BROKEN!!', '@shot.png'),
        'oversize': ('shot.png', png() + b'x' * (1 << 20), '@shot.png'),
        'dimension': ('shot.png', png(width=100000, height=100000), '@shot.png'),
        'truncated-jpeg': ('shot.jpg', b'\xff\xd8\xffbroken', '@shot.jpg'),
        'unsupported': ('shot.svg', b'<svg xmlns="http://www.w3.org/2000/svg"></svg>', '@shot.svg'),
        'traversal': ('shot.png', png(), '@../outside.png'),
    }
    for label, (name, data, prompt) in cases.items():
        with Case(binary, output, 'reject-' + label) as case:
            write(case.workspace / name, data)
            write(case.directory / 'outside.png', png())
            case.chat(prompt, success=False)
            assert not case.requests
            case.audit()
    for kind in ('symlink', 'hardlink'):
        with Case(binary, output, 'reject-' + kind) as case:
            source = write(case.directory / 'outside.png', png())
            if kind == 'symlink':
                (case.workspace / 'shot.png').symlink_to(source)
            else:
                os.link(source, case.workspace / 'shot.png')
            case.chat('@shot.png', success=False)
            assert not case.requests
    with Case(binary, output, 'reject-protected') as case:
        private = case.workspace / 'private-state'
        private.mkdir(mode=0o700)
        write(private / 'config.toml', (case.state / 'config.toml').read_bytes()).chmod(0o600)
        write(private / 'shot.png', png())
        case.env['MELDRA_HOME'] = str(private)
        case.chat('@private-state/shot.png', success=False)
        assert not case.requests
    with Case(binary, output, 'reject-total') as case:
        data = png() + b'x' * ((1 << 20) - len(png()))
        for name in ('a.png', 'b.png', 'c.png'):
            write(case.workspace / name, data)
        case.chat('@a.png @b.png @c.png', success=False)
        assert not case.requests


def controls(binary, output):
    for kind in ('queue', 'steer'):
        original, changed = png(), png((2, 3, 4))
        with Case(binary, output, 'control-' + kind, [{'stream_wait': True}, {}]) as case:
            write(case.workspace / 'shot.png', original)
            with Interactive(case) as client:
                client.wait('CAPABILITY_PARTIAL_STREAM')
                client.send('/' + kind + ' Inspect @shot.png')
                client.wait('Queued' if kind == 'queue' else 'Steering received')
                write(case.workspace / 'shot.png', changed)
                if kind == 'queue':
                    case.release.set()
                case.wait_requests(2)
                client.wait('CAPABILITY_COMPLETE')
                assert_image(case.requests[-1], original)
                assert 'data:image/png;base64,' + base64.b64encode(changed).decode() not in images(case.requests[-1])
                client.finish()
            audit_images(case, original)
            case.complete()


def queue_recovery(binary, output):
    data = png()
    with Case(binary, output, 'queue-recovery', [{'stream_wait': True}, {}, {}]) as case:
        write(case.workspace / 'shot.png', data)
        with Interactive(case) as client:
            client.wait('CAPABILITY_PARTIAL_STREAM')
            client.send('/queue Inspect @shot.png')
            client.wait('Queued')
            task = case.audit()[0]['task']['id']
            client.process.kill()
            client.process.wait(timeout=5)
        write(case.workspace / 'shot.png', png((3, 4, 5)))
        case.run(['task', 'resume', task, '--prompt', 'Inspect only.'], label='inspect-resume')
        assert not images(case.requests[-1]), 'ordinary resume executed queued image input'
        case.run(['task', 'resume', task, '--prompt', '/continue-queue'], label='continue-queue')
        assert_image(case.requests[-1], data)
        audit_images(case, data)
        case.complete()


def history_budget(binary, output):
    with Case(binary, output, 'history-budget', [{}, {}, {}]) as case:
        blobs = []
        for index, color in enumerate([(1, 2, 3), (4, 5, 6), (7, 8, 9)]):
            data = png(color)
            data += b'x' * ((1 << 20) - len(data))
            blobs.append(data)
            write(case.workspace / ('shot' + str(index) + '.png'), data)
        case.chat('@shot0.png')
        task = audit_images(case, blobs[0])[0]['task']['id']
        for index in (1, 2):
            case.run(['resume', task, '--prompt', '@shot' + str(index) + '.png'], label='resume-' + str(index))
        assert len(images(case.requests[-1])) == 2
        assert_image(case.requests[-1], blobs[2])
        assert_image(case.requests[-1], blobs[1])
        assert 'older image snapshots omitted' in json.dumps(case.requests[-1])
        case.complete()


def corrupt_artifact(binary, output):
    for kind in ('missing', 'corrupt'):
        data = png()
        with Case(binary, output, 'artifact-' + kind, [{}]) as case:
            write(case.workspace / 'shot.png', data)
            case.chat('Inspect @shot.png')
            records = audit_images(case, data)
            artifact = case.state / 'tasks/artifacts' / hashlib.sha256(data).hexdigest()
            if kind == 'missing':
                artifact.unlink()
            else:
                artifact.write_bytes(b'x' * len(data))
            case.run(['resume', records[0]['task']['id'], '--prompt', 'Continue.'], success=False, label='resume')
            assert len(case.requests) == 1
            case.complete()


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--binary', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--only', action='append')
    args = parser.parse_args()
    binary, output = args.binary.resolve(), args.output.resolve()
    output.mkdir(parents=True, exist_ok=False)
    scenarios = dict(submit_resume=submit_resume, formats=formats, rejected=rejected, controls=controls, queue_recovery=queue_recovery, history_budget=history_budget, corrupt_artifact=corrupt_artifact)
    if args.only:
        scenarios = {name: scenarios[name] for name in args.only}
    sources = [*ROOT.glob('internal/**/*.go'), ROOT / 'go.mod', ROOT / 'go.sum', Path(__file__).resolve(), ROOT / 'docs/images-failure-matrix.md']
    report = {'passed': False, 'live_model': False, 'binary_sha256': hashlib.sha256(binary.read_bytes()).hexdigest(),
              'source_sha256': {str(path.relative_to(ROOT)): hashlib.sha256(path.read_bytes()).hexdigest() for path in sorted(sources)}, 'scenarios': []}
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
    report['passed'] = all(check['passed'] for check in report['scenarios'])
    dump(output / 'report.json', report)
    dump(output / 'checksums.json', {str(path.relative_to(output)): hashlib.sha256(path.read_bytes()).hexdigest() for path in sorted(output.rglob('*')) if path.is_file()})
    raise SystemExit(0 if report['passed'] else 1)


if __name__ == '__main__':
    main()
