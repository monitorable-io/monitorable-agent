# Agent distribution (canonical source)

Version-controlled source for the Monitorable agent distribution served at the
`get.*` host of each environment:

| Env | Host | Origin |
|-----|------|--------|
| dev | `get.monitorable.lan` | nginx static (`/var/www/get-monitorable`, manual) |
| staging | `get-mon.ok9k.com` | R2 bucket `monitorable-get-staging` (CI) |
| prod (Phase 2) | `get.monitorable.io` | R2 bucket `monitorable-get-prod` (CI) |

## Contents

- `install.sh` — Linux installer template with four placeholders: `@@BASE_URL@@` (the
  env's download host), `@@SIGNING_PUBKEY@@` (the env's release-signing public key, one
  line of base64 DER), `@@MIN_VERSION@@` (the oldest release it accepts as `latest`) and
  `@@API_URL@@` (the env's public API host, which answers the enrollment call below).
  `scripts/publish-dist.sh` fills all four when it renders the installer for the
  installer host (Bunny). Since v1.3.2 R2's `install.sh` is the stub (`install-stub.sh`,
  its own single `@@INSTALLER_URL@@` placeholder) instead of a second copy of this
  template. A copy with any placeholder left refuses to run. The API key comes from
  `MONITORABLE_API_KEY` in the environment (preferred — an argv value is world-readable in
  `/proc/<pid>/cmdline` and is written to `auth.log` by sudo) or from `--api-key=`, which
  overrides it. The environment form is `sudo -s` to get a root shell, then `export
  MONITORABLE_API_KEY=<key>` on its own line, then `curl … | sh -s -- --endpoint=<url>` —
  all inside that root shell. Don't use sudo's `-E` flag: sudo-rs, Ubuntu's default sudo
  since 25.10, doesn't implement `-E` at all, and some sudoers policies refuse it even
  where it's implemented; a `VAR=x curl … | sudo sh` prefix doesn't work either, since it
  sets the variable for `curl`, not for `sudo`. Where a root shell isn't available, `--api-key=`
  is the fallback, not an equal alternative. Other args: `--endpoint=` (optional; the backend always passes it),
  `--version=vX.Y.Z` (optional; default `latest`). It migrates an install made under the previous names (binary
  `monitorable-otelcol`, unit `monitorable-collector`) — the on-disk queue is kept.
- `configs/linux/` — collector config, the `monitorable-agent.service` systemd unit
  (SMART capability, supplementary-group and `DeviceAllow=` placeholders substituted by
  `install.sh`; the API key and endpoint are written separately to
  `/etc/monitorable/agent.env`, 0600 root-only, referenced via `EnvironmentFile=`). The
  exporter uses `endpoint: "${MONITORABLE_ENDPOINT}"` (no `/otel` suffix; OTLP appends
  `/v1/metrics`).

## Enrollment (host-driven install)

Instead of copying a per-server command from the dashboard, a host can register itself.
Create an enrollment key in **Team → API keys**, then run the installer with it:

```bash
curl --proto '=https' --tlsv1.2 -fsSL <installer>/install.sh | sudo sh -s -- --enroll-key=<key>
```

The installer calls `POST $API_URL/v1/enroll`, which creates the server (or rotates its
agent key, see below) and returns the agent key and the ingest endpoint. The installer
writes those to `/etc/monitorable/agent.env` exactly as a dashboard-rendered install does.
An enrollment key can only register servers; if one is exposed, revoke it in
**Team → API keys**.

| Flag | Meaning |
|---|---|
| `--enroll-key=<key>` | The enrollment key. Replaces `--api-key=` and `--endpoint=`, which are refused alongside it; an exported `MONITORABLE_API_KEY` is refused too. |
| `--name=<name>` | The server's name. Default: the host's name (`uname -n`), cut to 100 characters. Letters, digits, space, dot, underscore and dash; at most 100 characters. |
| `--server-ref=<ref>` | This host's stable id: a cloud instance id, a Terraform `self.id`. Letters, digits, dot, underscore, colon, slash and dash; at most 128 characters. An empty value is refused. |

The flags take the equals form only (`--name=web-01`, not `--name web-01`). `--name` and
`--server-ref` need an enrollment key, and none of the three combines with `--uninstall`.

**The key through the environment.** `MONITORABLE_ENROLL_KEY=<key>` is the alternative to
`--enroll-key=`, for the same reason as `MONITORABLE_API_KEY`: an argv value sits in the
world-readable `/proc/<pid>/cmdline` for the whole run and sudo writes it to `auth.log`.
Export it inside a root shell (`sudo -s`), as in the main install. The installer removes
it from its own environment right away, so no child process inherits it.

**Flow order.** Platform checks, then download and signature/checksum verification, then
enrollment, then install. Enrollment happens only once everything is verified, so a failed
download or a bad signature never registers a server. A refused enrollment (plan limit, a
rejected key, an API that cannot be reached) installs nothing: no service user, no files in
place, and an agent already on the host keeps running, untouched.

**No derived ref.** The installer never makes up a `--server-ref`, not from the hostname
and not from a machine id. Without `--server-ref`, every run creates a new server. On a
host that already has an agent, the installer warns before it enrolls, because re-running
the command registers a second server and the first one goes stale on the dashboard. To
re-enroll a host as the same server, pass the same `--server-ref` each time.

**What a rotation means.** When `--server-ref` names a server the organization already has,
the enrollment is a rotation: the same server keeps its name and its history, gets a new
agent key, and the previous agent key stops working. The installer says so
("Re-enrolled server #N (name): its agent key was rotated").

**In automation, pass `--server-ref`.** The enrollment request is sent with
`--retry 5 --retry-max-time 240`, so curl retries a 429 (waiting as long as `Retry-After`
says), a 408, a server error (500, 502, 503, 504) and a timeout; any other 4xx, a plan
limit included, is final on the first try. The API allows 10 enrollments a minute per key
(a fixed one-minute window), and hosts that are waiting all come back at the same window
reset, so one key enrolls roughly 40-50 hosts in a simultaneous burst. A host that runs out
of retries exits 1 with "re-run in a minute" and nothing installed (cloud-init does not run
the command again): stagger larger simultaneous boots, or give groups of hosts their own
enrollment key. With `--server-ref`, a retried request is a second rotation of the same server, which
changes nothing visible. Without it, a response that is lost after the server has already
committed can register a second server, which then shows on the dashboard. So config
management, Terraform and cloud-init should always pass it:

```yaml
## template: jinja
#cloud-config
# cloud-init fills in the instance id below
runcmd:
  - "curl --proto '=https' --tlsv1.2 -fsSL <installer>/install.sh | sh -s -- --enroll-key=<key> --server-ref={{ v1.instance_id }}"
```

**Where the enroll key goes.** The installer sends it to curl on curl's standard input
(`-K -`), so it is never in curl's argv; it is never written to disk. The response, which
holds the new agent key, is written under `umask 077`, read, and deleted at once.

**A rollback after an enrollment keeps the enrolled key.** See "Failed-update rollback"
below.

Binaries and `SHA256SUMS` are **not** committed — `scripts/publish-dist.sh`
cross-compiles the binaries fresh and writes the checksums alongside them.

**Published surface is Linux-only** (amd64 + arm64) — the live `get.*` surface serves
`install.sh`, `configs/linux/`, the linux binaries, `SHA256SUMS` and `SHA256SUMS.sig`
(binaries and both sums files under `binaries/otel/<version>/` and `binaries/otel/latest/`),
and `latest.json`.

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

`publish-dist.sh` uploads binaries → configs → `SHA256SUMS.sig` → `SHA256SUMS` →
`latest.json` → `install.sh` (Bunny, the installer host) → verify → `install.sh` (R2, real
or stub) → verify it. The sums follow every payload they cover, so an install racing a
publish fails closed rather than installing new checksums over old payloads. The installer
host's copy follows the sums because its `MIN_VERSION` is the new release: published first,
it would refuse every install until the new signed sums arrived, whereas the old installer,
with its lower floor, accepts them. R2's `install.sh` goes dead last of all, only after that
publish has verified the installer host serves this run's installer — see below.

Around that check:

- Nothing is touched before the preflights pass (root, Linux, amd64/arm64, `curl`,
  `sha256sum`, `openssl`, `fold`, a running systemd). A host without systemd is refused with
  zero side effects. So are an embedded key that is not an ECDSA P-256 public key, a
  malformed `MIN_VERSION`, and a `--version=` that is neither `latest` nor `vX.Y.Z` — each
  with its own message, never a failed signature or a failed download later on.
- With an `https://` base URL every download is `--proto =https --tlsv1.2`, so a redirect
  cannot downgrade a transfer to cleartext. The dev `http://` origin keeps plain curl.
- The output prints the verified binary's sha256. Each release's `SHA256SUMS` (and its
  staging-key `SHA256SUMS.sig`) is also attached to its GitHub release, so it can be
  checked against a second origin.
- The service user, group and docker membership are created only after verification, so a
  failed fresh install leaves no account behind (only the empty `/opt/monitorable` and
  `/etc/monitorable` the downloads were staged in).

`SHA256SUMS` starts with one `# version vX.Y.Z` line and is signed (openssl ECDSA P-256,
`SHA256SUMS.sig`) with a per-environment key that exists only in this repo's GitHub
environments (and an offline backup). The public keys are committed at `distribution/keys/`.
The script verifies the signature, then requires the signed version to be ≥ its
`MIN_VERSION` (or to equal `--version=`), then checks every file against the authenticated
sums. The trust anchor is wherever `install.sh` itself comes from. It is served from the
installer host — `https://get.monitorable.net/install.sh` in prod,
`https://install-mon.ok9k.com/install.sh` in staging — a Bunny Storage zone behind a Bunny
CDN pull zone, not cached, with write access held only by that env's
`BUNNY_STORAGE_PASSWORD` (a storage-zone password, scoped to that one zone; the Bunny
account API key never reaches CI). `publish-dist.sh` uploads the rendered installer there
**last**, after every other object, once the signed sums it depends on are already live.
Writing the R2 download bucket is not enough to get code onto a host: that needs the
installer host, and the signing key, too. Since v1.3.2 the copy at `<get host>/install.sh`
on R2 is a stub (both envs) that just points at the installer host — `publish-dist.sh`
publishes it only after that same run's publish has verified the installer host serves this
run's installer (see "Publish order" above), so R2 never loses the real installer to a stub
before the host demonstrably has it. Verify a release yourself:
`openssl dgst -sha256 -verify distribution/keys/prod.pub -signature SHA256SUMS.sig SHA256SUMS`.
Design: `docs/superpowers/specs/2026-09-26-installer-signing-design.md` in the platform repo.

**Off-infra backstop.** `.github/workflows/dist-backstop.yml` runs
`scripts/check-published-dist.sh prod` every 15 minutes. It checks the installer (with its
key and `MIN_VERSION`), runs the `openssl` signature check of the served `SHA256SUMS`
against the committed `keys/prod.pub`, confirms `latest.json` matches the signed version,
compares the config and unit hashes, and sends a HEAD for each binary. It reports to
healthchecks.io (check `installer-dist-prod`). The runs are started by a systemd timer on a
host outside our infrastructure (`scripts/dist-backstop-dispatch/`), not by GitHub's cron,
which dropped almost every slot. Run it by hand with
`gh workflow run dist-backstop.yml -f env=staging` (or `prod`). The harnesses are
`scripts/test-check-published-dist.sh` and `scripts/test-dispatch-dist-backstop.sh`.

## Publishing

```bash
scripts/publish-dist.sh staging --preflight      # prove the credentials can write the R2 bucket AND the Bunny zone (put+delete one object in each)
scripts/publish-dist.sh staging                  # build + render + upload to R2, then the installer to the Bunny installer host
scripts/publish-dist.sh staging --render-only    # build + render into ./distribution-build, no upload
scripts/publish-dist.sh staging --check-installer  # manual pre-cutover check: does this env's installer host serve an installer with this env's key?
```

`publish-dist.sh` renders `@@API_URL@@` per environment: staging `https://api-mon.ok9k.com`,
prod `https://api.monitorable.io`. Dev has no publish step; for a hand-rendered dev
installer set it to `http://api.monitorable.lan` (an `http://` API URL gets plain curl,
without the `--proto`/`--tlsv1.2` flags).

A build needs `VERSION=vX.Y.Z` (the release job passes the pushed tag; anything else is
refused before signing) and `RELEASE_SIGNING_KEY`, which must match
`distribution/keys/<env>.pub`. A publish or `--preflight` also needs
`BUNNY_STORAGE_PASSWORD` (not `--render-only`, which never uploads).

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
(`target=prune-staging`) only enforces R2 retention on staging. `publish-prod` can only
publish tags from v1.3.0 on: an older tag's tree has no `distribution/keys/prod.pub`, so
`publish-dist.sh` stops with "no public key" — there is no prod rollback to v1.2.x through
`publish-prod`. There is no merge-to-main publish. A config-only change therefore still needs a new tag, and reaches a host only
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

`SHA256SUMS` is uploaded after every payload it covers, but the upload as a whole is not
atomic: for the few minutes it takes, the bucket holds a mix of new and old objects and the
sums match only one of them. A fresh install started inside that window therefore aborts on a checksum mismatch
without touching the host — fail-closed by design — and simply needs to be retried once the
release job reports success. Existing agents are unaffected; they never re-download.

### Failed-update rollback (v1.3.3+)

An update (re-running the installer on a host with an agent) that fails the stay-up check
puts the previous agent back. Before any side effect the installer records whether the
running agent is healthy: the current `monitorable-agent` unit, binary, config and
`agent.env` present, `active`/`running`, main process up ≥ 10 s (measured with
`/proc/<pid>/stat` start times, because lxcfs virtualises `/proc/uptime` in LXC) and running
the binary that is on disk (not an older one left running by an interrupted run). Right before
the install renames it clears any old snapshot, then, for a healthy agent only, snapshots:

| Live file | Snapshot |
|---|---|
| `/opt/monitorable/monitorable-agent` | `/opt/monitorable/monitorable-agent.prev` (hard link) |
| `/etc/monitorable/collector-config.yaml` | `/etc/monitorable/collector-config.yaml.prev` |
| `/etc/monitorable/agent.env` | `/etc/monitorable/agent.env.prev` |
| `/etc/systemd/system/monitorable-agent.service` | `/etc/monitorable/monitorable-agent.service.prev` |

- **Update stays up:** the snapshot is deleted.
- **Update fails** (it doesn't stay up, or `daemon-reload`, `enable` or the restart fails
  outright): the snapshot is renamed back and the service is restarted and re-checked.
  - The run exits 1 either way.
  - The output says whether the previous agent is running again.
  - If this run gave a new API key or endpoint, the output says it was not applied: the
    restored agent keeps its previous `agent.env`.
  - An enrollment run is the exception. The binary, config and unit are restored, but
    `agent.env` stays as this run wrote it, because the enrolled agent key is shown once
    and, after a `--server-ref` rotation, the previous key no longer works. The output
    says "The restored agent uses the agent key from this enrollment." The restored
    release reads that same `agent.env`, since a snapshot is only taken of a current
    (v1.2.0 or later) unit.
- **No snapshot** (fresh install, an agent already down or up for less than 10 s, the pre-v1.2.0
  migration): a failure is reported as before, as "restarting in a loop", or, when systemd
  refused the start outright, "systemd could not start the agent".

Snapshots never go under `/var/lib/monitorable`, which the agent user can write. The rollback
never downloads anything, so it doesn't check `MIN_VERSION`. The on-disk queue is not snapshotted.

An interrupted update (for example, a dropped SSH session) can leave a host on the new
release with the snapshot still in place. Re-run the update: the next run clears the old
snapshot first. Spec: platform `docs/superpowers/specs/2026-09-27-update-rollback-design.md`.

### Rollback semantics

The config and unit objects on R2 are unversioned while binaries are versioned, and
`SHA256SUMS` is published per version. An older `--version=` therefore fails **closed** on
a checksum mismatch — the installer stops before touching the running install — instead of
installing a v1.1.23 config next to a v1.1.22 binary that has no `file_storage` type and
crash-looping the host. Rolling back below the current config requires republishing the
matching (pre-rollback) config, unit and `SHA256SUMS` for that version. A signing
installer also refuses any `--version=` below v1.3.0: those releases have no
`SHA256SUMS.sig` and no signed version line.

`--version=` below v1.2.0 (the rename release) cannot work either way: those older
version prefixes on R2 hold objects under the pre-rename binary name
(`monitorable-otelcol-linux-$ARCH`) and have no `SHA256SUMS` object, and the current
`install.sh` only ever requests `monitorable-agent-linux-$ARCH` plus its checksum file.
Rolling back below the rename requires republishing that old version's binary and config
under the new install.sh's expectations (or keeping a matching pre-rename install.sh
around) — there is no supported `--version=` path across the rename boundary.
