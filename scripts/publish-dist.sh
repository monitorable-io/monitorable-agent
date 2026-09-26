#!/usr/bin/env bash
# Build Monitorable agent binaries, render install.sh for the target env, publish the full
# distribution (install.sh + configs + binaries) to that env's R2 bucket, and upload the
# rendered installer to that env's Bunny installer zone.
#
# Usage: scripts/publish-dist.sh <staging|prod> [--render-only|--prune-only|--preflight|--check-installer]
#   --render-only     : build + render into ./distribution-build, skip R2/Bunny upload
#                       (CI dry-run / local test; no Cloudflare or Bunny creds needed).
#   --prune-only      : enforce R2 retention only (delete old versioned binaries), no build/publish
#   --preflight       : prove the credentials can write this env's R2 bucket AND its Bunny
#                       installer zone (put + delete one tiny object in each) and exit; no
#                       build. The release jobs run this first.
#   --check-installer : check whether this env's installer host already serves an installer
#                       that embeds this env's signing key; a manual pre-cutover check (it no
#                       longer gates STUB_INSTALLER — see distribution/README.md); no build, no key.
#
# VERSION (env) must be a plain release tag vX.Y.Z for a build (the release job passes the
# pushed tag); --preflight, --prune-only and --check-installer do not use it.
#
# Credentials (env): CLOUDFLARE_R2_TOKEN = an R2 API token with "Object Read & Write"
# scoped to THIS env's bucket only, and CLOUDFLARE_ACCOUNT_ID. Uploads go over R2's S3
# API with the AWS CLI; the S3 credentials are derived from the token as Cloudflare
# documents (Access Key ID = the token's id, Secret Access Key = SHA-256 of its value),
# so no second secret exists. Bucket-scoped tokens are exactly what the Cloudflare REST
# API (wrangler) does NOT accept — it needs the account-wide "Admin Read & Write" — which
# is why this script does not use wrangler (2026-09-22).
# BUNNY_STORAGE_PASSWORD = the storage-zone password of this env's installer zone (write
# access to that zone only).
set -euo pipefail

ENV="${1:-}"
MODE="${2:-}"
VERSION="${VERSION:-dev}"

# R2_JURISDICTION: the bucket's jurisdiction ("" = default, "eu"). A jurisdiction-
# restricted bucket is reachable ONLY through its own S3 endpoint
# (<account>.<jurisdiction>.r2.cloudflarestorage.com); on the default endpoint it does
# not exist for the token — R2 answers AccessDenied, not NoSuchBucket (prod, 2026-09-22).
# The dashboard's token page lists the endpoints its buckets need.
# since v1.3.2 both installer hosts serve install.sh; R2 carries the stub, put only after
# this run verified the installer host
case "$ENV" in
  staging) BASE_URL="https://get-mon.ok9k.com"; BUCKET="monitorable-get-staging"; R2_JURISDICTION="${R2_JURISDICTION-}"
           INSTALLER_URL="${INSTALLER_URL:-https://install-mon.ok9k.com}"; STUB_INSTALLER="${STUB_INSTALLER:-1}"
           BUNNY_STORAGE_HOST="${BUNNY_STORAGE_HOST:-storage.bunnycdn.com}"; BUNNY_STORAGE_ZONE="${BUNNY_STORAGE_ZONE:-mon-staging}" ;;
  prod)    BASE_URL="https://get.monitorable.io"; BUCKET="monitorable-get-prod";   R2_JURISDICTION="${R2_JURISDICTION-eu}"
           INSTALLER_URL="${INSTALLER_URL:-https://get.monitorable.net}"; STUB_INSTALLER="${STUB_INSTALLER:-1}"
           BUNNY_STORAGE_HOST="${BUNNY_STORAGE_HOST:-storage.bunnycdn.com}"; BUNNY_STORAGE_ZONE="${BUNNY_STORAGE_ZONE:-mon-prod}" ;;
  *) echo "usage: $0 <staging|prod> [--render-only|--prune-only|--preflight|--check-installer]" >&2; exit 1 ;;
esac

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SRC="$ROOT/distribution"
OUT="$ROOT/distribution-build"
BINOUT="$OUT/binaries/otel"

# --- R2 access over the S3 API (AWS CLI, preinstalled on GitHub runners) ---
S3_ENDPOINT=""
r2_auth() {
  local token="${CLOUDFLARE_R2_TOKEN:-${CLOUDFLARE_API_TOKEN:-}}" verify id
  if [ -z "$token" ] || [ -z "${CLOUDFLARE_ACCOUNT_ID:-}" ]; then
    echo "ERROR: CLOUDFLARE_R2_TOKEN and CLOUDFLARE_ACCOUNT_ID must be set" >&2; exit 1
  fi
  command -v aws >/dev/null || { echo "ERROR: aws CLI not found" >&2; exit 1; }
  # The token id doubles as the S3 Access Key ID; verify answers it (user-owned tokens on
  # /user/tokens/verify, account-owned ones on /accounts/<id>/tokens/verify) and doubles
  # as the "is this a valid token at all" check. Prints status + id tail only.
  for verify in "user/tokens/verify" "accounts/${CLOUDFLARE_ACCOUNT_ID}/tokens/verify"; do
    id=$(curl -fsS -H "Authorization: Bearer $token" "https://api.cloudflare.com/client/v4/$verify" 2>/dev/null \
      | python3 -c 'import json,sys; d=json.load(sys.stdin); r=d.get("result") or {}; print(r["id"] if d.get("success") and r.get("status")=="active" else "")' 2>/dev/null || true)
    [ -n "$id" ] && break
  done
  if [ -z "$id" ]; then
    echo "ERROR: Cloudflare does not accept CLOUDFLARE_R2_TOKEN as an active API token (length ${#token})." >&2
    echo "       Re-create it under R2 → Manage R2 API Tokens and store its 'Token value'." >&2
    exit 1
  fi
  echo "r2: token active (id …${id: -6}), account …${CLOUDFLARE_ACCOUNT_ID: -4}, bucket ${BUCKET}"
  export AWS_ACCESS_KEY_ID="$id"
  AWS_SECRET_ACCESS_KEY="$(printf '%s' "$token" | sha256sum | cut -d' ' -f1)"
  export AWS_SECRET_ACCESS_KEY
  export AWS_DEFAULT_REGION=auto AWS_EC2_METADATA_DISABLED=true
  # Newer AWS CLIs add CRC checksum trailers by default; keep to what R2 accepts.
  export AWS_REQUEST_CHECKSUM_CALCULATION=when_required AWS_RESPONSE_CHECKSUM_VALIDATION=when_required
  S3_ENDPOINT="https://${CLOUDFLARE_ACCOUNT_ID}${R2_JURISDICTION:+.$R2_JURISDICTION}.r2.cloudflarestorage.com"
  echo "r2: endpoint …${S3_ENDPOINT#https://????????????????????????????????}"
}
s3() { aws --endpoint-url "$S3_ENDPOINT" "$@"; }
# Delete is idempotent on S3 (a missing key still returns 204), so a failure here is a
# real one (auth, network), never "already gone".
r2_delete() { s3 s3api delete-object --bucket "$BUCKET" --key "$1" >/dev/null; }

# --- Bunny Storage: the installer host's origin (spec 2026-09-26-installer-signing-design.md
# D2/D8). The storage-zone password can write that one zone and nothing else; the account API
# key never comes near CI. It reaches curl through a config on stdin (-K -), never argv,
# which every process on the runner can read. Bunny passwords are hyphenated hex; anything
# else is refused, which also keeps it from breaking out of the quoted config value.
bunny_require_password() {
  if [ -z "${BUNNY_STORAGE_PASSWORD:-}" ]; then
    echo "ERROR: BUNNY_STORAGE_PASSWORD is not set: the installer host cannot be published" >&2; exit 1
  fi
  if ! [[ "$BUNNY_STORAGE_PASSWORD" =~ ^[A-Za-z0-9-]+$ ]]; then
    echo "ERROR: BUNNY_STORAGE_PASSWORD is not a Bunny storage-zone password (length ${#BUNNY_STORAGE_PASSWORD})" >&2; exit 1
  fi
}
# bunny_curl <curl args...>: the header line must stay exactly `bunny_curl() {`, because
# validate.yml extracts the function by that line to test that the password never reaches argv.
# Never add -L here: curl forwards the AccessKey config across a redirect to another host.
# Never add -v/--trace either: both print the header line, AccessKey included, to output.
bunny_curl() {
  printf 'header = "AccessKey: %s"\n' "$BUNNY_STORAGE_PASSWORD" \
    | curl --proto '=https' --tlsv1.2 -fsS -K - "$@"
}
bunny_url() { printf 'https://%s/%s/%s' "$BUNNY_STORAGE_HOST" "$BUNNY_STORAGE_ZONE" "$1"; }

PUT_ATTEMPTS="${PUT_ATTEMPTS:-5}"
# with_retries <label> <command...>: each upload is retried (a transient error on ONE object
# was seen on the v1.2.0 release), which is safe because overwriting an object is idempotent.
with_retries() {
  local label="$1" attempt=1
  shift
  while :; do
    if "$@"; then return 0; fi
    if [ "$attempt" -ge "$PUT_ATTEMPTS" ]; then
      echo "ERROR: put $label failed after $attempt attempt(s)" >&2
      return 1
    fi
    echo "put $label: attempt $attempt failed; retrying in $((attempt * 5))s" >&2
    sleep "$((attempt * 5))"
    attempt=$((attempt + 1))
  done
}
put() { # localpath key content-type
  with_retries "$2" s3 s3 cp "$1" "s3://${BUCKET}/$2" --content-type "$3" --no-progress
}
# bunny_put: Bunny checks the uppercase-hex SHA256 in the Checksum header and rejects a
# corrupted upload instead of storing it.
bunny_put() { # localpath name
  local sum
  sum="$(sha256sum "$1" | cut -d' ' -f1 | tr 'a-f' 'A-F')"
  with_retries "bunny:$2" bunny_curl -o /dev/null -T "$1" \
    -H "Checksum: $sum" -H "Content-Type: text/x-shellscript" "$(bunny_url "$2")"
}

if [ "$MODE" = "--preflight" ]; then
  bunny_require_password
  # As with RELEASE_SIGNING_KEY below: unexport once validated, so go build, aws, openssl,
  # python3 and curl never inherit it through the environment. bunny_curl still reads it —
  # it runs as a function in this shell, not a child process.
  export -n BUNNY_STORAGE_PASSWORD
  r2_auth
  printf 'preflight %s %s\n' "$ENV" "$(date -u +%FT%TZ)" > "${TMPDIR:-/tmp}/r2-preflight.txt"
  if ! s3 s3 cp "${TMPDIR:-/tmp}/r2-preflight.txt" "s3://${BUCKET}/.preflight" --content-type text/plain --no-progress; then
    echo "ERROR: token is active but cannot write bucket ${BUCKET}: check its permission (Object Read & Write) and bucket scope, on the account that owns the bucket" >&2
    exit 1
  fi
  r2_delete ".preflight" || echo "note: could not delete ${BUCKET}/.preflight (harmless)"
  if ! bunny_put "${TMPDIR:-/tmp}/r2-preflight.txt" ".preflight"; then
    echo "ERROR: BUNNY_STORAGE_PASSWORD cannot write zone ${BUNNY_STORAGE_ZONE} on ${BUNNY_STORAGE_HOST}: check the zone name, its region host and the password" >&2
    exit 1
  fi
  bunny_curl -o /dev/null -X DELETE "$(bunny_url .preflight)" || echo "note: could not delete ${BUNNY_STORAGE_ZONE}/.preflight (harmless)"
  echo "preflight OK: credentials can write ${BUCKET} and Bunny zone ${BUNNY_STORAGE_ZONE}"; exit 0
fi

# --- Retention: keep latest/ + the N newest vX.Y.Z versions on R2 (best-effort) ---
# Version source is git tags, so the release checkout must fetch them (fetch-depth: 0).
KEEP_VERSIONS="${KEEP_VERSIONS:-5}"
prune_old_r2_versions() {
  local tags old v arch key
  r2_auth
  tags=$(git -C "$ROOT" tag -l 'v[0-9]*' 2>/dev/null | sort -V) || { echo "retention: git tags unavailable, skipping prune"; return 0; }
  [ -z "$tags" ] && { echo "retention: no version tags, nothing to prune"; return 0; }
  old=$(printf '%s\n' "$tags" | head -n "-${KEEP_VERSIONS}")
  if [ -z "$old" ]; then
    echo "retention: $(printf '%s\n' "$tags" | grep -c .) version(s) <= keep=${KEEP_VERSIONS}; nothing to prune"; return 0
  fi
  # shellcheck disable=SC2086  # intentional word-split: one version per word on one line
  echo "retention: keeping latest/ + newest ${KEEP_VERSIONS}; pruning binaries for:" $old
  for v in $old; do
    for arch in amd64 arm64; do
      # Old name too, so v1.1.x objects published under the pre-rename binary name are still reaped.
      for key in "binaries/otel/$v/monitorable-agent-linux-$arch" "binaries/otel/$v/monitorable-otelcol-linux-$arch"; do
        if r2_delete "$key"; then echo "  pruned $key (or already absent)"; else echo "  (delete FAILED for $key)"; fi
      done
    done
    if r2_delete "binaries/otel/$v/SHA256SUMS"; then echo "  pruned binaries/otel/$v/SHA256SUMS (or already absent)"; else echo "  (delete FAILED for $v/SHA256SUMS)"; fi
    if r2_delete "binaries/otel/$v/SHA256SUMS.sig"; then echo "  pruned binaries/otel/$v/SHA256SUMS.sig (or already absent)"; else echo "  (delete FAILED for $v/SHA256SUMS.sig)"; fi
  done
}

# --prune-only: skip build/publish, just enforce retention on R2 (manual / backlog cleanup)
if [ "$MODE" = "--prune-only" ]; then
  prune_old_r2_versions || echo "retention prune skipped (non-fatal)"
  echo "prune-only complete"; exit 0
fi

# --- Release signing (spec 2026-09-26-installer-signing-design.md §4.2) ---
# Every published SHA256SUMS is signed with this env's key, which lives only in the agent
# repo's GitHub environment (and Infisical, for DR). The committed public key is the
# reference: a key that does not match it is refused before anything is built.
EXPECTED_PUBKEY_FILE="${EXPECTED_PUBKEY_FILE:-$SRC/keys/${ENV}.pub}"
[ -f "$EXPECTED_PUBKEY_FILE" ] || { echo "ERROR: no public key at $EXPECTED_PUBKEY_FILE" >&2; exit 1; }
# The one-line form install.sh embeds: base64 DER SubjectPublicKeyInfo, no PEM armour.
pem_body() { grep -v -- '-----' | tr -d '\n'; }
PUBKEY_LINE="$(pem_body < "$EXPECTED_PUBKEY_FILE")"

# installer_serves_key: true when this env's installer host serves an installer that embeds
# THIS env's key. Used by --check-installer, a manual pre-cutover check — it no longer gates
# the stub render; see step 4d below for how R2 still never loses the real installer before
# the installer host has it.
# The body is fetched completely before it is matched, and the matcher reads ALL of its
# input (grep -c, never -q): under pipefail, a reader that stops at the first match kills
# its writer with SIGPIPE once the rest of the body outgrows the pipe buffer, and the
# pipeline then reports a served key as "not served". validate.yml tests the matcher.
installer_body_has_key() {
  [ "$(grep -cF -- "SIGNING_PUBKEY=\"$PUBKEY_LINE\"")" -gt 0 ]
}
installer_serves_key() {
  local body
  body="$(curl --proto '=https' --tlsv1.2 -fsS "$INSTALLER_URL/install.sh")" || return 1
  installer_body_has_key <<<"$body"
}
if [ "$MODE" = "--check-installer" ]; then
  if installer_serves_key; then echo "installer OK: $INSTALLER_URL/install.sh embeds this env's key"; exit 0; fi
  echo "ERROR: $INSTALLER_URL/install.sh is not live with this env's signing key" >&2; exit 1
fi

# VERSION becomes the installer's MIN_VERSION and the signed `# version` line, and install.sh
# accepts only vX.Y.Z: anything else (a pre-release tag, the unset default "dev") would
# brick every latest install, and a `|` or `&` in it would corrupt the sed rendering below.
# Components are capped at 9 digits, as install.sh's version_ge caps them.
if ! [[ "$VERSION" =~ ^v[0-9]{1,9}\.[0-9]{1,9}\.[0-9]{1,9}$ ]]; then
  echo "ERROR: VERSION must look like vX.Y.Z (got '$VERSION')" >&2; exit 1
fi

if [ -z "${RELEASE_SIGNING_KEY:-}" ]; then
  echo "ERROR: RELEASE_SIGNING_KEY is not set: every published SHA256SUMS must be signed" >&2; exit 1
fi
SIGNING_KEY_FILE="$(mktemp)"
trap 'rm -f "$SIGNING_KEY_FILE"' EXIT
printf '%s\n' "$RELEASE_SIGNING_KEY" > "$SIGNING_KEY_FILE"
# From here on only the 0600 temp file holds the key: nothing this script runs (go build,
# openssl, aws, curl) inherits it through the environment.
unset RELEASE_SIGNING_KEY
if [ "$(openssl pkey -in "$SIGNING_KEY_FILE" -pubout 2>/dev/null | pem_body)" != "$PUBKEY_LINE" ]; then
  echo "ERROR: RELEASE_SIGNING_KEY does not match $EXPECTED_PUBKEY_FILE — refusing to sign" >&2; exit 1
fi
# A publish needs the Bunny password too; find out now, not after the R2 upload.
if [ "$MODE" != "--render-only" ]; then
  bunny_require_password
  # As with RELEASE_SIGNING_KEY above: unexport once validated, so go build, aws, openssl,
  # python3 and curl never inherit it through the environment. bunny_curl still reads it —
  # it runs as a function in this shell, not a child process.
  export -n BUNNY_STORAGE_PASSWORD
fi

rm -rf "$OUT"
mkdir -p "$OUT/configs" "$BINOUT"

# 1. Render the installer. $OUT/installer/install.sh is the real installer with all three
# values filled; it is what the installer host (Bunny) serves. $OUT/install.sh is the object
# that becomes <bucket>/install.sh: the stub (STUB_INSTALLER=1) or the real installer
# (STUB_INSTALLER=0). The stub is rendered here unconditionally — there is no pre-render
# gate any more — but it is only PUBLISHED after this run has verified the installer host
# serves this run's installer (step 4d, after verify_published), so R2 never loses the real
# installer unless the host demonstrably serves it first.
mkdir -p "$OUT/installer"
sed -e "s|@@BASE_URL@@|$BASE_URL|g" -e "s|@@SIGNING_PUBKEY@@|$PUBKEY_LINE|g" -e "s|@@MIN_VERSION@@|$VERSION|g" \
  "$SRC/install.sh" > "$OUT/installer/install.sh"
if [ "$STUB_INSTALLER" = 1 ]; then
  sed "s|@@INSTALLER_URL@@|$INSTALLER_URL|g" "$SRC/install-stub.sh" > "$OUT/install.sh"
else
  cp "$OUT/installer/install.sh" "$OUT/install.sh"
fi
chmod +x "$OUT/install.sh" "$OUT/installer/install.sh"
if grep -q '@@' "$OUT/install.sh" "$OUT/installer/install.sh"; then
  echo "ERROR: an unrendered placeholder remains in install.sh" >&2; exit 1
fi

# 2. Stage the configs to publish. Published surface is Linux-only.
mkdir -p "$OUT/configs/linux"
cp -R "$SRC/configs/linux/." "$OUT/configs/linux/"

# 3. Cross-compile binaries (CGO off for static, portable artifacts). Linux only.
build() { # os arch ext
  local os="$1" arch="$2" ext="${3:-}"
  echo "building ${os}/${arch}"
  GOOS="$os" GOARCH="$arch" CGO_ENABLED=0 \
    go build -C "$ROOT" \
    -ldflags "-X github.com/monitorable-io/monitorable-agent/internal/version.Version=${VERSION}" \
    -o "$BINOUT/monitorable-agent-${os}-${arch}${ext}" \
    cmd/monitorable-agent/main.go
}
build linux amd64
build linux arm64
# macOS/Windows intentionally not built/published yet (owner decision 2026-06-14);
# to enable later, add `build darwin …` / `build windows … .exe` + their config copy.

# 3b. Checksums for the installer's verification step. ONE SHA256SUMS covers every file
# install.sh downloads — both binaries, the collector config and the systemd unit template
# — because the unit template is sed-rendered into /etc/systemd/system and daemon-reloaded,
# i.e. an unverified one is an arbitrary ExecStart on every installing host. The first line
# is a `# version vX.Y.Z` comment install.sh checks against its pinned/floor versions before
# trusting the rest; `sha256sum -c` warns about that comment line, which is expected.
# The two config files are copied into $BINOUT purely so the four entries can be generated
# (and verified locally with `cd distribution-build/binaries/otel && sha256sum -c
# SHA256SUMS`) under BARE relative names, which is how install.sh looks them up. They are
# PUBLISHED from $OUT/configs below; the upload loop here globs monitorable-agent-linux-*
# only, so these copies never reach the bucket.
cp "$OUT/configs/linux/collector-config.yaml" "$OUT/configs/linux/monitorable-agent.service" "$BINOUT/"
( cd "$BINOUT" && {
    printf '# version %s\n' "$VERSION"
    sha256sum \
      monitorable-agent-linux-amd64 \
      monitorable-agent-linux-arm64 \
      collector-config.yaml \
      monitorable-agent.service
  } > SHA256SUMS )
openssl dgst -sha256 -sign "$SIGNING_KEY_FILE" -out "$BINOUT/SHA256SUMS.sig" "$BINOUT/SHA256SUMS"
# Prove the signature with the committed public key before anything leaves this machine.
if ! openssl dgst -sha256 -verify "$EXPECTED_PUBKEY_FILE" -signature "$BINOUT/SHA256SUMS.sig" "$BINOUT/SHA256SUMS" >/dev/null; then
  echo "ERROR: the fresh SHA256SUMS signature does not verify with $EXPECTED_PUBKEY_FILE" >&2; exit 1
fi

if [ "$MODE" = "--render-only" ]; then
  echo "render-only: artifacts in $OUT"; exit 0
fi

# 4. Publish to the REAL bucket.
r2_auth

# Publish order is binaries → configs → SHA256SUMS.sig → SHA256SUMS → latest.json →
# install.sh (Bunny) → verify_published → install.sh (R2, real or stub) → verify_served.
# install.sh verifies the binary, the config AND the unit against the one SHA256SUMS, so any
# install that races this publish FAILS CLOSED — a new config or binary against the old
# sums (or the reverse) aborts before anything is touched, and the operator retries. The
# window is the length of this upload, a few minutes. Sums after the payloads keeps that
# window as short as possible and keeps the previous set installable for as much of it as
# possible; it does not eliminate it. Signed, versioned config objects would; see
# distribution/README.md. install.sh (Bunny) goes after the sums: the new installer's
# MIN_VERSION is this release, so it must not go live before the signed sums that satisfy it
# (it would refuse every install for the whole upload, and for good if a later put failed),
# while the old installer, with its lower floor, accepts the new sums. R2's install.sh (real
# or stub) goes dead last of all, after verify_published has confirmed the installer host:
# see 4d below.
# (Fail loud if the build produced no binaries, rather than uploading a literal glob.)
shopt -s nullglob
bins=("$BINOUT"/monitorable-agent-linux-*)
shopt -u nullglob
if [ "${#bins[@]}" -eq 0 ]; then
  echo "ERROR: no binaries in $BINOUT — build step produced nothing" >&2; exit 1
fi
for f in "${bins[@]}"; do
  put "$f" "binaries/otel/${VERSION}/$(basename "$f")" "application/octet-stream"
  put "$f" "binaries/otel/latest/$(basename "$f")"     "application/octet-stream"
done

# Configs are unversioned objects while binaries are versioned, so upload configs
# AFTER binaries: a reinstall that races this publish then sees, at worst, an old
# config with a new binary (harmless) rather than a new config with an old binary
# (unknown `file_storage` type to that binary → crash loop).
while IFS= read -r f; do
  rel="configs/${f#"$OUT"/configs/}"
  case "$f" in
    *.yaml) ct="text/yaml" ;;
    *)      ct="text/plain" ;;   # .service
  esac
  put "$f" "$rel" "$ct"
done < <(find "$OUT/configs" -type f)

put "$BINOUT/SHA256SUMS.sig" "binaries/otel/${VERSION}/SHA256SUMS.sig" "application/octet-stream"
put "$BINOUT/SHA256SUMS.sig" "binaries/otel/latest/SHA256SUMS.sig"     "application/octet-stream"
put "$BINOUT/SHA256SUMS" "binaries/otel/${VERSION}/SHA256SUMS" "text/plain"
put "$BINOUT/SHA256SUMS" "binaries/otel/latest/SHA256SUMS"     "text/plain"

# 4b. Version manifest — the backend reads this (GET /latest.json) to compare
# each server's reported agent version against the newest published one.
printf '{"version":"%s"}\n' "$VERSION" > "$OUT/latest.json"
put "$OUT/latest.json" "latest.json" "application/json"

# 4b'. The installer host (Bunny) gets the real installer LAST: its MIN_VERSION is this
# release, so it must not go live before the signed sums that satisfy it. If this put fails,
# the previous installer (lower floor) stays live and accepts the new sums; the job fails and
# a re-run finishes it.
bunny_put "$OUT/installer/install.sh" "install.sh"

echo "published ${ENV} distribution to ${BUCKET} and installer to Bunny zone ${BUNNY_STORAGE_ZONE}"

# 4c. Verify the SERVED surface against what was just built: every object install.sh
# downloads (versioned + latest/ binaries, configs, both SHA256SUMS), plus latest.json on
# the R2 base URL, PLUS install.sh on the installer host (Bunny), all fetched over https and
# compared byte-for-byte. A put that "succeeded" but served something else, or a stale edge
# cache, fails the job here rather than on a customer's host — a Bunny pull zone that has
# cached the previous script fails this check for as long as it keeps serving the stale
# copy. Objects can take a moment to propagate, so mismatches are retried for a bounded
# window before they count. R2's own install.sh is checked separately, in 4d: it is not
# uploaded yet at this point.
VDIR="$OUT/verify"
verify_served() { # url local_path
  local url="$1" local_path="$2" tries=0
  until curl --proto '=https' --tlsv1.2 -fsSL --retry 3 -o "$VDIR/obj" "$url" && cmp -s "$VDIR/obj" "$local_path"; do
    tries=$((tries + 1))
    if [ "$tries" -ge 12 ]; then
      echo "ERROR: served $url does not match the published $local_path (after ${tries} tries)" >&2
      return 1
    fi
    sleep 10
  done
  echo "verified $url"
}
verify_published() {
  local key local_path
  rm -rf "$VDIR"; mkdir -p "$VDIR"
  # url → local file it must match
  local pairs=(
    "$BASE_URL/binaries/otel/${VERSION}/SHA256SUMS=$BINOUT/SHA256SUMS"
    "$BASE_URL/binaries/otel/latest/SHA256SUMS=$BINOUT/SHA256SUMS"
    "$BASE_URL/binaries/otel/${VERSION}/SHA256SUMS.sig=$BINOUT/SHA256SUMS.sig"
    "$BASE_URL/binaries/otel/latest/SHA256SUMS.sig=$BINOUT/SHA256SUMS.sig"
    "$BASE_URL/configs/linux/collector-config.yaml=$OUT/configs/linux/collector-config.yaml"
    "$BASE_URL/configs/linux/monitorable-agent.service=$OUT/configs/linux/monitorable-agent.service"
  )
  local f
  for f in "${bins[@]}"; do
    pairs+=("$BASE_URL/binaries/otel/${VERSION}/$(basename "$f")=$f" "$BASE_URL/binaries/otel/latest/$(basename "$f")=$f")
  done
  pairs+=("$BASE_URL/latest.json=$OUT/latest.json")
  pairs+=("$INSTALLER_URL/install.sh=$OUT/installer/install.sh")
  local pair
  for pair in "${pairs[@]}"; do
    key="${pair%%=*}"; local_path="${pair#*=}"
    verify_served "$key" "$local_path" || return 1
  done
  curl -fsSL "$BASE_URL/binaries/otel/latest/SHA256SUMS" -o "$VDIR/sums"
  curl -fsSL "$BASE_URL/binaries/otel/latest/SHA256SUMS.sig" -o "$VDIR/sig"
  if ! openssl dgst -sha256 -verify "$EXPECTED_PUBKEY_FILE" -signature "$VDIR/sig" "$VDIR/sums" >/dev/null; then
    echo "ERROR: the served latest/SHA256SUMS does not verify with $EXPECTED_PUBKEY_FILE" >&2
    return 1
  fi
  echo "verified the served signature"
}
verify_published

# 4d. R2's install.sh goes last of all, after the installer host was verified above: in stub
# mode that is what keeps R2 from ever losing the real installer to a stub before the host
# serves this run's installer — and, unlike a gate on the host's CURRENT key, it keeps a
# key-rotation release working (the host only receives the new key from this same run).
# In non-stub mode the real installer's MIN_VERSION is this release, so after the sums is
# all it needs; last costs nothing.
put "$OUT/install.sh" "install.sh" "text/x-shellscript"
verify_served "$BASE_URL/install.sh" "$OUT/install.sh"

rm -rf "$VDIR"
echo "verified ${ENV} surface at ${BASE_URL} and ${INSTALLER_URL}"

# 5. Enforce retention: keep latest/ + the newest KEEP_VERSIONS versions on R2.
prune_old_r2_versions || echo "retention prune skipped (non-fatal)"
