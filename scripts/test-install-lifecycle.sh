#!/bin/sh
# Lifecycle harness for distribution/install.sh. Dev host only (needs LXD).
# Usage: scripts/test-install-lifecycle.sh <lxd-container>
# The container must be a networked Ubuntu 24.04 that can reach get-mon.ok9k.com (the
# genuine files are copied from there once). Every install then runs against a local
# https mirror (scripts/install-test-mirror.py) serving copies re-signed with a throwaway
# key, so the harness can stage any signature, version or checksum failure.
# shellcheck disable=SC2016,SC2034  # check() strings hold $vars for eval to expand later; rc feeds them the same way.
set -eu
CT="${1:?usage: $0 <lxd-container>}"
SRC_URL="${SRC_URL:-https://get-mon.ok9k.com}"
ORIGIN="https://localhost:8443"
KEY="$(printf '%064d' 0 | tr 0 a)"
ENDPOINT="https://ingest-mon.ok9k.com"
MIN="v1.3.0"
FAILS=0

# Every harness command trusts the mirror's self-signed certificate (and only it).
run() { lxc exec "$CT" --env CURL_CA_BUNDLE=/root/tls/cert.pem -- sh -c "$1"; }
# run_rc <cmd>: prints combined output to /tmp/out in the container, returns the exit code
run_rc() { lxc exec "$CT" --env CURL_CA_BUNDLE=/root/tls/cert.pem -- sh -c "$1 > /tmp/out 2>&1; echo \$?" ; }
# raw: no CA override, for the one-time copy from the real staging origin
raw() { lxc exec "$CT" -- sh -c "$1"; }
out_has() { run "grep -qF -- '$1' /tmp/out"; }
pass() { printf 'PASS %s\n' "$1"; }
fail() { printf 'FAIL %s\n' "$1"; run 'tail -n 20 /tmp/out' || true; FAILS=$((FAILS + 1)); }
check() { if eval "$2"; then pass "$1"; else fail "$1"; fi; }

# Genuine files, TLS certificate and throwaway signing key.
raw "rm -rf /root/mirrors /root/tls && mkdir -p /root/tls /root/mirrors/src/configs/linux"
raw "a=\$(dpkg --print-architecture) && cd /root/mirrors/src && curl -fsS -o monitorable-agent-linux-\$a $SRC_URL/binaries/otel/latest/monitorable-agent-linux-\$a && curl -fsS -o SHA256SUMS $SRC_URL/binaries/otel/latest/SHA256SUMS && curl -fsS -o configs/linux/collector-config.yaml $SRC_URL/configs/linux/collector-config.yaml && curl -fsS -o configs/linux/monitorable-agent.service $SRC_URL/configs/linux/monitorable-agent.service"
raw "openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 1 -subj /CN=localhost -addext subjectAltName=DNS:localhost -keyout /root/tls/key.pem -out /root/tls/cert.pem >/dev/null 2>&1"
raw "openssl ecparam -name prime256v1 -genkey -noout -out /root/tls/sign.key"
# A second key the installer does not embed: signs the genuine sums for the "valid
# signature, wrong key" case.
raw "openssl ecparam -name prime256v1 -genkey -noout -out /root/tls/other.key"
TEST_PUB="$(raw "openssl pkey -in /root/tls/sign.key -pubout" | grep -v -- '-----' | tr -d '\n')"
lxc file push --quiet scripts/install-test-mirror.py "$CT/root/install-test-mirror.py"
lxc file push --quiet scripts/install-test-mirror-build.sh "$CT/root/mirror-build.sh"
trap 'lxc exec "$CT" -- systemctl stop install-test-mirror >/dev/null 2>&1 || true' EXIT
run "systemctl stop install-test-mirror >/dev/null 2>&1; systemd-run --quiet --collect --unit=install-test-mirror python3 /root/install-test-mirror.py"

# render <base url> <path in container> [<min version>]: fill the placeholders the way the
# backend (BASE_URL, SIGNING_PUBKEY) and the vendoring step (MIN_VERSION) do.
render() {
    sed -e "s|@@BASE_URL@@|$1|g" -e "s|@@SIGNING_PUBKEY@@|$TEST_PUB|g" -e "s|@@MIN_VERSION@@|${3:-$MIN}|g" \
        distribution/install.sh | lxc exec "$CT" -- sh -c "cat > $2"
}

run "sh /root/mirror-build.sh good $MIN ok ok"
run "sh /root/mirror-build.sh tampered $MIN tampered ok"
run "for i in 1 2 3 4 5 6 7 8 9 10; do curl -fsS -o /dev/null $ORIGIN/good/configs/linux/collector-config.yaml && exit 0; sleep 1; done; exit 1"
render "$ORIGIN/good" /root/install.sh
render "$ORIGIN/tampered" /root/install-tampered.sh
render "$ORIGIN/redir/good" /root/install-redirect.sh

# Every case up to fresh-install needs a host with no agent; assert it so none of them can
# pass because of a leftover from an earlier run.
check clean-host-precondition '! run "getent passwd monitorable || test -e /etc/monitorable/agent.env || test -e /etc/systemd/system/monitorable-agent.service"'

# A host without systemd is refused before ANY side effect. systemd is hidden for this one
# run by a private tmpfs over /run/systemd in its own mount namespace.
rc=$(run_rc "unshare -m sh -c 'mount -t tmpfs tmpfs /run/systemd && exec sh /root/install.sh --endpoint=$ENDPOINT --api-key=$KEY'")
check no-systemd-refused '[ "$rc" = 1 ] && out_has "requires systemd" && ! run "getent passwd monitorable || test -e /opt/monitorable || test -e /etc/monitorable || test -e /etc/systemd/system/monitorable-agent.service"'
run "sh /root/install.sh --uninstall >/dev/null 2>&1"

# An https origin that redirects to http is refused: nothing downloads in cleartext.
rc=$(run_rc "sh /root/install-redirect.sh --endpoint=$ENDPOINT --api-key=$KEY")
check https-redirect-to-http-refused '[ "$rc" = 1 ] && out_has "Failed to download" && ! run "test -e /opt/monitorable/monitorable-agent"'
run "sh /root/install.sh --uninstall >/dev/null 2>&1"

# A checksum mismatch on a fresh install leaves no service user or group behind.
rc=$(run_rc "sh /root/install-tampered.sh --endpoint=$ENDPOINT --api-key=$KEY")
check tampered-leaves-no-user '[ "$rc" = 1 ] && out_has "Checksum mismatch" && ! run "getent passwd monitorable || getent group monitorable"'
run "sh /root/install.sh --uninstall >/dev/null 2>&1"

# Signature: a valid signature over OTHER bytes, a valid signature over the genuine sums by
# a DIFFERENT key, an empty .sig, or no .sig at all, installs nothing.
run "sh /root/mirror-build.sh badsig $MIN ok bad && sh /root/mirror-build.sh nosig $MIN ok none"
run "sh /root/mirror-build.sh otherkey $MIN ok otherkey && sh /root/mirror-build.sh emptysig $MIN ok empty"
render "$ORIGIN/badsig" /root/install-badsig.sh
render "$ORIGIN/nosig" /root/install-nosig.sh
render "$ORIGIN/otherkey" /root/install-otherkey.sh
render "$ORIGIN/emptysig" /root/install-emptysig.sh
rc=$(run_rc "sh /root/install-badsig.sh --endpoint=$ENDPOINT --api-key=$KEY")
check bad-signature-refused '[ "$rc" = 1 ] && out_has "signature does NOT verify" && ! run "test -e /opt/monitorable/monitorable-agent || getent passwd monitorable"'
run "sh /root/install.sh --uninstall >/dev/null 2>&1"
rc=$(run_rc "sh /root/install-nosig.sh --endpoint=$ENDPOINT --api-key=$KEY")
check missing-signature-refused '[ "$rc" = 1 ] && out_has "Could not download SHA256SUMS.sig" && ! run "test -e /opt/monitorable/monitorable-agent || getent passwd monitorable"'
run "sh /root/install.sh --uninstall >/dev/null 2>&1"
rc=$(run_rc "sh /root/install-otherkey.sh --endpoint=$ENDPOINT --api-key=$KEY")
check other-key-signature-refused '[ "$rc" = 1 ] && out_has "signature does NOT verify" && ! run "test -e /opt/monitorable/monitorable-agent || getent passwd monitorable"'
run "sh /root/install.sh --uninstall >/dev/null 2>&1"
rc=$(run_rc "sh /root/install-emptysig.sh --endpoint=$ENDPOINT --api-key=$KEY")
check empty-signature-refused '[ "$rc" = 1 ] && out_has "signature does NOT verify" && ! run "test -e /opt/monitorable/monitorable-agent || getent passwd monitorable"'
run "sh /root/install.sh --uninstall >/dev/null 2>&1"

# A key-free UPDATE against a bad signature leaves the installed agent exactly as it was:
# same binary bytes, service still running.
rc=$(run_rc "sh /root/install.sh --endpoint=$ENDPOINT --api-key=$KEY")
GOOD_SHA="$(run "sha256sum /opt/monitorable/monitorable-agent 2>/dev/null" | cut -d' ' -f1)"
check keyfree-badsig-precondition '[ "$rc" = 0 ] && [ -n "$GOOD_SHA" ] && run "systemctl is-active --quiet monitorable-agent"'
rc=$(run_rc "sh /root/install-badsig.sh")
check keyfree-update-bad-signature-untouched '[ "$rc" = 1 ] && out_has "Updating the existing agent" && out_has "signature does NOT verify" && [ "$(run "sha256sum /opt/monitorable/monitorable-agent" | cut -d" " -f1)" = "$GOOD_SHA" ] && run "systemctl is-active --quiet monitorable-agent"'
run "sh /root/install.sh --uninstall >/dev/null 2>&1"

# A template served without its placeholders filled is refused before anything else.
lxc exec "$CT" -- sh -c "cat > /root/install-unrendered.sh" < distribution/install.sh
rc=$(run_rc "sh /root/install-unrendered.sh --endpoint=$ENDPOINT --api-key=$KEY")
check unrendered-template-refused '[ "$rc" = 1 ] && out_has "served unrendered" && ! run "test -e /opt/monitorable"'

# No openssl: refused before any side effect. The binary is hidden for this one run by a
# bind mount of /dev/null in a private mount namespace; the inner `! command -v` is the
# precondition, so the case cannot pass without openssl actually being hidden.
rc=$(run_rc "unshare -m sh -c 'mount --bind /dev/null \"\$(command -v openssl)\" && ! command -v openssl >/dev/null && exec sh /root/install.sh --endpoint=$ENDPOINT --api-key=$KEY'")
check no-openssl-refused '[ "$rc" = 1 ] && out_has "openssl is required" && ! run "test -e /opt/monitorable || getent passwd monitorable"'
run "sh /root/install.sh --uninstall >/dev/null 2>&1"

# Replay: validly signed sums for a release below the installer's floor are refused.
run "sh /root/mirror-build.sh old v1.2.9 ok ok"
render "$ORIGIN/old" /root/install-old.sh
rc=$(run_rc "sh /root/install-old.sh --endpoint=$ENDPOINT --api-key=$KEY")
check replay-below-floor-refused '[ "$rc" = 1 ] && out_has "older than this installer" && ! run "test -e /opt/monitorable/monitorable-agent"'
run "sh /root/install.sh --uninstall >/dev/null 2>&1"

# A version component too long for the shell's integer test (here 10 digits) is malformed,
# never compared: it fails closed instead of erroring and falling through to the next one.
run "sh /root/mirror-build.sh hugever v1.9999999999.0 ok ok"
render "$ORIGIN/hugever" /root/install-hugever.sh
rc=$(run_rc "sh /root/install-hugever.sh --endpoint=$ENDPOINT --api-key=$KEY")
check oversized-version-component-refused '[ "$rc" = 1 ] && out_has "older than this installer" && ! run "test -e /opt/monitorable/monitorable-agent"'
run "sh /root/install.sh --uninstall >/dev/null 2>&1"

# Numeric, not lexical: v1.10.0 clears a v1.9.0 floor.
run "sh /root/mirror-build.sh numeric v1.10.0 ok ok"
render "$ORIGIN/numeric" /root/install-numeric.sh v1.9.0
rc=$(run_rc "sh /root/install-numeric.sh --endpoint=$ENDPOINT --api-key=$KEY")
check floor-compares-numerically '[ "$rc" = 0 ] && run "systemctl is-active --quiet monitorable-agent"'
run "sh /root/install.sh --uninstall >/dev/null 2>&1"

# Pin: --version=vX installs only sums signed as vX.
run "sh /root/mirror-build.sh pinned v1.3.0 ok ok v1.3.1 && sh /root/mirror-build.sh pinned v1.3.0 ok ok v1.3.0"
render "$ORIGIN/pinned" /root/install-pinned.sh
rc=$(run_rc "sh /root/install-pinned.sh --version=v1.3.1 --endpoint=$ENDPOINT --api-key=$KEY")
check pinned-version-mismatch-refused '[ "$rc" = 1 ] && out_has "not the requested v1.3.1" && ! run "test -e /opt/monitorable/monitorable-agent"'
run "sh /root/install.sh --uninstall >/dev/null 2>&1"
rc=$(run_rc "sh /root/install-pinned.sh --version=v1.3.0 --endpoint=$ENDPOINT --api-key=$KEY")
check pinned-version-match-installs '[ "$rc" = 0 ] && run "systemctl is-active --quiet monitorable-agent"'
run "sh /root/install.sh --uninstall >/dev/null 2>&1"

# A path-shaped --version resolves to latest/ on the server; the pin still refuses it.
# curl and the mirror's http.server both normalize "otel/../latest" down to "latest"
# (RFC 3986 dot-segment removal), so the request actually lands one level up from the
# usual otel/<version>/ layout; stage the same genuine, correctly signed v1.3.0 bundle
# there so the download succeeds and the pin check is what refuses it.
run "sh /root/mirror-build.sh good $MIN ok ok ../latest"
rc=$(run_rc "sh /root/install.sh --version=../latest --endpoint=$ENDPOINT --api-key=$KEY")
check dotdot-version-refused '[ "$rc" = 1 ] && out_has "not the requested ../latest" && ! run "test -e /opt/monitorable/monitorable-agent"'
run "sh /root/install.sh --uninstall >/dev/null 2>&1"

# Signed sums without exactly one version line fail closed.
run "sh /root/mirror-build.sh noversion - ok ok && sh /root/mirror-build.sh dupversion dup ok ok"
render "$ORIGIN/noversion" /root/install-noversion.sh
render "$ORIGIN/dupversion" /root/install-dupversion.sh
rc=$(run_rc "sh /root/install-noversion.sh --endpoint=$ENDPOINT --api-key=$KEY")
check missing-version-line-refused '[ "$rc" = 1 ] && out_has "exactly one version line" && ! run "test -e /opt/monitorable/monitorable-agent"'
run "sh /root/install.sh --uninstall >/dev/null 2>&1"
rc=$(run_rc "sh /root/install-dupversion.sh --endpoint=$ENDPOINT --api-key=$KEY")
check duplicate-version-line-refused '[ "$rc" = 1 ] && out_has "exactly one version line" && ! run "test -e /opt/monitorable/monitorable-agent"'
run "sh /root/install.sh --uninstall >/dev/null 2>&1"

# A stale download left in a pre-existing, non-root-owned /opt/monitorable is never reused:
# curl -o would write into that inode (still owned by its planter, who could rewrite it
# after verification) and the rename would install it. The installed binary must be a new,
# root-owned file.
run "a=\$(dpkg --print-architecture) && mkdir -p /opt/monitorable && chown nobody:nogroup /opt/monitorable && printf planted > /opt/monitorable/monitorable-agent-linux-\$a.tmp && chown nobody:nogroup /opt/monitorable/monitorable-agent-linux-\$a.tmp"
rc=$(run_rc "sh /root/install.sh --endpoint=$ENDPOINT --api-key=$KEY")
check stale-tmp-not-reused '[ "$rc" = 0 ] && [ "$(run "stat -c %U /opt/monitorable/monitorable-agent")" = root ]'
run "sh /root/install.sh --uninstall >/dev/null 2>&1"

# fresh install with a key
rc=$(run_rc "sh /root/install.sh --endpoint=$ENDPOINT --api-key=$KEY")
check fresh-install '[ "$rc" = 0 ] && run "systemctl is-active --quiet monitorable-agent"'
# The output names the digest of the binary it verified, for comparison with the
# SHA256SUMS attached to the GitHub release.
SHA="$(run "sha256sum /opt/monitorable/monitorable-agent" | cut -d' ' -f1)"
check fresh-install-prints-sha256 '[ -n "$SHA" ] && out_has "sha256 $SHA"'

# key-free update keeps key + endpoint
rc=$(run_rc "sh /root/install.sh")
check keyfree-update '[ "$rc" = 0 ] && out_has "Updating the existing agent" && run "grep -qx MONITORABLE_API_KEY=$KEY /etc/monitorable/agent.env && grep -qx MONITORABLE_ENDPOINT=$ENDPOINT /etc/monitorable/agent.env"'

# an explicit flag still wins over agent.env
rc=$(run_rc "sh /root/install.sh --endpoint=https://ingest-mon.ok9k.com/")
check flag-beats-agent-env '[ "$rc" = 0 ] && run "grep -qx MONITORABLE_ENDPOINT=https://ingest-mon.ok9k.com/ /etc/monitorable/agent.env"'
# Reset agent.env's endpoint back to $ENDPOINT before the next cases (which corrupt/edit
# agent.env directly and don't care about ENDPOINT's exact value, but do need a well-formed
# file to start from). Not run via run_rc/check: a failure here must not abort the whole
# harness under `set -e` (it does fail pre-fix, since install.sh still requires a key).
run "sh /root/install.sh --endpoint=$ENDPOINT >/dev/null 2>&1" || true

# CRLF agent.env is rejected by the charset check, never written back corrupted
run "cp /etc/monitorable/agent.env /root/agent.env.bak && sed -i 's/\$/\r/' /etc/monitorable/agent.env"
rc=$(run_rc "sh /root/install.sh")
check crlf-agent-env '[ "$rc" != 0 ] && out_has "contains characters outside"'
run "cp /root/agent.env.bak /etc/monitorable/agent.env"

# agent.env without the key line
run "grep -v '^MONITORABLE_API_KEY=' /root/agent.env.bak > /etc/monitorable/agent.env"
rc=$(run_rc "sh /root/install.sh")
check env-without-key '[ "$rc" != 0 ] && out_has "No agent is installed on this server"'
run "cp /root/agent.env.bak /etc/monitorable/agent.env"

# --uninstall refuses other options
rc=$(run_rc "sh /root/install.sh --uninstall --version=v1.2.0")
check uninstall-rejects-options '[ "$rc" = 1 ] && out_has "--uninstall takes no other options"'

# --uninstall on a failed unit still removes everything
# make the unit fail: swap the binary for /bin/false and restart. The running binary's
# inode can't be overwritten in place (ETXTBSY) while the unit is up, so stop it first.
run "systemctl stop monitorable-agent; cp /bin/false /opt/monitorable/monitorable-agent && systemctl restart monitorable-agent" || true
# Assert the precondition actually holds (the unit is genuinely not healthy) so this case
# can't pass vacuously if the swap above ever silently stops working again.
sleep 2
check failed-unit-precondition '! run "systemctl is-active --quiet monitorable-agent"'
rc=$(run_rc "sh /root/install.sh --uninstall")
check uninstall-failed-unit '[ "$rc" = 0 ] && out_has "Uninstalled. Now open the dashboard"'
check uninstall-leaves-nothing '! run "test -e /opt/monitorable || test -e /etc/monitorable || test -e /var/lib/monitorable || test -e /var/log/monitorable || test -e /etc/systemd/system/monitorable-agent.service || test -e /etc/udev/rules.d/99-monitorable-nvme-smart.rules || getent passwd monitorable || getent group monitorable"'

# second --uninstall is a no-op
rc=$(run_rc "sh /root/install.sh --uninstall")
check uninstall-idempotent '[ "$rc" = 0 ] && out_has "Nothing to remove"'

# key-free run on a clean host
rc=$(run_rc "sh /root/install.sh")
check keyfree-on-clean-host '[ "$rc" = 1 ] && out_has "No agent is installed on this server"'

# key-free update of a pre-v1.2.0 host: v1.0.0–v1.1.23 kept the key and endpoint as
# Environment= lines in the monitorable-collector unit and wrote no agent.env. A stand-in
# unit in that exact shape (sleep instead of the old binary) is enough: the script only
# reads the two lines, then installs and retires the old unit.
run "printf '[Service]\nExecStart=/bin/sleep infinity\nEnvironment=MONITORABLE_API_KEY=%s\nEnvironment=MONITORABLE_ENDPOINT=%s\n[Install]\nWantedBy=multi-user.target\n' $KEY $ENDPOINT > /etc/systemd/system/monitorable-collector.service && systemctl daemon-reload && systemctl enable --now monitorable-collector >/dev/null 2>&1"
rc=$(run_rc "sh /root/install.sh")
check keyfree-legacy-unit '[ "$rc" = 0 ] && out_has "Updating the existing agent" && out_has "pre-v1.2.0" && run "grep -qx MONITORABLE_API_KEY=$KEY /etc/monitorable/agent.env && grep -qx MONITORABLE_ENDPOINT=$ENDPOINT /etc/monitorable/agent.env && systemctl is-active --quiet monitorable-agent && ! test -e /etc/systemd/system/monitorable-collector.service && ! systemctl is-active --quiet monitorable-collector"'
run "sh /root/install.sh --uninstall >/dev/null 2>&1"

# a pre-v1.2.0 unit without the key line is still "No agent is installed", never an
# install with an empty key
run "printf '[Service]\nExecStart=/bin/sleep infinity\nEnvironment=MONITORABLE_ENDPOINT=%s\n' $ENDPOINT > /etc/systemd/system/monitorable-collector.service"
rc=$(run_rc "sh /root/install.sh")
check legacy-unit-without-key '[ "$rc" = 1 ] && out_has "No agent is installed on this server"'
run "rm -f /etc/systemd/system/monitorable-collector.service && systemctl daemon-reload"

# --uninstall reports a failed delete instead of aborting outright under set -e.
# chattr +i needs CAP_LINUX_IMMUTABLE, which this unprivileged LXD container's profile
# denies (verified: chattr +i -> "Operation not permitted"), so the obstruction here is a
# tmpfs remounted read-only over /etc/monitorable: rm -rf's unlink of the files inside it
# then hits a genuine EROFS failure.
run "sh /root/install.sh --endpoint=$ENDPOINT --api-key=$KEY >/dev/null 2>&1"
run "mount -t tmpfs tmpfs /etc/monitorable && echo obstruction > /etc/monitorable/keepme && mount -o remount,ro /etc/monitorable"
rc=$(run_rc "sh /root/install.sh --uninstall")
check uninstall-rm-failure '[ "$rc" = 1 ] && out_has "Uninstall incomplete" && out_has "Could not delete /etc/monitorable"'
# undo the obstruction and finish the uninstall so the host is left clean
run "mount -o remount,rw /etc/monitorable && rm -f /etc/monitorable/keepme && umount /etc/monitorable"
rc=$(run_rc "sh /root/install.sh --uninstall")
check uninstall-rm-failure-cleanup '[ "$rc" = 0 ] && ! run "test -e /etc/monitorable"'

printf '%s failure(s)\n' "$FAILS"
[ "$FAILS" -eq 0 ]
