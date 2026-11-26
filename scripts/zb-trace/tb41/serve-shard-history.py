"""Serve the third original's archived dependency payloads on its private bridge."""
from hashlib import sha256
from http.server import BaseHTTPRequestHandler, HTTPServer
import json
from pathlib import Path
import ssl
import sys

GATEWAY = '10.251.200.1'
GUTENBERG = 'www.gutenberg.org'
GITHUB = 'raw.githubusercontent.com'
S3 = 's3.amazonaws.com'
ROUTES = {
    (GUTENBERG, '/cache/epub/1342/pg1342.txt'): (
        'pride.txt', 'b42030bac8a2bd81bb8141e3499ce2e867ccd141aeeabc41fb068d92a6853ad7',
        'text/plain; charset=utf-8'),
    (GITHUB, '/pytorch/examples/main/word_language_model/data/wikitext-2/train.txt'): (
        'wiki.train.tokens', '9e9fa1ad55b1c2c95b08e37dd8e653f638fac2c6de904b79e813611eefbc985f',
        'text/plain; charset=utf-8'),
    (GITHUB, '/karpathy/char-rnn/master/data/tinyshakespeare/input.txt'): (
        'tinyshakespeare.txt', '86c4e6aa9db7c042ec79f339dcb96d42b0075e16b8fc2e86bf0ca57e2dc565ed',
        'text/plain; charset=utf-8'),
    (S3, '/research.metamind.io/wikitext/wikitext-2-raw-v1.zip'): (
        'wikitext-2-raw.zip', 'd06ba1cb1536b93f79fe24f185e9b02b4bd76f6a459f21bd1bf3134c6f07721e',
        'application/xml'),
}


def serve(root, certificate, key):
    payloads = {}
    for route, (name, expected_hash, media_type) in ROUTES.items():
        body = (root / name).read_bytes()
        assert sha256(body).hexdigest() == expected_hash, name
        payloads[route] = (body, media_type)

    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):
            host = self.headers.get('Host')
            route = (host, self.path)
            if route == (GUTENBERG, '/ebooks/1342.txt.utf-8'):
                self.send_response(302)
                self.send_header('Location', '/cache/epub/1342/pg1342.txt')
                self.send_header('Content-Length', '0')
                self.end_headers()
                return
            if self.path == '/healthz' and host in (GUTENBERG, GITHUB, S3):
                body, media_type = b'shard-history', 'text/plain'
            elif route in payloads:
                body, media_type = payloads[route]
            else:
                self.send_error(404)
                return
            self.send_response(200)
            self.send_header('Content-Type', media_type)
            self.send_header('Content-Length', str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            print(json.dumps(dict(host=host, path=self.path, bytes=len(body),
                                  sha256=sha256(body).hexdigest())), flush=True)

    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    context.load_cert_chain(certificate, key)
    with HTTPServer((GATEWAY, 443), Handler) as server:
        server.socket = context.wrap_socket(server.socket, server_side=True)
        server.serve_forever()


if __name__ == '__main__':
    payloads, certificate, key = sys.argv[1:]
    serve(Path(payloads), certificate, key)
