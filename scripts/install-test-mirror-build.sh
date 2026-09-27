#!/bin/sh
# Test-only mirror builder for scripts/test-install-lifecycle.sh. Runs INSIDE the LXD
# container: builds /root/mirrors/<name>/ from the genuine staging files in
# /root/mirrors/src, signed with the harness's throwaway key /root/tls/sign.key.
# Usage: mirror-build.sh <name> <version> <ok|tampered> <ok|bad|otherkey|empty|none> [<dir> [<ok|broken>]]
#   <version>: vX.Y.Z written as "# version vX.Y.Z"; "-" writes no version line; "dup"
#              writes two ("v1.3.0" and "v9.9.9")
#   binary:    ok = genuine; tampered = one byte appended
#   signature: ok = valid; bad = a valid signature over OTHER bytes; otherkey = a valid
#              signature over THESE sums by a different key (/root/tls/other.key, the
#              realistic attacker); empty = a zero-byte .sig; none = no .sig
#   <dir>:     binaries/otel/<dir>/ (default latest). Other dirs of <name> are kept.
#   config:    ok = genuine; broken = an unknown top-level key appended, so the release is
#              validly signed but the collector refuses to start on it (update rollback).
#              Its SHA256SUMS line is always recomputed from the served file.
set -eu
name=$1 ver=$2 bin=$3 sig=$4 dir=${5:-latest} cfg=${6:-ok}
arch=$(dpkg --print-architecture)
m=/root/mirrors/$name
d=$m/binaries/otel/$dir
rm -rf "$d"
mkdir -p "$d" "$m/configs/linux"
cp /root/mirrors/src/configs/linux/* "$m/configs/linux/"
case "$cfg" in
    ok) ;;
    broken) printf 'monitorable_rollback_test: 1\n' >> "$m/configs/linux/collector-config.yaml" ;;
    *) echo "mirror-build: unknown config mode '$cfg'" >&2; exit 1 ;;
esac
cfg_sha=$(sha256sum "$m/configs/linux/collector-config.yaml" | cut -d' ' -f1)
cp "/root/mirrors/src/monitorable-agent-linux-$arch" "$d/"
if [ "$bin" = tampered ]; then printf x >> "$d/monitorable-agent-linux-$arch"; fi
{
    case "$ver" in
        -) ;;
        dup) printf '# version v1.3.0\n# version v9.9.9\n' ;;
        *) printf '# version %s\n' "$ver" ;;
    esac
    # Staging's own sums already carry a version line once it publishes signed releases.
    grep -v '^# version ' /root/mirrors/src/SHA256SUMS |
        awk -v s="$cfg_sha" '$2 == "collector-config.yaml" { print s "  " $2; next } { print }'
} > "$d/SHA256SUMS"
case "$sig" in
    ok) openssl dgst -sha256 -sign /root/tls/sign.key -out "$d/SHA256SUMS.sig" "$d/SHA256SUMS" ;;
    bad) printf 'other bytes' | openssl dgst -sha256 -sign /root/tls/sign.key -out "$d/SHA256SUMS.sig" ;;
    otherkey) openssl dgst -sha256 -sign /root/tls/other.key -out "$d/SHA256SUMS.sig" "$d/SHA256SUMS" ;;
    empty) : > "$d/SHA256SUMS.sig" ;;
    none) ;;
    *) echo "mirror-build: unknown signature mode '$sig'" >&2; exit 1 ;;
esac
