"""Stand-ins for what nginx proxies to, for deploy/nginx/test/run.sh.

Ports 9001 (backend), 9004 (WebSocket) and 9005 (sidecar) answer every
request with what they received, as JSON, so a test can see exactly which
path and headers reached them. Port 8443 is the explorer: HTTPS with a
certificate for mempool.guide, answering with headers that must not reach a
browser on the swap site's origin. Port 9443 is the same explorer with a
certificate nobody trusts.
"""

import http.server
import json
import ssl
import sys
import threading


class Echo(http.server.BaseHTTPRequestHandler):
    def answer(self):
        length = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(length).decode() if length else ""
        out = json.dumps({
            "port": self.server.server_port,
            "method": self.command,
            "path": self.path,
            "headers": {k.lower(): v for k, v in self.headers.items()},
            "body": body,
        }).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(out)))
        self.end_headers()
        self.wfile.write(out)

    do_GET = do_POST = do_PUT = do_DELETE = do_PATCH = answer

    def log_message(self, *args):
        pass


class Explorer(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        out = b"<html><script>alert(document.domain)</script></html>"
        self.send_response(200)
        self.send_header("Content-Type", "text/html")
        self.send_header("Set-Cookie", "session=stolen; Path=/")
        self.send_header("Service-Worker-Allowed", "/")
        self.send_header("Content-Security-Policy", "script-src *")
        self.send_header("Access-Control-Allow-Origin", "*")
        self.send_header("X-Explorer-Path", self.path)
        self.send_header("X-Seen-Cookie", self.headers.get("Cookie", ""))
        self.send_header("X-Seen-Forwarded", self.headers.get(
            "X-Forwarded-For", ""))
        self.send_header("Content-Length", str(len(out)))
        self.end_headers()
        self.wfile.write(out)

    do_POST = do_GET

    def log_message(self, *args):
        pass


def serve(port, handler, cert=None):
    server = http.server.ThreadingHTTPServer(("0.0.0.0", port), handler)
    if cert:
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.load_cert_chain(*cert)
        server.socket = context.wrap_socket(server.socket, server_side=True)
    threading.Thread(target=server.serve_forever, daemon=True).start()


if __name__ == "__main__":
    certs = sys.argv[1]
    for port in (9001, 9004, 9005):
        serve(port, Echo)
    serve(8443, Explorer, (f"{certs}/explorer.crt", f"{certs}/explorer.key"))
    serve(9443, Explorer, (f"{certs}/untrusted.crt", f"{certs}/untrusted.key"))
    print("mock ready", flush=True)
    threading.Event().wait()
