# Agent distribution (canonical source)

Version-controlled source for the Monitorable agent distribution served at the
`get.*` host of each environment:

| Env | Host | Origin |
|-----|------|--------|
| dev | `get.monitorable.lan` | nginx static (`/var/www/get-monitorable`, manual) |
| staging | `get-mon.ok9k.com` | R2 bucket `monitorable-get-staging` (CI) |
| prod (Phase 2) | `get.monitorable.io` | R2 bucket `monitorable-get-prod` (CI) |

## Contents

- `install.sh` — Linux installer. `BASE_URL="@@BASE_URL@@"` is a publish-time
  placeholder baked per env by `scripts/publish-dist.sh`. The API key comes from
  `MONITORABLE_API_KEY` in the environment (preferred — an argv value is world-readable in
  `/proc/<pid>/cmdline` and is written to `auth.log` by sudo) or from `--api-key=`, which
  overrides it. The environment form is `sudo -s` to get a root shell, then `export
  MONITORABLE_API_KEY=<key>` on its own line, then `curl … | sh -s -- --endpoint=<url>` —
  all inside that root shell. Don't use sudo's `-E` flag: sudo-rs, Ubuntu's default sudo
  since 25.10, doesn't implement `-E` at all, and some sudoers policies refuse it even
  where it's implemented; a `VAR=x curl … | sudo sh` prefix doesn't work either, since it
  sets the variable for `curl`, not for `sudo`. Where a root shell isn't available, `--api-key=`
  is the fallback, not an equal alternative. Other args: `--endpoint=` (optional; the backend always passes it),
  `--version=` (optional). It migrates an install made under the previous names (binary
  `monitorable-otelcol`, unit `monitorable-collector`) — the on-disk queue is kept.
- `configs/linux/` — collector config, the `monitorable-agent.service` systemd unit
  (SMART capability, supplementary-group and `DeviceAllow=` placeholders substituted by
  `install.sh`; the API key and endpoint are written separately to
  `/etc/monitorable/agent.env`, 0600 root-only, referenced via `EnvironmentFile=`). The
  exporter uses `endpoint: "${MONITORABLE_ENDPOINT}"` (no `/otel` suffix; OTLP appends
  `/v1/metrics`).

Binaries and `SHA256SUMS` are **not** committed — `scripts/publish-dist.sh`
cross-compiles the binaries fresh and writes the checksums alongside them.

**Published surface is Linux-only** (amd64 + arm64) — the live `get.*` surface serves
`install.sh`, `configs/linux/`, the linux binaries, and `SHA256SUMS`.

## Integrity

One `SHA256SUMS` object, published under `binaries/otel/<version>/` and
`binaries/otel/latest/`, lists exactly four bare relative names:

```
monitorable-agent-linux-amd64
monitorable-agent-linux-arm64
collector-config.yaml
monitorable-agent.service
```

`install.sh` downloads everything it needs to `.tmp` paths under `/opt/monitorable` first,
verifies the three files it installs (binary, config, unit template) against that one
object, and only then moves anything into place. The unit template matters as much as the
binary: it is sed-rendered into `/etc/systemd/system` and `daemon-reload`ed, so an
unverified one is an arbitrary `ExecStart=`.

`publish-dist.sh` uploads binaries → configs → `SHA256SUMS`, sums strictly last, so an
install racing a publish sees at worst the previous, self-consistent set rather than new
checksums over old payloads.

Around that check:

- Nothing is touched before the preflights pass (root, Linux, amd64/arm64, `curl`,
  `sha256sum`, a running systemd). A host without systemd is refused with zero side
  effects.
- With an `https://` base URL every download is `--proto =https --tlsv1.2`, so a redirect
  cannot downgrade a transfer to cleartext. The dev `http://` origin keeps plain curl.
- The output prints the verified binary's sha256. Each release's `SHA256SUMS` is also
  attached to its GitHub release, so it can be checked against a second origin.
- The service user, group and docker membership are created only after verification, so a
  failed fresh install leaves no account behind (only the empty `/opt/monitorable` and
  `/etc/monitorable` the downloads were staged in).

What this protects: a truncated or corrupted transfer, and a CDN serving new checksums
against stale payloads. What it does **not** protect: a compromise of the bucket or of the
TLS channel — `SHA256SUMS` is unsigned and comes from the same origin as `install.sh`
itself. The root of trust is the install command rendered in the dashboard. Signing the
checksum file with a key held outside R2 is a planned follow-up.

## Publishing

```bash
scripts/publish-dist.sh staging --preflight      # prove the credentials can write the bucket (put+delete one object)
scripts/publish-dist.sh staging                  # build + render + upload to R2
scripts/publish-dist.sh staging --render-only    # build + render into ./distribution-build, no upload
```

Uploads go over R2's **S3 API** with the AWS CLI (preinstalled on GitHub runners). The
credentials are `CLOUDFLARE_R2_TOKEN` — an R2 API token with **Object Read & Write**
scoped to that one bucket — and `CLOUDFLARE_ACCOUNT_ID`; the S3 key pair is derived from
the token as Cloudflare documents (Access Key ID = the token's id, Secret Access Key =
SHA-256 of its value), so there is no second secret to store. This is what makes
per-bucket tokens possible: the Cloudflare REST API that wrangler uses rejects
bucket-scoped tokens (`403 Authentication error`) and needs the account-wide "Admin Read
& Write" instead — learned the hard way on 2026-09-22, when tightening the staging token
to Object Read & Write broke the wrangler-based publish. No npm, no wrangler.

Publishing to staging happens **only** when a `v*` tag is pushed
(`.github/workflows/release.yml` job `release` → `scripts/publish-dist.sh staging` +
GitHub release + `latest.json`). Publishing to **prod** is a manual dispatch of the same
workflow **on `main`** with `target=publish-prod`, `tag=<that released tag>` and
`confirm=publish-prod` — GitHub runs the workflow file as it exists at the dispatched
ref, so the job checks out the tag's tree itself instead of being dispatched on it (job
`publish-prod` → `scripts/publish-dist.sh prod`, under the `production` GitHub
environment, which holds an R2 token scoped to the prod bucket only). A plain dispatch
(`target=prune-staging`) only enforces R2 retention on staging. There is no merge-to-main
publish. A config-only change therefore still needs a new tag, and reaches a host only
when it is reinstalled — collectors never auto-update.

Every `put` is retried (5 attempts, linear backoff), and after the last object the script
re-downloads the whole surface through the public base URL and compares it byte-for-byte
with what it built (`verify_published`), so a half-applied publish fails the job instead
of a customer's install.

## Retry and offline buffering (Linux, v1.1.23+)

`otlp_http` never gives up on a retryable error (`max_elapsed_time: 0`, backoff capped at
60 s) and keeps its sending queue on disk through the `file_storage` extension
(`/var/lib/monitorable/queue`; override with `MONITORABLE_STATE_DIR`, though on a real
host that needs a systemd drop-in — the shipped unit's `EnvironmentFile=` only sets
`MONITORABLE_API_KEY` and `MONITORABLE_ENDPOINT`, from `/etc/monitorable/agent.env`).
The queue holds 1440 batches — stored as uncompressed protobuf,
typically 50-100 KB each → typically well under 150 MB (a busy host with
`send_batch_size: 3000` can flush more than one batch a minute, so 1440 requests may be
less than a day there) — and survives collector restarts. Permanent errors (4xx, 500) are
still dropped immediately; when the queue is full the newest batch is rejected.
`cmd/monitorable-agent/main.go` registers the extension. A "Dropping data … interrupted due to shutdown" log line at a restart is
misleading: those batches are re-read from disk and retried as soon as the collector is
back.

### The publish window

`SHA256SUMS` is uploaded last, but the upload as a whole is not atomic: for the few minutes
it takes, the bucket holds a mix of new and old objects and the sums match only one of
them. A fresh install started inside that window therefore aborts on a checksum mismatch
without touching the host — fail-closed by design — and simply needs to be retried once the
release job reports success. Existing agents are unaffected; they never re-download.

### Rollback semantics

The config and unit objects on R2 are unversioned while binaries are versioned, and
`SHA256SUMS` is published per version. An older `--version=` therefore fails **closed** on
a checksum mismatch — the installer stops before touching the running install — instead of
installing a v1.1.23 config next to a v1.1.22 binary that has no `file_storage` type and
crash-looping the host. Rolling back below the current config requires republishing the
matching (pre-rollback) config, unit and `SHA256SUMS` for that version.

`--version=` below v1.2.0 (the rename release) cannot work either way: those older
version prefixes on R2 hold objects under the pre-rename binary name
(`monitorable-otelcol-linux-$ARCH`) and have no `SHA256SUMS` object, and the current
`install.sh` only ever requests `monitorable-agent-linux-$ARCH` plus its checksum file.
Rolling back below the rename requires republishing that old version's binary and config
under the new install.sh's expectations (or keeping a matching pre-rename install.sh
around) — there is no supported `--version=` path across the rename boundary.
