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

render() { sed "s|@@BASE_URL@@|$BASE_URL|g" distribution/install.sh | lxc exec "$CT" -- sh -c 'cat > /root/install.sh'; }
run() { lxc exec "$CT" -- sh -c "$1"; }
# run_rc <cmd>: prints combined output to /tmp/out in the container, returns the exit code
run_rc() { lxc exec "$CT" -- sh -c "$1 > /tmp/out 2>&1; echo \$?" ; }
out_has() { run "grep -qF -- '$1' /tmp/out"; }
pass() { printf 'PASS %s\n' "$1"; }
fail() { printf 'FAIL %s\n' "$1"; run 'tail -n 20 /tmp/out' || true; FAILS=$((FAILS + 1)); }
check() { if eval "$2"; then pass "$1"; else fail "$1"; fi; }

render

# fresh install with a key
rc=$(run_rc "sh /root/install.sh --endpoint=$ENDPOINT --api-key=$KEY")
check fresh-install '[ "$rc" = 0 ] && run "systemctl is-active --quiet monitorable-agent"'

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
rc=$(run_rc "sh /root/install.sh --uninstall")
check uninstall-failed-unit '[ "$rc" = 0 ] && out_has "Uninstalled. Now open the dashboard"'
check uninstall-leaves-nothing '! run "test -e /opt/monitorable || test -e /etc/monitorable || test -e /var/lib/monitorable || test -e /var/log/monitorable || test -e /etc/systemd/system/monitorable-agent.service || test -e /etc/udev/rules.d/99-monitorable-nvme-smart.rules || getent passwd monitorable || getent group monitorable"'

# second --uninstall is a no-op
rc=$(run_rc "sh /root/install.sh --uninstall")
check uninstall-idempotent '[ "$rc" = 0 ] && out_has "Nothing to remove"'

# key-free run on a clean host
rc=$(run_rc "sh /root/install.sh")
check keyfree-on-clean-host '[ "$rc" = 1 ] && out_has "No agent is installed on this server"'

printf '%s failure(s)\n' "$FAILS"
[ "$FAILS" -eq 0 ]
