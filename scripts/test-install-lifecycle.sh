#!/bin/sh
# Lifecycle harness for distribution/install.sh. Dev host only (needs LXD).
# Usage: scripts/test-install-lifecycle.sh <lxd-container>
# The container must be a networked Ubuntu 24.04 that can reach get-mon.ok9k.com.
# shellcheck disable=SC2016,SC2034  # check() strings hold $vars for eval to expand later; rc feeds them the same way.
set -eu
CT="${1:?usage: $0 <lxd-container>}"
BASE_URL="${BASE_URL:-https://get-mon.ok9k.com}"
KEY="$(printf '%064d' 0 | tr 0 a)"
ENDPOINT="https://ingest-mon.ok9k.com"
FAILS=0

# render [<base url> <path in container>]: install.sh with @@BASE_URL@@ baked, as publish does
render() { sed "s|@@BASE_URL@@|${1:-$BASE_URL}|g" distribution/install.sh | lxc exec "$CT" -- sh -c "cat > ${2:-/root/install.sh}"; }
run() { lxc exec "$CT" -- sh -c "$1"; }
# run_rc <cmd>: prints combined output to /tmp/out in the container, returns the exit code
run_rc() { lxc exec "$CT" -- sh -c "$1 > /tmp/out 2>&1; echo \$?" ; }
out_has() { run "grep -qF -- '$1' /tmp/out"; }
pass() { printf 'PASS %s\n' "$1"; }
fail() { printf 'FAIL %s\n' "$1"; run 'tail -n 20 /tmp/out' || true; FAILS=$((FAILS + 1)); }
check() { if eval "$2"; then pass "$1"; else fail "$1"; fi; }

render

# Local download mirror (scripts/install-test-mirror.py) for the cases a real origin can't
# stage: a tampered binary, and an https origin that redirects to cleartext. Filled with
# the current staging files; stopped on exit however the harness ends.
run "rm -rf /root/mirror /root/mirror-bad /root/tls && mkdir -p /root/tls /root/mirror/binaries/otel/latest /root/mirror/configs/linux"
run "a=\$(dpkg --print-architecture) && cd /root/mirror && curl -fsS -o binaries/otel/latest/monitorable-agent-linux-\$a $BASE_URL/binaries/otel/latest/monitorable-agent-linux-\$a && curl -fsS -o binaries/otel/latest/SHA256SUMS $BASE_URL/binaries/otel/latest/SHA256SUMS && curl -fsS -o configs/linux/collector-config.yaml $BASE_URL/configs/linux/collector-config.yaml && curl -fsS -o configs/linux/monitorable-agent.service $BASE_URL/configs/linux/monitorable-agent.service"
run "cp -r /root/mirror /root/mirror-bad && printf x >> /root/mirror-bad/binaries/otel/latest/monitorable-agent-linux-\$(dpkg --print-architecture)"
run "openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 1 -subj /CN=localhost -addext subjectAltName=DNS:localhost -keyout /root/tls/key.pem -out /root/tls/cert.pem >/dev/null 2>&1"
lxc file push --quiet scripts/install-test-mirror.py "$CT/root/install-test-mirror.py"
trap 'lxc exec "$CT" -- systemctl stop install-test-mirror >/dev/null 2>&1 || true' EXIT
run "systemctl stop install-test-mirror >/dev/null 2>&1; systemd-run --quiet --collect --unit=install-test-mirror python3 /root/install-test-mirror.py"
run "for i in 1 2 3 4 5 6 7 8 9 10; do curl -fsS --cacert /root/tls/cert.pem -o /dev/null https://localhost:8443/configs/linux/collector-config.yaml && exit 0; sleep 1; done; exit 1"
render https://localhost:8443 /root/install-tampered.sh
render https://localhost:8443/redir /root/install-redirect.sh
MIRROR_CA="CURL_CA_BUNDLE=/root/tls/cert.pem"

# Every case up to fresh-install needs a host with no agent; assert it so none of them can
# pass because of a leftover from an earlier run.
check clean-host-precondition '! run "getent passwd monitorable || test -e /etc/monitorable/agent.env || test -e /etc/systemd/system/monitorable-agent.service"'

# A host without systemd is refused before ANY side effect. systemd is hidden for this one
# run by a private tmpfs over /run/systemd in its own mount namespace.
rc=$(run_rc "unshare -m sh -c 'mount -t tmpfs tmpfs /run/systemd && exec sh /root/install.sh --endpoint=$ENDPOINT --api-key=$KEY'")
check no-systemd-refused '[ "$rc" = 1 ] && out_has "requires systemd" && ! run "getent passwd monitorable || test -e /opt/monitorable || test -e /etc/monitorable || test -e /etc/systemd/system/monitorable-agent.service"'
run "sh /root/install.sh --uninstall >/dev/null 2>&1"

# An https origin that redirects to http is refused: nothing downloads in cleartext.
rc=$(run_rc "$MIRROR_CA sh /root/install-redirect.sh --endpoint=$ENDPOINT --api-key=$KEY")
check https-redirect-to-http-refused '[ "$rc" = 1 ] && out_has "Failed to download" && ! run "test -e /opt/monitorable/monitorable-agent"'
run "sh /root/install.sh --uninstall >/dev/null 2>&1"

# A checksum mismatch on a fresh install leaves no service user or group behind.
rc=$(run_rc "$MIRROR_CA sh /root/install-tampered.sh --endpoint=$ENDPOINT --api-key=$KEY")
check tampered-leaves-no-user '[ "$rc" = 1 ] && out_has "Checksum mismatch" && ! run "getent passwd monitorable || getent group monitorable"'
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
