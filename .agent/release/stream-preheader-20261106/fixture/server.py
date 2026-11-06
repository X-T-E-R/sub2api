#!/usr/local/bin/python
"""Health-only fixture. It has no provider, credential or application database."""
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import signal
import sys


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path != '/health':
            self.send_error(404)
            return
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b'{"status":"ok"}')

    def log_message(self, *_args):
        pass


signal.signal(signal.SIGTERM, lambda *_args: sys.exit(0))
ThreadingHTTPServer(('0.0.0.0', 8080), Handler).serve_forever()
