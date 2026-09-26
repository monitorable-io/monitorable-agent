#!/usr/bin/env python3
"""Test-only download mirror for scripts/test-install-lifecycle.sh.

Runs INSIDE the LXD test container, never on a real host. /root/mirrors/<name>/ trees are
built by install-test-mirror-build.sh from the genuine staging files.

  https://127.0.0.1:8443/redir/<p> -> 302 to http://localhost:8080/<p> (cleartext)
  https://127.0.0.1:8443/<p>       -> /root/mirrors/<p>
  http://127.0.0.1:8080/<p>        -> /root/mirrors/<p>

The certificate is self-signed for "localhost"; the harness exports CURL_CA_BUNDLE for it.
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


plain = functools.partial(http.server.SimpleHTTPRequestHandler, directory="/root/mirrors")
threading.Thread(target=serve, args=(8080, plain, False), daemon=True).start()
serve(8443, functools.partial(RedirectOrServe, directory="/root/mirrors"), True)
