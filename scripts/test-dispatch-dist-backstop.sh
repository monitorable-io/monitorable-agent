#!/usr/bin/env bash
# Test for scripts/dispatch-dist-backstop.sh. Serves a stub of GitHub's workflow-dispatch
# endpoint on 127.0.0.1 that records every request and answers with the status a case
# sets, and checks that the dispatcher sends exactly the right request, reports GitHub's
# refusals as failures, refuses a missing or malformed token without sending anything, and
# never puts the token in curl's argv. Needs python3.
# Run from anywhere: scripts/test-dispatch-dist-backstop.sh
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DISPATCHER="$ROOT/scripts/dispatch-dist-backstop.sh"
TOKEN="github_pat_TEST0123_abcXYZ"

T="$(mktemp -d)"
SERVER_PID=""
cleanup() {
    if [ -n "$SERVER_PID" ]; then kill "$SERVER_PID" 2>/dev/null || true; fi
    rm -rf "$T"
}
trap cleanup EXIT

# --- Server: records each request as one JSON line in $T/reqs, answers with $T/status ------
python3 - "$T" <<'PY' &
import http.server, json, os, sys
d = sys.argv[1]

class Stub(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass
    def handle_any(self):
        n = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(n).decode() if n else ""
        with open(os.path.join(d, "reqs"), "a") as f:
            f.write(json.dumps({"method": self.command, "path": self.path,
                                "headers": {k.lower(): v for k, v in self.headers.items()},
                                "body": body}) + "\n")
        with open(os.path.join(d, "status")) as f:
            status = int(f.read())
        payload = b"" if status == 204 else b'{"message":"stub says no"}'
        self.send_response(status)
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)
    do_POST = do_GET = handle_any

srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Stub)
with open(os.path.join(d, "port.tmp"), "w") as f:
    f.write(str(srv.server_address[1]))
os.rename(os.path.join(d, "port.tmp"), os.path.join(d, "port"))
srv.serve_forever()
PY
SERVER_PID=$!
for _ in $(seq 50); do [ -s "$T/port" ] && break; sleep 0.1; done
[ -s "$T/port" ] || { echo "FAIL: test server did not start"; exit 1; }
URL="http://127.0.0.1:$(cat "$T/port")"

# A curl shim first on PATH: records its argv, then runs the real curl with the same stdin.
REAL_CURL="$(command -v curl)"
mkdir "$T/shim"
printf '#!/bin/sh\nprintf "%%s\\n" "$@" >> "%s/argv"\nexec "%s" "$@"\n' "$T" "$REAL_CURL" > "$T/shim/curl"
chmod +x "$T/shim/curl"

printf '%s\n' "$TOKEN" > "$T/token"          # as `echo … > file` writes it: trailing newline
printf '' > "$T/token-empty"
printf 'github_pat_"injected\n' > "$T/token-quote"
printf 'github_pat_a b\n' > "$T/token-space"
printf '%s\n%s\n' "$TOKEN" "$TOKEN" > "$T/token-two-lines"

# --- Cases ---------------------------------------------------------------------------------
FAILS=0
fail() { echo "FAIL: $1"; FAILS=$((FAILS + 1)); }
requests() { if [ -f "$T/reqs" ]; then grep -c . "$T/reqs"; else echo 0; fi; }

# run <status> [VAR=value...]: one dispatch against the stub answering <status>. Sets OUT,
# RC, and REQS (how many requests reached the stub).
run() {
    local status="$1"; shift
    printf '%s' "$status" > "$T/status"
    rm -f "$T/reqs" "$T/argv"
    RC=0
    # The inner env applies the case's -u/overrides on top of the defaults (env takes -u
    # only before its assignments).
    OUT="$(env PATH="$T/shim:$PATH" DISPATCH_API_URL="$URL" DISPATCH_TOKEN_FILE="$T/token" \
        DISPATCH_ALLOW_HTTP=1 env "$@" "$DISPATCHER" 2>&1)" || RC=$?
    REQS="$(requests)"
}
# expect <name> <exit> <requests> <output substring>
expect() {
    if [ "$RC" = "$2" ] && [ "$REQS" = "$3" ] && grep -qF -- "$4" <<<"$OUT"; then
        echo "ok: $1"
    else
        fail "$1: want exit $2, $3 request(s), output with '$4'; got exit $RC, $REQS request(s):"
        printf '%s\n' "$OUT" | sed 's/^/    /'
    fi
}

# GitHub accepted the dispatch: exactly the request the workflow's inputs expect.
run 204
expect accepted 0 1 "dispatched dist-backstop (env=prod) on main"
if [ "$REQS" = 1 ]; then
    if python3 - "$T/reqs" "$TOKEN" <<'PY'; then echo "ok: request shape"; else fail "request shape"; fi
import json, sys
r = json.loads(open(sys.argv[1]).read())
h = r["headers"]
want = {
    "method": "POST",
    "path": "/repos/monitorable-io/monitorable-agent/actions/workflows/dist-backstop.yml/dispatches",
    "authorization": "Bearer " + sys.argv[2],
    "accept": "application/vnd.github+json",
    "x-github-api-version": "2022-11-28",
    "body": {"ref": "main", "inputs": {"env": "prod", "selftest": "none"}},
}
got = {
    "method": r["method"], "path": r["path"],
    "authorization": h.get("authorization"), "accept": h.get("accept"),
    "x-github-api-version": h.get("x-github-api-version"),
    "body": json.loads(r["body"] or "null"),
}
for k in want:
    if want[k] != got[k]:
        print(f"    {k}: want {want[k]!r}, got {got[k]!r}")
        sys.exit(1)
PY
fi
# The token reaches curl on stdin (a -K config), never in argv, which every local user can
# read in /proc.
if [ -s "$T/argv" ] && ! grep -qF "$TOKEN" "$T/argv" && grep -qx -- '-K' "$T/argv"; then
    echo "ok: token not in argv"
else
    fail "token not in argv (argv: $(tr '\n' ' ' 2>/dev/null < "$T/argv"))"
fi

# GitHub refused: a bad or expired token, a missing permission, an unknown workflow or ref.
run 401; expect "401 bad credentials" 1 1 "HTTP 401"
run 403; expect "403 missing permission" 1 1 "HTTP 403"
run 404; expect "404 no such workflow" 1 1 "HTTP 404"
run 422; expect "422 bad inputs" 1 1 "HTTP 422"
# A 200 is not what the dispatch endpoint answers: treated as a failure, not a success.
run 200; expect "200 is not 204" 1 1 "HTTP 200"
# A server error is retried, then still a failure.
run 503; expect "503 retried then failed" 1 3 "HTTP 503"

# Token problems are refused before anything is sent.
run 204 DISPATCH_TOKEN_FILE="$T/missing"; expect "missing token file" 2 0 "not readable"
run 204 DISPATCH_TOKEN_FILE="$T/token-empty"; expect "empty token" 2 0 "does not look like a GitHub token"
run 204 DISPATCH_TOKEN_FILE="$T/token-quote"; expect "token with a quote" 2 0 "does not look like a GitHub token"
run 204 DISPATCH_TOKEN_FILE="$T/token-space"; expect "token with a space" 2 0 "does not look like a GitHub token"
run 204 DISPATCH_TOKEN_FILE="$T/token-two-lines"; expect "two-line token file" 2 0 "does not look like a GitHub token"
# Under systemd the token comes from LoadCredential: $CREDENTIALS_DIRECTORY/github-token.
mkdir "$T/creds"; cp "$T/token" "$T/creds/github-token"
run 204 -u DISPATCH_TOKEN_FILE CREDENTIALS_DIRECTORY="$T/creds"
expect "systemd credential" 0 1 "dispatched dist-backstop"
run 204 -u DISPATCH_TOKEN_FILE -u CREDENTIALS_DIRECTORY; expect "no token source" 2 0 "no token"

# Transport: plain HTTP is refused unless the test allows it, and an unreachable API fails.
run 204 DISPATCH_ALLOW_HTTP=0; expect "plain http refused" 1 0 "dispatch request failed"
run 204 DISPATCH_API_URL="http://127.0.0.1:1"; expect "unreachable api" 1 0 "dispatch request failed"
# Nothing of the token leaks into the output of any failure either.
run 401
if grep -qF "$TOKEN" <<<"$OUT"; then fail "token printed on failure"; else echo "ok: token not printed"; fi

if [ "$FAILS" -ne 0 ]; then echo "$FAILS case(s) failed"; exit 1; fi
echo "all cases passed"
