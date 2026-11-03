#!/usr/bin/env python3
"""Test-only download mirror for scripts/test-install-lifecycle.sh.

Runs INSIDE the LXD test container, never on a real host. /root/mirrors/<name>/ trees are
built by install-test-mirror-build.sh from the genuine staging files.

  https://127.0.0.1:8443/redir/<p> -> 302 to http://localhost:8080/<p> (cleartext)
  https://127.0.0.1:8443/<p>       -> /root/mirrors/<p>
  http://127.0.0.1:8080/<p>        -> /root/mirrors/<p>
  POST https://127.0.0.1:8443/<tree>/v1/enroll -> a stand-in for the API's enroll endpoint
       (the wire contract in the Part 2d plan), picked by <tree>:
         api-good   201, the agent key and endpoint in spaced, multi-line JSON
         api-rotate 200, rotated: true, a different agent key (the rotated one)
         api-402    402 plan_limit_reached
         api-429    429 + Retry-After: 1 on the first call, 201 on every later one

Usage: install-test-mirror.py <enroll key> <agent key> <ingest endpoint> <rotated agent key>. Every enroll call
needs "Authorization: Bearer <enroll key>" (else 401 invalid_api_key). Each POST appends
"POST <path> <body>" to /root/mirror-posts.log (never the Authorization header).

The certificate is self-signed for "localhost"; the harness exports CURL_CA_BUNDLE for it.
"""
import functools
import http.server
import json
import ssl
import sys
import threading

ENROLL_KEY, AGENT_KEY, ENDPOINT, ROTATED_KEY = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4]
POST_LOG = "/root/mirror-posts.log"
_lock = threading.Lock()
_calls = {}


def problem(status, code, detail):
    # The API's RFC 9457 shape: the installer must pick code and detail out of the rest.
    return json.dumps({
        "type": "https://monitorable.io/docs/api/errors#" + code, "title": "Error",
        "status": status, "detail": detail, "code": code, "request_id": "req_test",
    }).encode()


class RedirectOrServe(http.server.SimpleHTTPRequestHandler):
    def do_GET(self):
        if self.path.startswith("/redir/"):
            self.send_response(302)
            self.send_header("Location", "http://localhost:8080/" + self.path[len("/redir/"):])
            self.end_headers()
            return
        super().do_GET()

    def answer(self, status, body, headers=()):
        self.send_response(status)
        self.send_header("Content-Type", "application/json" if status < 300 else "application/problem+json")
        for k, v in headers:
            self.send_header(k, v)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length") or 0))
        with _lock:
            with open(POST_LOG, "a") as f:
                f.write("POST %s %s\n" % (self.path, body.decode(errors="replace")))
        parts = self.path.split("/")
        if len(parts) != 4 or parts[0] != "" or parts[2:] != ["v1", "enroll"]:
            self.answer(404, problem(404, "not_found", "no such endpoint"))
            return
        tree = parts[1]
        if self.headers.get("Authorization") != "Bearer " + ENROLL_KEY:
            self.answer(401, problem(401, "invalid_api_key", "the API key is not valid"))
            return
        with _lock:
            _calls[tree] = _calls.get(tree, 0) + 1
            n = _calls[tree]
        # protojson output is not the JSON one types: random whitespace around ":" and ",",
        # line breaks, and the key last. The installer's parser must read this as well.
        created = (
            '{ "server_id" : 42 ,\n "name": "web-01", "ingest_endpoint" : "%s",\n'
            ' "rotated": false, "agent_api_key":"%s" }' % (ENDPOINT, AGENT_KEY)
        ).encode()
        if tree == "api-good":
            self.answer(201, created)
        elif tree == "api-rotate":
            self.answer(200, (
                '{"server_id":42,\n\t"name" :\t"web-01",\n "agent_api_key" : "%s" ,'
                ' "ingest_endpoint":"%s", "rotated" : true\n}' % (ROTATED_KEY, ENDPOINT)
            ).encode())
        elif tree == "api-402":
            self.answer(402, problem(402, "plan_limit_reached", "max_servers is 3 on Lite"))
        elif tree == "api-429":
            if n == 1:
                self.answer(429, problem(429, "rate_limited", "too many requests"), [("Retry-After", "1")])
            else:
                self.answer(201, created)
        else:
            self.answer(404, problem(404, "not_found", "no such tree"))


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
