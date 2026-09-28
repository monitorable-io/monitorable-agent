#!/usr/bin/env bash
# Test for scripts/check-published-dist.sh. Builds a signed staging release with a throwaway
# key (publish-dist.sh --render-only, VERSION=v0.0.0), lays it out as the published tree,
# serves one copy per case on 127.0.0.1, and checks that the checker passes the good tree
# and fails each broken one naming exactly the right checks. Needs go, openssl, python3.
# Run from anywhere: scripts/test-check-published-dist.sh
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
CHECKER="$ROOT/scripts/check-published-dist.sh"

T="$(mktemp -d)"
SERVER_PID=""
cleanup() {
    if [ -n "$SERVER_PID" ]; then kill "$SERVER_PID" 2>/dev/null || true; fi
    rm -rf "$T"
}
trap cleanup EXIT

# --- Fixture: a signed release, laid out as published ------------------------------------
openssl ecparam -name prime256v1 -genkey -noout -out "$T/k.pem"
openssl pkey -in "$T/k.pem" -pubout -out "$T/k.pub"
openssl ecparam -name prime256v1 -genkey -noout -out "$T/other.pem"
openssl pkey -in "$T/other.pem" -pubout -out "$T/other.pub"
RELEASE_SIGNING_KEY="$(cat "$T/k.pem")" EXPECTED_PUBKEY_FILE="$T/k.pub" VERSION=v0.0.0 \
    scripts/publish-dist.sh staging --render-only >/dev/null
B="$ROOT/distribution-build"
GOOD="$T/good"
mkdir -p "$GOOD/binaries/otel/latest" "$GOOD/configs/linux" "$T/srv"
cp "$B/installer/install.sh" "$GOOD/install.sh"
cp "$B/binaries/otel/SHA256SUMS" "$B/binaries/otel/SHA256SUMS.sig" \
   "$B/binaries/otel/monitorable-agent-linux-amd64" "$B/binaries/otel/monitorable-agent-linux-arm64" \
   "$GOOD/binaries/otel/latest/"
cp "$B/configs/linux/collector-config.yaml" "$B/configs/linux/monitorable-agent.service" "$GOOD/configs/linux/"
printf '{"version":"v0.0.0"}\n' > "$GOOD/latest.json"
SUMS=binaries/otel/latest/SHA256SUMS

# new_case <name>: a hard-linked copy of the good tree, served at $URL/<name>.
new_case() { cp -al "$GOOD" "$T/srv/$1"; }
# put <case> <path>: replace one file of a case with stdin. The old file is unlinked first,
# so the hard-linked original in $GOOD is never written through.
put() { local f="$T/srv/$1/$2"; cat > "$f.new"; rm -f "$f"; mv "$f.new" "$f"; }
# sign <case>: re-sign that case's SHA256SUMS with the throwaway key (a validly signed file).
sign() { openssl dgst -sha256 -sign "$T/k.pem" "$T/srv/$1/$SUMS" | put "$1" "$SUMS.sig"; }

new_case ok
new_case sig-other-key
openssl dgst -sha256 -sign "$T/other.pem" "$GOOD/$SUMS" | put sig-other-key "$SUMS.sig"
new_case sums-byte-changed
sed '2s/^./x/' "$GOOD/$SUMS" | put sums-byte-changed "$SUMS"
new_case latest-json-other
printf '{"version":"v9.9.9"}\n' | put latest-json-other latest.json
new_case floor-above
sed 's/^MIN_VERSION=.*/MIN_VERSION="v0.0.1"/' "$GOOD/install.sh" | put floor-above install.sh
new_case two-versions
{ cat "$GOOD/$SUMS"; echo "# version v9.9.9"; } | put two-versions "$SUMS"
sign two-versions
new_case installer-other-key
sed "s|^SIGNING_PUBKEY=.*|SIGNING_PUBKEY=\"$(grep -v -- '-----' "$T/other.pub" | tr -d '\n')\"|" "$GOOD/install.sh" |
    put installer-other-key install.sh
new_case base-url-prod
sed 's|^BASE_URL=.*|BASE_URL="https://get.monitorable.io"|' "$GOOD/install.sh" | put base-url-prod install.sh
new_case config-changed
{ cat "$GOOD/configs/linux/collector-config.yaml"; echo "# changed after signing"; } |
    put config-changed configs/linux/collector-config.yaml
new_case binary-truncated
head -c 1000 "$GOOD/binaries/otel/latest/monitorable-agent-linux-amd64" |
    put binary-truncated binaries/otel/latest/monitorable-agent-linux-amd64
new_case installer-404
rm "$T/srv/installer-404/install.sh"
new_case crlf-version
sed '1s/$/\r/' "$GOOD/$SUMS" | put crlf-version "$SUMS"
sign crlf-version
new_case latest-json-pretty
printf '{\n  "version": "v0.0.0",\n  "notes": "pretty-printed, extra field"\n}\n' | put latest-json-pretty latest.json
new_case retry-fixed
printf '{"version":"v9.9.9"}\n' | put retry-fixed latest.json

# --- Server --------------------------------------------------------------------------------
python3 - "$T/srv" "$T/port" <<'PY' &
import functools, http.server, sys

class Quiet(http.server.SimpleHTTPRequestHandler):
    def log_message(self, *args):
        pass

srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), functools.partial(Quiet, directory=sys.argv[1]))
with open(sys.argv[2] + ".tmp", "w") as f:
    f.write(str(srv.server_address[1]))
import os
os.rename(sys.argv[2] + ".tmp", sys.argv[2])
srv.serve_forever()
PY
SERVER_PID=$!
for _ in $(seq 50); do [ -s "$T/port" ] && break; sleep 0.1; done
[ -s "$T/port" ] || { echo "FAIL: test server did not start"; exit 1; }
URL="http://127.0.0.1:$(cat "$T/port")"

# --- Cases ---------------------------------------------------------------------------------
FAILS=0
# run_checker <case> [VAR=value...]: the checker against one served case, as staging.
run_checker() {
    local c="$1"; shift
    env CHECK_INSTALLER_URL="$URL/$c/install.sh" CHECK_BASE_URL="$URL/$c" CHECK_KEY_FILE="$T/k.pub" \
        CHECK_ALLOW_HTTP=1 CHECK_RETRY_DELAY=0 "$@" "$CHECKER" staging
}
# expect <case> <exit> <RESULT line> [VAR=value...]
expect() {
    local c="$1" want_rc="$2" want="$3" out rc=0
    shift 3
    out="$(run_checker "$c" "$@" 2>&1)" || rc=$?
    if [ "$rc" = "$want_rc" ] && [ "$(tail -n 1 <<<"$out")" = "$want" ]; then
        echo "ok: $c"
    else
        echo "FAIL: $c: want exit $want_rc and '$want', got exit $rc:"
        printf '%s\n' "$out" | sed 's/^/    /'
        FAILS=$((FAILS + 1))
    fi
}

expect ok                  0 "RESULT PASS staging v0.0.0"
expect sig-other-key       1 "RESULT FAIL staging: signature"
expect sums-byte-changed   1 "RESULT FAIL staging: signature"
expect latest-json-other   1 "RESULT FAIL staging: latest-json"
expect floor-above         1 "RESULT FAIL staging: version-floor"
expect two-versions        1 "RESULT FAIL staging: version-floor, latest-json"
expect installer-other-key 1 "RESULT FAIL staging: installer-key"
expect base-url-prod       1 "RESULT FAIL staging: installer"
expect config-changed      1 "RESULT FAIL staging: signed-files"
expect binary-truncated    1 "RESULT FAIL staging: binaries"
expect installer-404       1 "RESULT FAIL staging: installer, installer-key, version-floor"
expect crlf-version        1 "RESULT FAIL staging: version-floor, latest-json"
expect latest-json-pretty  0 "RESULT PASS staging v0.0.0"
# A redirect is never followed: the directory URL answers 301, and its body is no installer.
expect ok                  1 "RESULT FAIL staging: installer, installer-key, version-floor" CHECK_INSTALLER_URL="$URL/ok"
# The workflow's self-test: the real files against the other env's key.
expect ok                  1 "RESULT FAIL staging: installer-key, signature" CHECK_KEY_FILE="$T/other.pub"
# The scheduled run never reads plain HTTP: without CHECK_ALLOW_HTTP every fetch is refused.
expect ok                  1 "RESULT FAIL staging: installer, installer-key, signature, version-floor, latest-json, signed-files, binaries" CHECK_ALLOW_HTTP=0

# Retry: the first pass sees a wrong latest.json, which is fixed while the checker waits.
rc=0
run_checker retry-fixed CHECK_RETRY_DELAY=5 > "$T/retry.out" 2>&1 &
checker_pid=$!
for _ in $(seq 100); do grep -q '^== first pass failed' "$T/retry.out" && break; sleep 0.1; done
printf '{"version":"v0.0.0"}\n' | put retry-fixed latest.json
wait "$checker_pid" || rc=$?
if [ "$rc" = 0 ] && grep -q '^== first pass failed (latest-json)' "$T/retry.out" &&
   [ "$(tail -n 1 "$T/retry.out")" = "RESULT PASS staging v0.0.0" ]; then
    echo "ok: retry-fixed"
else
    echo "FAIL: retry-fixed: want a failed first pass then a pass, got exit $rc:"
    sed 's/^/    /' "$T/retry.out"
    FAILS=$((FAILS + 1))
fi

# Usage errors exit 2.
for args in "" "bogus"; do
    rc=0
    # shellcheck disable=SC2086  # intentional: "" must become no argument at all
    "$CHECKER" $args >/dev/null 2>&1 || rc=$?
    if [ "$rc" = 2 ]; then echo "ok: usage '$args'"; else echo "FAIL: usage '$args': want exit 2, got $rc"; FAILS=$((FAILS + 1)); fi
done

if [ "$FAILS" -ne 0 ]; then echo "$FAILS case(s) failed"; exit 1; fi
echo "all cases passed"
