#!/bin/sh
# Test-only mirror builder for scripts/test-install-lifecycle.sh. Runs INSIDE the LXD
# container: builds /root/mirrors/<name>/ from the genuine staging files in
# /root/mirrors/src, signed with the harness's throwaway key /root/tls/sign.key.
# Usage: mirror-build.sh <name> <version> <ok|tampered> <ok|bad|none> [<dir>]
#   <version>: vX.Y.Z written as "# version vX.Y.Z"; "-" writes no version line; "dup"
#              writes two ("v1.3.0" and "v9.9.9")
#   binary:    ok = genuine; tampered = one byte appended
#   signature: ok = valid; bad = a valid signature over OTHER bytes; none = no .sig
#   <dir>:     binaries/otel/<dir>/ (default latest). Other dirs of <name> are kept.
set -eu
name=$1 ver=$2 bin=$3 sig=$4 dir=${5:-latest}
arch=$(dpkg --print-architecture)
m=/root/mirrors/$name
d=$m/binaries/otel/$dir
rm -rf "$d"
mkdir -p "$d" "$m/configs/linux"
cp /root/mirrors/src/configs/linux/* "$m/configs/linux/"
cp "/root/mirrors/src/monitorable-agent-linux-$arch" "$d/"
if [ "$bin" = tampered ]; then printf x >> "$d/monitorable-agent-linux-$arch"; fi
{
    case "$ver" in
        -) ;;
        dup) printf '# version v1.3.0\n# version v9.9.9\n' ;;
        *) printf '# version %s\n' "$ver" ;;
    esac
    # Staging's own sums already carry a version line once it publishes signed releases.
    grep -v '^# version ' /root/mirrors/src/SHA256SUMS
} > "$d/SHA256SUMS"
case "$sig" in
    ok) openssl dgst -sha256 -sign /root/tls/sign.key -out "$d/SHA256SUMS.sig" "$d/SHA256SUMS" ;;
    bad) printf 'other bytes' | openssl dgst -sha256 -sign /root/tls/sign.key -out "$d/SHA256SUMS.sig" ;;
    none) ;;
esac
