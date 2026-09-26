#!/usr/bin/env python3
"""Test-only download mirror for scripts/test-install-lifecycle.sh.

Runs INSIDE the LXD test container, never on a real host. The harness fills both
directories with byte-identical copies of the staging files and then tampers with one
copy of the binary.

  https://127.0.0.1:8443/redir/<p> -> 302 to http://localhost:8080/<p> (cleartext)
  https://127.0.0.1:8443/<p>       -> /root/mirror-bad/<p> (the binary is tampered)
  http://127.0.0.1:8080/<p>        -> /root/mirror/<p>     (the genuine files)

The certificate is self-signed for "localhost"; install.sh trusts it only through the
CURL_CA_BUNDLE the harness sets on that one run.
"""
import functools
import http.server
import ssl
import threading


class RedirectOrServe(http.server.SimpleHTTPRequestHandler):
    def do_GET(self):
        if self.path.startswith("/redir/"):
            self.send_response(302)
            self.send_header("Location", "http://localhost:8080/" + self.path[len("/redir/"):])
            self.end_headers()
            return
        super().do_GET()


def serve(port, handler, tls):
    httpd = http.server.ThreadingHTTPServer(("127.0.0.1", port), handler)
    if tls:
        ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        ctx.load_cert_chain("/root/tls/cert.pem", "/root/tls/key.pem")
        httpd.socket = ctx.wrap_socket(httpd.socket, server_side=True)
    httpd.serve_forever()


plain = functools.partial(http.server.SimpleHTTPRequestHandler, directory="/root/mirror")
threading.Thread(target=serve, args=(8080, plain, False), daemon=True).start()
serve(8443, functools.partial(RedirectOrServe, directory="/root/mirror-bad"), True)
