"""Disposable local workshop app. No external services or persistent writes."""
import argparse
import http.server
import json
from pathlib import Path
import threading

ROOT = Path(__file__).resolve().parent


class Handler(http.server.BaseHTTPRequestHandler):
    def reply(self, status, body, content_type='application/json'):
        data = body if isinstance(body, bytes) else json.dumps(body).encode()
        self.send_response(status)
        self.send_header('Content-Type', content_type)
        self.send_header('Cache-Control', 'no-store')
        self.send_header('Content-Length', str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        files = {'/': ('index.html', 'text/html'), '/app.js': ('app.js', 'text/javascript'), '/style.css': ('style.css', 'text/css')}
        path = self.path.split('?', 1)[0]
        if path not in files:
            self.reply(404, {'error': 'Not found'})
            return
        name, mime = files[path]
        self.reply(200, (ROOT / name).read_bytes(), mime)

    def do_POST(self):
        if self.path == '/__dev/stop':
            self.reply(200, {'stopping': True})
            threading.Thread(target=self.server.shutdown, daemon=True).start()
            return
        if self.path != '/api/reservations':
            self.reply(404, {'error': 'Unknown API route'})
            return
        length = int(self.headers.get('Content-Length', '0'))
        if not 0 < length <= 4096:
            self.reply(400, {'error': 'Invalid body size'})
            return
        try:
            data = json.loads(self.rfile.read(length))
        except (ValueError, UnicodeDecodeError):
            self.reply(400, {'error': 'Invalid JSON'})
            return
        prices = {'ceramics': 48, 'printmaking': 65}
        if not isinstance(data, dict):
            self.reply(400, {'error': 'Expected a reservation object'})
            return
        workshop, seats = data.get('workshop'), data.get('seats')
        name, email = data.get('name'), data.get('email')
        if (not isinstance(workshop, str) or workshop not in prices or type(seats) is not int or not 1 <= seats <= 4
                or not isinstance(name, str) or not name.strip() or not isinstance(email, str) or '@' not in email):
            self.reply(400, {'error': 'Check your workshop, seats, name and email'})
            return
        self.reply(201, {'reference': 'FW-' + workshop[:3].upper() + '-' + str(seats), 'workshop': workshop, 'seats': seats, 'total': prices[workshop] * seats})


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--port', type=int, default=0)
    args = parser.parse_args()
    server = http.server.ThreadingHTTPServer(('127.0.0.1', args.port), Handler)
    print(f'READY http://127.0.0.1:{server.server_port}', flush=True)
    try:
        server.serve_forever()
    finally:
        server.server_close()
        print('STOPPED cleanly', flush=True)
