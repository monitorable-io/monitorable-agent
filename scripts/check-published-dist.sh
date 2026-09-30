#!/usr/bin/env bash
# check-published-dist.sh <prod|staging>: the off-infra backstop for a published release.
# Walks what `curl … | sh` does, minus the binary downloads: the installer, its key and
# floor, the signed SHA256SUMS, latest.json, the signed config and unit, and a HEAD of each
# binary. One PASS/FAIL line per check, then a RESULT line; exit 0 = pass, 1 = a check
# failed on both passes (one re-check after CHECK_RETRY_DELAY, which absorbs a publish in
# progress), 2 = usage. Spec: platform docs/superpowers/specs/2026-09-28-installer-dist-backstop-design.md
#
# Overrides (tests and the workflow's self-test only): CHECK_INSTALLER_URL, CHECK_BASE_URL
# (where files are fetched from; check 1 still wants the env's canonical BASE_URL),
# CHECK_KEY_FILE, CHECK_RETRY_DELAY (seconds, default 180), CHECK_ALLOW_HTTP=1.
set -uo pipefail # not -e: every check runs and reports

usage() { echo "usage: $0 <prod|staging>" >&2; exit 2; }
[ $# -eq 1 ] || usage
ENV="$1"
case "$ENV" in
  prod)    DEFAULT_INSTALLER_URL="https://get.monitorable.net/install.sh"; CANONICAL_BASE="https://get.monitorable.io" ;;
  staging) DEFAULT_INSTALLER_URL="https://install-mon.ok9k.com/install.sh"; CANONICAL_BASE="https://get-mon.ok9k.com" ;;
  *) usage ;;
esac
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
INSTALLER_URL="${CHECK_INSTALLER_URL:-$DEFAULT_INSTALLER_URL}"
BASE="${CHECK_BASE_URL:-$CANONICAL_BASE}"
KEY_FILE="${CHECK_KEY_FILE:-$ROOT/distribution/keys/$ENV.pub}"
RETRY_DELAY="${CHECK_RETRY_DELAY:-180}"
PROTO="=https"
[ "${CHECK_ALLOW_HTTP:-0}" = 1 ] && PROTO="=http,https"
MIN_BINARY_BYTES=20000000

case "$RETRY_DELAY" in ''|*[!0-9]*) echo "CHECK_RETRY_DELAY must be whole seconds, got: $RETRY_DELAY" >&2; exit 2 ;; esac
[ -r "$KEY_FILE" ] || { echo "key file not readable: $KEY_FILE" >&2; exit 2; }
KEY_B64="$(grep -v -- '-----' "$KEY_FILE" | tr -d '\n')"
KEY_NAME="$(basename "$KEY_FILE")"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# Same comparison as install.sh's version_ge (copied unchanged): true when $1 >= $2, and
# false when either is not vX.Y.Z.
version_ge() {
    for _v in "$1" "$2"; do
        case "$_v" in v?*) ;; *) return 1 ;; esac
        case "${_v#v}" in
            *[!0-9.]*|*.*.*.*|*..*|.*|*.) return 1 ;;
            *[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]*) return 1 ;;
            *.*.*) ;;
            *) return 1 ;;
        esac
    done
    _a="${1#v}"; _b="${2#v}"
    _a1="${_a%%.*}"; _a="${_a#*.}"; _a2="${_a%%.*}"; _a3="${_a#*.}"
    _b1="${_b%%.*}"; _b="${_b#*.}"; _b2="${_b%%.*}"; _b3="${_b#*.}"
    if [ "$_a1" -ne "$_b1" ]; then [ "$_a1" -gt "$_b1" ]; return; fi
    if [ "$_a2" -ne "$_b2" ]; then [ "$_a2" -gt "$_b2" ]; return; fi
    [ "$_a3" -ge "$_b3" ]
}

# --retry-all-errors: plain --retry skips connection resets (curl 35/56), which the edge
# throws now and then. A retry cannot pass a wrong file: every check judges the content.
CURL=(curl --proto "$PROTO" -fsS --max-time 20 --retry 2 --retry-all-errors -A "monitorable-dist-backstop/1")
# fetch <url> <file>: 0 on a 2xx body saved to <file>; otherwise curl's error in $FETCH_ERR.
fetch() {
    FETCH_ERR="$("${CURL[@]}" -o "$2" "$1" 2>&1)" && return 0
    FETCH_ERR="${FETCH_ERR:-curl failed}"
    return 1
}
# content_length <url>: the Content-Length of a HEAD (empty when the HEAD fails).
content_length() {
    "${CURL[@]}" -I "$1" 2>/dev/null | tr -d '\r' | awk 'tolower($1) == "content-length:" { v = $2 } END { print v }'
}
# field <file> <NAME>: the value of a NAME="value" line.
field() { sed -n "s/^$2=\"\(.*\)\"\$/\1/p" "$1"; }

FAILED=()
pass() { echo "PASS $1"; }
fail() { echo "FAIL $1: $2"; FAILED+=("$1"); }

run_pass() {
    FAILED=()
    SIGNED_VERSION=""
    local d="$WORK/pass" have_installer=0 have_sums=0
    rm -rf "$d" && mkdir -p "$d"

    # 1. installer: reachable, and rendered for this env.
    if fetch "$INSTALLER_URL" "$d/install.sh"; then
        have_installer=1
        local base_line
        base_line="$(field "$d/install.sh" BASE_URL)"
        if [ "$base_line" = "$CANONICAL_BASE" ]; then pass installer
        else fail installer "BASE_URL is '$base_line', want '$CANONICAL_BASE'"; fi
    else
        fail installer "$INSTALLER_URL: $FETCH_ERR"
    fi

    # 2. installer-key: the served installer embeds the committed key.
    if [ "$have_installer" = 0 ]; then fail installer-key "no installer to read"
    elif [ "$(field "$d/install.sh" SIGNING_PUBKEY)" = "$KEY_B64" ]; then pass installer-key
    else fail installer-key "SIGNING_PUBKEY is not $KEY_NAME"; fi

    # 3. signature: the served sums verify with the committed key (install.sh's own command).
    if ! fetch "$BASE/binaries/otel/latest/SHA256SUMS" "$d/sums"; then
        fail signature "SHA256SUMS: $FETCH_ERR"
    else
        have_sums=1
        if ! fetch "$BASE/binaries/otel/latest/SHA256SUMS.sig" "$d/sig"; then
            fail signature "SHA256SUMS.sig: $FETCH_ERR"
        elif openssl dgst -sha256 -verify "$KEY_FILE" -signature "$d/sig" "$d/sums" >/dev/null 2>&1; then
            pass signature
        else
            fail signature "SHA256SUMS does not verify with $KEY_NAME"
        fi
    fi

    # 4. version-floor: one version line, not below the installer's MIN_VERSION.
    if [ "$have_sums" = 0 ]; then fail version-floor "no SHA256SUMS"
    else
        local n min
        n="$(grep -c '^# version ' "$d/sums")"
        if [ "$n" -ne 1 ]; then fail version-floor "SHA256SUMS has $n version lines, want 1"
        else
            SIGNED_VERSION="$(sed -n 's/^# version //p' "$d/sums")"
            if [ "$have_installer" = 0 ]; then fail version-floor "no installer to read MIN_VERSION from"
            else
                min="$(field "$d/install.sh" MIN_VERSION)"
                if version_ge "$SIGNED_VERSION" "$min"; then pass version-floor
                else fail version-floor "signed '$SIGNED_VERSION' is not >= the installer's MIN_VERSION '$min'"; fi
            fi
        fi
    fi

    # 5. latest-json: the dashboard's version equals the signed one.
    if ! fetch "$BASE/latest.json" "$d/latest.json"; then fail latest-json "latest.json: $FETCH_ERR"
    else
        local lj
        lj="$(sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$d/latest.json")"
        if [ -z "$SIGNED_VERSION" ]; then fail latest-json "latest.json says '$lj', no signed version to compare"
        elif [ "$lj" = "$SIGNED_VERSION" ]; then pass latest-json
        else fail latest-json "latest.json says '$lj', SHA256SUMS says '$SIGNED_VERSION'"; fi
    fi

    # 6. signed-files: the config and unit hash to their SHA256SUMS lines.
    if [ "$have_sums" = 0 ]; then fail signed-files "no SHA256SUMS"
    else
        local f want got bad=""
        for f in collector-config.yaml monitorable-agent.service; do
            if ! fetch "$BASE/configs/linux/$f" "$d/$f"; then bad+=" $f ($FETCH_ERR)"; continue; fi
            want="$(awk -v n="$f" '$2 == n { print $1 }' "$d/sums")"
            got="$(sha256sum "$d/$f" | cut -d' ' -f1)"
            [ -n "$want" ] && [ "$want" = "$got" ] || bad+=" $f"
        done
        if [ -z "$bad" ]; then pass signed-files; else fail signed-files "not as signed:$bad"; fi
    fi

    # 7. binaries: present and not truncated (hashes not checked: 114 MB per run).
    local arch len badbin=""
    for arch in amd64 arm64; do
        len="$(content_length "$BASE/binaries/otel/latest/monitorable-agent-linux-$arch")"
        case "$len" in ''|*[!0-9]*) badbin+=" $arch (no Content-Length)"; continue ;; esac
        [ "$len" -ge "$MIN_BINARY_BYTES" ] || badbin+=" $arch ($len bytes)"
    done
    if [ -z "$badbin" ]; then pass binaries; else fail binaries "missing or truncated:$badbin"; fi
}

echo "== $ENV: installer $INSTALLER_URL, files $BASE, key $KEY_NAME"
run_pass
if [ "${#FAILED[@]}" -gt 0 ]; then
    echo "== first pass failed (${FAILED[*]}); re-checking in ${RETRY_DELAY}s"
    sleep "$RETRY_DELAY"
    run_pass
fi
if [ "${#FAILED[@]}" -eq 0 ]; then
    echo "RESULT PASS $ENV $SIGNED_VERSION"
    exit 0
fi
summary="$(printf '%s, ' "${FAILED[@]}")"
echo "RESULT FAIL $ENV: ${summary%, }"
exit 1
