#!/bin/sh
# Lifecycle harness for distribution/install.sh. Dev host only.
# Usage: scripts/test-install-lifecycle.sh <lxd-container | ssh:user@host>
#   <lxd-container>: commands run through `lxc exec` (e.g. web-01).
#   ssh:user@host:   commands run over SSH as that (root) user, e.g. ssh:root@172.17.0.241 for
#                    the SELinux VM sel-01, where lxd-agent dies under enforcing. SSH_KEY=<file>
#                    picks the key (with IdentitiesOnly).
# The host must be a networked Ubuntu 24.04 or AlmaLinux 9 that can reach get-mon.ok9k.com
# (the genuine files are copied from there once). Every install then runs against a local
# https mirror (scripts/install-test-mirror.py) serving copies re-signed with a throwaway
# key, so the harness can stage any signature, version or checksum failure.
# shellcheck disable=SC2016,SC2034  # check() strings hold $vars for eval to expand later; rc feeds them the same way.
set -eu
CT="${1:?usage: $0 <lxd-container | ssh:user@host>}"
SRC_URL="${SRC_URL:-https://get-mon.ok9k.com}"
ORIGIN="https://localhost:8443"
KEY="$(printf '%064d' 0 | tr 0 a)"
ENDPOINT="https://ingest-mon.ok9k.com"
MIN="v1.3.0"
FAILS=0

# ct <env assignment | -> <cmd>: runs <cmd> with sh -c on the test host, stdin passed through.
# Over SSH the remote shell parses the command line once more, so <cmd> is single-quoted
# ('\'' for each quote) and reaches sh -c byte for byte, as it does through lxc exec.
case "$CT" in
    ssh:*)
        SSH_DEST="${CT#ssh:}"
        SSH_CTL="$(mktemp -d)"
        # One multiplexed connection for the run's several hundred commands.
        set -- -o ControlMaster=auto -o ControlPath="$SSH_CTL/c" -o ControlPersist=60 -o BatchMode=yes
        if [ -n "${SSH_KEY:-}" ]; then set -- "$@" -i "$SSH_KEY" -o IdentitiesOnly=yes; fi
        SSH_OPTS="$*"
        q() { printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")"; }
        # shellcheck disable=SC2086,SC2029  # SSH_OPTS: whitespace-free options, split on purpose; the command is quoted here, locally.
        ct() { if [ "$1" = - ]; then ssh $SSH_OPTS "$SSH_DEST" "sh -c $(q "$2")"; else ssh $SSH_OPTS "$SSH_DEST" "env $1 sh -c $(q "$2")"; fi; }
        ;;
    *)
        SSH_CTL=
        ct() { if [ "$1" = - ]; then lxc exec "$CT" -- sh -c "$2"; else lxc exec "$CT" --env "$1" -- sh -c "$2"; fi; }
        ;;
esac
# Every harness command trusts the mirror's self-signed certificate (and only it).
run() { ct CURL_CA_BUNDLE=/root/tls/cert.pem "$1"; }
# run_rc <cmd>: prints combined output to /tmp/out on the test host, returns the exit code
run_rc() { ct CURL_CA_BUNDLE=/root/tls/cert.pem "$1 > /tmp/out 2>&1; echo \$?" ; }
# raw: no CA override, for the one-time copy from the real staging origin
raw() { ct - "$1"; }
# The release file name's arch, mapped from uname -m the way install.sh does (no dpkg on RHEL).
ARCH_SH='case $(uname -m) in x86_64) a=amd64 ;; aarch64) a=arm64 ;; *) exit 1 ;; esac'
out_has() { run "grep -qF -- '$1' /tmp/out"; }
pass() { printf 'PASS %s\n' "$1"; }
fail() { printf 'FAIL %s\n' "$1"; run 'tail -n 20 /tmp/out' || true; FAILS=$((FAILS + 1)); }
check() { if eval "$2"; then pass "$1"; else fail "$1"; fi; }
# Update rollback evidence (spec 2026-09-27 §6). The binary is compared by inode: the good and
# broken mirrors ship the same bytes, and the hard-link restore brings back the exact
# pre-update inode. The config, agent.env and unit are compared by sha256.
state() { run "stat -c %i /opt/monitorable/monitorable-agent && sha256sum /etc/monitorable/collector-config.yaml /etc/monitorable/agent.env /etc/systemd/system/monitorable-agent.service | cut -d' ' -f1" | tr '\n' ' '; }
no_prev() { ! run "test -e /opt/monitorable/monitorable-agent.prev || test -e /etc/monitorable/collector-config.yaml.prev || test -e /etc/monitorable/agent.env.prev || test -e /etc/monitorable/monitorable-agent.service.prev"; }
# install.sh snapshots only an agent whose main process has been up >= 10s; wait past that.
settle() { sleep 12; }

# SELinux denials are collected from here on (the no-selinux-denials case at the end).
# ausearch -ts takes the date in the locale's %x format, so both sides run under LC_ALL=C.
AVC_SINCE="$(raw "LC_ALL=C date '+%x %T'")"

# Genuine files, TLS certificate and throwaway signing key.
raw "rm -rf /root/mirrors /root/tls && mkdir -p /root/tls /root/mirrors/src/configs/linux"
raw "$ARCH_SH && cd /root/mirrors/src && curl -fsS -o monitorable-agent-linux-\$a $SRC_URL/binaries/otel/latest/monitorable-agent-linux-\$a && curl -fsS -o SHA256SUMS $SRC_URL/binaries/otel/latest/SHA256SUMS && curl -fsS -o configs/linux/collector-config.yaml $SRC_URL/configs/linux/collector-config.yaml && curl -fsS -o configs/linux/monitorable-agent.service $SRC_URL/configs/linux/monitorable-agent.service"
raw "openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 1 -subj /CN=localhost -addext subjectAltName=DNS:localhost -keyout /root/tls/key.pem -out /root/tls/cert.pem >/dev/null 2>&1"
raw "openssl ecparam -name prime256v1 -genkey -noout -out /root/tls/sign.key"
# A second key the installer does not embed: signs the genuine sums for the "valid
# signature, wrong key" case.
raw "openssl ecparam -name prime256v1 -genkey -noout -out /root/tls/other.key"
TEST_PUB="$(raw "openssl pkey -in /root/tls/sign.key -pubout" | grep -v -- '-----' | tr -d '\n')"
raw "cat > /root/install-test-mirror.py" < scripts/install-test-mirror.py
raw "cat > /root/mirror-build.sh" < scripts/install-test-mirror-build.sh
trap 'raw "systemctl stop install-test-mirror >/dev/null 2>&1" || true; if [ -n "$SSH_CTL" ]; then ssh -o ControlPath="$SSH_CTL/c" -O exit "$SSH_DEST" 2>/dev/null; rm -rf "$SSH_CTL"; fi' EXIT
run "systemctl stop install-test-mirror >/dev/null 2>&1; systemd-run --quiet --collect --unit=install-test-mirror python3 /root/install-test-mirror.py"

# render <base url> <path in container> [<min version> [<signing key>]]: fill the
# placeholders the way publish-dist.sh does.
render() {
    sed -e "s|@@BASE_URL@@|$1|g" -e "s|@@SIGNING_PUBKEY@@|${4:-$TEST_PUB}|g" -e "s|@@MIN_VERSION@@|${3:-$MIN}|g" \
        distribution/install.sh | raw "cat > $2"
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
raw "cat > /root/install-unrendered.sh" < distribution/install.sh
rc=$(run_rc "sh /root/install-unrendered.sh --endpoint=$ENDPOINT --api-key=$KEY")
check unrendered-template-refused '[ "$rc" = 1 ] && out_has "served unrendered" && ! run "test -e /opt/monitorable"'

# No openssl: refused before any side effect. The binary is hidden for this one run by a
# bind mount of /dev/null in a private mount namespace; the inner `! command -v` is the
# precondition, so the case cannot pass without openssl actually being hidden.
rc=$(run_rc "unshare -m sh -c 'mount --bind /dev/null \"\$(command -v openssl)\" && ! command -v openssl >/dev/null && exec sh /root/install.sh --endpoint=$ENDPOINT --api-key=$KEY'")
check no-openssl-refused '[ "$rc" = 1 ] && out_has "openssl is required" && ! run "test -e /opt/monitorable || getent passwd monitorable"'
run "sh /root/install.sh --uninstall >/dev/null 2>&1"

# No fold (it wraps the embedded key into PEM lines): refused before any side effect, the
# same way as openssl above.
rc=$(run_rc "unshare -m sh -c 'mount --bind /dev/null \"\$(command -v fold)\" && ! command -v fold >/dev/null && exec sh /root/install.sh --endpoint=$ENDPOINT --api-key=$KEY'")
check no-fold-refused '[ "$rc" = 1 ] && out_has "fold (coreutils) is required" && ! run "test -e /opt/monitorable || getent passwd monitorable"'
run "sh /root/install.sh --uninstall >/dev/null 2>&1"

# The embedded key must be an ECDSA P-256 one: an Ed25519 key, or a P-256 key cut short,
# is refused before any side effect instead of surfacing as a failed signature.
ED_PUB="$(raw "openssl genpkey -algorithm ed25519 | openssl pkey -pubout" | grep -v -- '-----' | tr -d '\n')"
render "$ORIGIN/good" /root/install-ed25519.sh "$MIN" "$ED_PUB"
render "$ORIGIN/good" /root/install-shortkey.sh "$MIN" "${TEST_PUB%????}"
rc=$(run_rc "sh /root/install-ed25519.sh --endpoint=$ENDPOINT --api-key=$KEY")
check non-p256-key-refused '[ -n "$ED_PUB" ] && [ "$rc" = 1 ] && out_has "not an ECDSA P-256 public key" && ! run "test -e /opt/monitorable"'
run "sh /root/install.sh --uninstall >/dev/null 2>&1"
rc=$(run_rc "sh /root/install-shortkey.sh --endpoint=$ENDPOINT --api-key=$KEY")
check short-p256-key-refused '[ "$rc" = 1 ] && out_has "not an ECDSA P-256 public key" && ! run "test -e /opt/monitorable"'
run "sh /root/install.sh --uninstall >/dev/null 2>&1"

# A rendered MIN_VERSION that is not vX.Y.Z is refused before any side effect, never
# reported as "older than this installer's minimum".
render "$ORIGIN/good" /root/install-badmin.sh v1.3
rc=$(run_rc "sh /root/install-badmin.sh --endpoint=$ENDPOINT --api-key=$KEY")
check malformed-min-version-refused '[ "$rc" = 1 ] && out_has "malformed minimum version" && ! out_has "older than" && ! run "test -e /opt/monitorable"'
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

# A --version that is neither latest nor vX.Y.Z is refused before any side effect: a
# path-shaped one would otherwise resolve to another directory on the server (curl
# normalizes "otel/../latest" to "latest"), and a bare 1.3.0 to one that does not exist.
rc=$(run_rc "sh /root/install.sh --version=../latest --endpoint=$ENDPOINT --api-key=$KEY")
check dotdot-version-refused '[ "$rc" = 1 ] && out_has "--version must be latest or vX.Y.Z" && ! run "test -e /opt/monitorable"'
rc=$(run_rc "sh /root/install.sh --version=1.3.0 --endpoint=$ENDPOINT --api-key=$KEY")
check unprefixed-version-refused '[ "$rc" = 1 ] && out_has "--version must be latest or vX.Y.Z" && ! run "test -e /opt/monitorable"'
# An explicit --version=latest is the default, not a pin.
rc=$(run_rc "sh /root/install.sh --version=latest --endpoint=$ENDPOINT --api-key=$KEY")
check explicit-latest-installs '[ "$rc" = 0 ] && run "systemctl is-active --quiet monitorable-agent"'
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

# A signed version line with a CRLF ending (a sums file edited on Windows) is refused, never
# read as v1.3.0: the \r makes it malformed, and a malformed version fails closed.
run "sh /root/mirror-build.sh crlfversion crlf ok ok"
render "$ORIGIN/crlfversion" /root/install-crlfversion.sh
rc=$(run_rc "sh /root/install-crlfversion.sh --endpoint=$ENDPOINT --api-key=$KEY")
check crlf-version-line-refused '[ "$rc" = 1 ] && out_has "refusing" && ! run "test -e /opt/monitorable/monitorable-agent"'
run "sh /root/install.sh --uninstall >/dev/null 2>&1"

# Leading zeros compare as decimal (never octal, never as text): v1.02.9 is below the
# v1.3.0 floor and is refused.
run "sh /root/mirror-build.sh zeroversion v1.02.9 ok ok"
render "$ORIGIN/zeroversion" /root/install-zeroversion.sh
rc=$(run_rc "sh /root/install-zeroversion.sh --endpoint=$ENDPOINT --api-key=$KEY")
check leading-zero-below-floor-refused '[ "$rc" = 1 ] && out_has "older than this installer" && ! run "test -e /opt/monitorable/monitorable-agent"'
run "sh /root/install.sh --uninstall >/dev/null 2>&1"

# A stale download left in a pre-existing, non-root-owned /opt/monitorable is never reused:
# curl -o would write into that inode (still owned by its planter, who could rewrite it
# after verification) and the rename would install it. The installed binary must be a new,
# root-owned file.
# (nobody: = nobody's login group: nogroup on Ubuntu, nobody on AlmaLinux.)
run "$ARCH_SH && mkdir -p /opt/monitorable && chown nobody: /opt/monitorable && printf planted > /opt/monitorable/monitorable-agent-linux-\$a.tmp && chown nobody: /opt/monitorable/monitorable-agent-linux-\$a.tmp"
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

# key-free update keeps key + endpoint; a healthy agent is snapshotted, and the snapshot is
# gone once the update stays up
settle
rc=$(run_rc "sh /root/install.sh")
check keyfree-update '[ "$rc" = 0 ] && out_has "Updating the existing agent" && out_has "Saved the running agent for rollback" && no_prev && run "grep -qx MONITORABLE_API_KEY=$KEY /etc/monitorable/agent.env && grep -qx MONITORABLE_ENDPOINT=$ENDPOINT /etc/monitorable/agent.env"'

# --- Update rollback (spec docs/superpowers/specs/2026-09-27-update-rollback-design.md) ---
# A validly signed v1.3.1 whose config the collector refuses: every signature, version and
# checksum check passes, then the agent exits on start.
run "sh /root/mirror-build.sh broken v1.3.1 ok ok latest broken"
render "$ORIGIN/broken" /root/install-broken.sh

# A healthy agent + a failed update: the full snapshot is restored (same binary inode,
# and the same config, unit and agent.env even though the broken release changes the
# config and unit and this run also changes the endpoint) and
# the run still exits 1.
settle
BEFORE="$(state)"
rc=$(run_rc "sh /root/install-broken.sh --endpoint=https://ingest-mon.ok9k.com/")
# The endpoint this run asked for is dropped with the rest of the new release, and the
# output says so.
check update-rolls-back '[ "$rc" = 1 ] && out_has "Saved the running agent for rollback" && out_has "restoring the previous agent" && out_has "previous agent is restored and running" && out_has "was not applied" && run "systemctl is-active --quiet monitorable-agent" && [ "$(state)" = "$BEFORE" ] && no_prev'
# On an SELinux host the restored unit carries the policy's default type, not the etc_t it
# picked up as a snapshot in /etc/monitorable. No SELinux (this Ubuntu container) = vacuous.
check update-rolls-back-unit-label 'run "! command -v selinuxenabled >/dev/null 2>&1 || ! selinuxenabled || [ \"\$(stat -c %C /etc/systemd/system/monitorable-agent.service | cut -d: -f3)\" = \"\$(matchpathcon -n /etc/systemd/system/monitorable-agent.service | cut -d: -f3)\" ]"'

# The unconditional clear fails (a directory where a snapshot file goes): exit 1 before any
# rename, the old agent untouched.
settle
BEFORE="$(state)"
run "mkdir /etc/monitorable/agent.env.prev"
rc=$(run_rc "sh /root/install-broken.sh")
check snapshot-failure-untouched '[ "$rc" = 1 ] && out_has "Could not clear the old rollback snapshot" && out_has "the existing agent is untouched" && run "systemctl is-active --quiet monitorable-agent" && [ "$(state)" = "$BEFORE" ]'
run "rmdir /etc/monitorable/agent.env.prev"

# The snapshot copy fails partway (a full /etc/monitorable): the binary's hard link is
# already made when the config copy hits ENOSPC. Exit 1 before any rename, the partial
# snapshot removed, the old agent untouched. A tmpfs stands in for a full disk: room for
# the current files plus exactly one download of the broken release's config (install.sh
# stages that one download, collector-config.yaml.tmp, in /etc/monitorable), so the
# download fits and the snapshot's config copy is the first write that doesn't. This
# unprivileged container may mount tmpfs (the uninstall-rm-failure case below does too).
settle
BEFORE="$(state)"
run "rm -rf /root/etcmon && cp -a /etc/monitorable /root/etcmon && b=0 && for f in /root/etcmon/*; do b=\$((b + (\$(stat -c %s \"\$f\") + 4095) / 4096)); done && c=\$(( (\$(stat -c %s /root/mirrors/broken/configs/linux/collector-config.yaml) + 4095) / 4096 )) && mount -t tmpfs -o nr_blocks=\$((b + c)),mode=\$(stat -c %a /root/etcmon),uid=0,gid=0 tmpfs /etc/monitorable && cp -a /root/etcmon/. /etc/monitorable/"
# Exactly one broken-config download fits, and nothing after it.
check tmpfs-full-precondition 'run "cmp -s /root/etcmon/agent.env /etc/monitorable/agent.env && cp /root/mirrors/broken/configs/linux/collector-config.yaml /etc/monitorable/probe1 && ! cp /root/etcmon/agent.env /etc/monitorable/probe2 2>/dev/null"'
run "rm -f /etc/monitorable/probe1 /etc/monitorable/probe2"
rc=$(run_rc "sh /root/install-broken.sh")
check snapshot-save-failure-untouched '[ "$rc" = 1 ] && out_has "Could not save the running agent for rollback" && out_has "the existing agent is untouched" && run "systemctl is-active --quiet monitorable-agent" && no_prev'
# Compared while the tmpfs is still mounted: it holds the only config and agent.env this
# run could have touched (after the umount the untouched originals underneath show).
check snapshot-save-failure-state-mounted '[ "$(state)" = "$BEFORE" ]'
run "umount /etc/monitorable"
check snapshot-save-failure-state '[ "$(state)" = "$BEFORE" ]'

# The restored agent fails too: a test-only drop-in (not snapshotted, so it survives the
# rollback) fails ExecStartPre once /run/mon-fail exists. "+" runs it outside the unit's
# sandbox (NoExecPaths=/ would otherwise block /bin/sh).
run "mkdir -p /etc/systemd/system/monitorable-agent.service.d && printf '[Service]\nExecStartPre=+/bin/sh -c \"! test -e /run/mon-fail\"\n' > /etc/systemd/system/monitorable-agent.service.d/rollback-test.conf && systemctl daemon-reload && systemctl restart monitorable-agent"
settle
check rollback-also-fails-precondition 'run "systemctl is-active --quiet monitorable-agent"'
BEFORE="$(state)"
run "touch /run/mon-fail"
rc=$(run_rc "sh /root/install-broken.sh")
# The restored files stay in place (spec §4.1); a key-free run changed no agent.env, so
# there's no "not applied" note.
check rollback-also-fails '[ "$rc" = 1 ] && out_has "Saved the running agent for rollback" && out_has "did not stay up either" && ! out_has "previous agent is restored and running" && ! out_has "was not applied" && [ "$(state)" = "$BEFORE" ] && no_prev'
run "rm -rf /etc/systemd/system/monitorable-agent.service.d /run/mon-fail && systemctl daemon-reload"
rc=$(run_rc "sh /root/install.sh")
check reinstall-after-rollback-failure '[ "$rc" = 0 ] && run "systemctl is-active --quiet monitorable-agent"'

# An agent up for less than 10s is not "healthy": no snapshot, today's failure.
run "systemctl restart monitorable-agent"
rc=$(run_rc "sh /root/install-broken.sh")
check young-agent-no-rollback '[ "$rc" = 1 ] && out_has "failed to start and is restarting in a loop" && ! out_has "Saved the running agent" && ! out_has "restoring" && no_prev'
rc=$(run_rc "sh /root/install.sh")
check reinstall-after-young '[ "$rc" = 0 ] && run "systemctl is-active --quiet monitorable-agent"'

# A stopped agent (MainPID=0) is not "healthy" either: the broken config stays live.
settle
run "systemctl stop monitorable-agent"
rc=$(run_rc "sh /root/install-broken.sh")
check unhealthy-no-rollback '[ "$rc" = 1 ] && out_has "failed to start and is restarting in a loop" && ! out_has "Saved the running agent" && ! out_has "restoring" && run "grep -q monitorable_rollback_test /etc/monitorable/collector-config.yaml" && no_prev'

# Leftover snapshots from an interrupted run are cleared even when no new snapshot is
# taken (the agent is stopped), so a later run can never restore them.
run "systemctl stop monitorable-agent && for p in /opt/monitorable/monitorable-agent.prev /etc/monitorable/collector-config.yaml.prev /etc/monitorable/agent.env.prev /etc/monitorable/monitorable-agent.service.prev; do echo junk > \$p; done"
rc=$(run_rc "sh /root/install.sh")
check stale-prev-cleared '[ "$rc" = 0 ] && ! out_has "Saved the running agent" && no_prev && run "systemctl is-active --quiet monitorable-agent"'

# A run killed after the binary rename but before the restart leaves the OLD process
# running while the NEW binary sits on disk. That process isn't "healthy" for a snapshot:
# the snapshot would hold the new files, not what is running. Simulated by swapping the
# on-disk binary for a copy (a new inode) under the running agent.
settle
run "cp /opt/monitorable/monitorable-agent /opt/monitorable/agent.copy && mv -f /opt/monitorable/agent.copy /opt/monitorable/monitorable-agent"
rc=$(run_rc "sh /root/install-broken.sh")
check stale-running-binary-no-snapshot '[ "$rc" = 1 ] && ! out_has "Saved the running agent" && ! out_has "restoring" && no_prev'
rc=$(run_rc "sh /root/install.sh")
check reinstall-after-stale-binary '[ "$rc" = 0 ] && run "systemctl is-active --quiet monitorable-agent"'

# `systemctl enable` fails after the renames (multi-user.target.wants is a file, not a
# directory): an update failure like a failed restart, so a snapshot is rolled back
# instead of the script dying under set -e with the new files on disk. The restored agent
# restarts fine (restart doesn't need enable). A key-free run: nothing to report as not
# applied.
settle
BEFORE="$(state)"
run "mv /etc/systemd/system/multi-user.target.wants /root/wants.bak && touch /etc/systemd/system/multi-user.target.wants"
check enable-failure-precondition '! run "systemctl enable monitorable-agent >/dev/null 2>&1"'
rc=$(run_rc "sh /root/install.sh")
check enable-failure-rolls-back '[ "$rc" = 1 ] && out_has "Saved the running agent for rollback" && out_has "previous agent is restored and running" && ! out_has "was not applied" && run "systemctl is-active --quiet monitorable-agent" && [ "$(state)" = "$BEFORE" ] && no_prev'
run "rm -f /etc/systemd/system/multi-user.target.wants && mv /root/wants.bak /etc/systemd/system/multi-user.target.wants && systemctl daemon-reload"

# A restart that fails outright with no snapshot (the agent was stopped) says so, instead
# of "restarting in a loop".
run "mkdir -p /etc/systemd/system/monitorable-agent.service.d && printf '[Service]\nExecStartPre=+/bin/sh -c \"! test -e /run/mon-fail\"\n' > /etc/systemd/system/monitorable-agent.service.d/rollback-test.conf && touch /run/mon-fail && systemctl daemon-reload && systemctl stop monitorable-agent"
rc=$(run_rc "sh /root/install.sh")
check restart-failure-message '[ "$rc" = 1 ] && out_has "systemd could not start the agent" && ! out_has "restarting in a loop" && ! out_has "Saved the running agent"'
run "rm -rf /etc/systemd/system/monitorable-agent.service.d /run/mon-fail && systemctl daemon-reload"
rc=$(run_rc "sh /root/install.sh")
check reinstall-after-restart-failure '[ "$rc" = 0 ] && run "systemctl is-active --quiet monitorable-agent"'

# A healthy current agent next to a leftover pre-v1.2.0 unit file takes no snapshot (spec
# §3.1): the legacy migration deletes files after the renames. This is the case where only
# the legacy-unit guard, not a missing agent, keeps the snapshot off.
settle
run "printf '[Service]\nExecStart=/bin/sleep infinity\n' > /etc/systemd/system/monitorable-collector.service && systemctl daemon-reload"
rc=$(run_rc "sh /root/install.sh")
check keyfree-legacy-healthy-no-snapshot '[ "$rc" = 0 ] && ! out_has "Saved the running agent" && no_prev && ! run "test -e /etc/systemd/system/monitorable-collector.service" && run "systemctl is-active --quiet monitorable-agent"'

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
check keyfree-legacy-unit '[ "$rc" = 0 ] && out_has "Updating the existing agent" && out_has "pre-v1.2.0" && run "grep -qx MONITORABLE_API_KEY=$KEY /etc/monitorable/agent.env && grep -qx MONITORABLE_ENDPOINT=$ENDPOINT /etc/monitorable/agent.env && systemctl is-active --quiet monitorable-agent && ! test -e /etc/systemd/system/monitorable-collector.service && ! systemctl is-active --quiet monitorable-collector" && ! out_has "Saved the running agent" && no_prev'
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

# Under SELinux the whole run left no AVC or USER_AVC denial. "<no matches>" is required
# explicitly: ausearch also exits 1 on a bad -ts or a missing log, which must not pass.
# --input-logs: without a tty (ssh, lxc exec) ausearch reads records from stdin instead of
# the log files and finds nothing, so the check would pass vacuously. A just-booted LXD VM
# logs lxd-agent vsock_socket denials for about a minute, so start the run after that.
# No SELinux (this Ubuntu container) = vacuous.
check no-selinux-denials 'run "! command -v selinuxenabled >/dev/null 2>&1 || ! selinuxenabled || LC_ALL=C ausearch --input-logs -m avc,user_avc -ts $AVC_SINCE 2>&1 | grep -qx \"<no matches>\""'

printf '%s failure(s)\n' "$FAILS"
[ "$FAILS" -eq 0 ]
