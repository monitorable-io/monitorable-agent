# monitorable-agent

A single-binary [OpenTelemetry Collector](https://opentelemetry.io/docs/collector/)
distribution built for [Monitorable](https://monitorable.io): server health monitoring
with a five-minute setup. It bundles the standard `host_metrics` receiver with a custom
`monitorable` receiver, batches and retries deliveries, and exports metrics over
OTLP/HTTP to a Monitorable ingest endpoint. There is no separate config to author —
`install.sh` sets it all up.

## What it collects

- CPU, memory, disk, network, and load average
- CPU temperature and frequency
- SMART disk health (tiered by detected hardware; see Security model below)
- systemd unit snapshot
- Docker container snapshot (CPU/memory/network per container), when a Docker socket is
  present

## Install

```bash
sudo -s
export MONITORABLE_API_KEY=<key>
curl -fsSL <base>/install.sh | sh -s -- --endpoint=<url>
```

`<base>` is the distribution host for your environment (e.g. `get.monitorable.io`);
Monitorable's dashboard renders the full command with your server's endpoint and API
key filled in.

Run the `export` and the install inside a root shell (`sudo -s`), not via sudo's `-E`
flag — sudo-rs, Ubuntu's default sudo since 25.10, doesn't implement `-E`, and some
sudoers policies refuse it anyway; a root shell keeps the key off argv without it.

Where you can't get a root shell, fall back to `… | sudo sh -s -- --api-key=<key>` — the
key then sits in the world-readable `/proc/<pid>/cmdline` and in `/var/log/auth.log`
(`adm`-readable). `--api-key=` overrides the environment when both are given.

The installer:

- creates a dedicated, unprivileged `monitorable` user and group
- installs the `monitorable-agent` binary to `/opt/monitorable`
- installs and enables the `monitorable-agent` systemd unit
- writes the API key and endpoint to `/etc/monitorable/agent.env`, mode `0600`, owned by
  root; systemd reads it via `EnvironmentFile=` before dropping privileges, so the key is
  never in the (world-readable) unit file and never on the agent's command line
- creates `/var/lib/monitorable`, owned by the `monitorable` user; the `queue`
  subdirectory (the on-disk retry queue) is created by the `file_storage` extension the
  first time the agent starts

Re-running the command upgrades an existing install in place, including one made under
the tool's previous name (`monitorable-otelcol` / `monitorable-collector`).

## Security model

- Runs as a dedicated, unprivileged `monitorable` user — never root
- The systemd unit is sandboxed: `ProtectSystem=strict`, `NoNewPrivileges`, a private
  mount namespace, a `@system-service` syscall filter, `MemoryDenyWriteExecute`,
  `UMask=0077`, `NoExecPaths=/` with only the agent binary re-allowed (it execs nothing),
  and `DevicePolicy=closed` with a read-only allow-list for exactly the disk
  nodes SMART needs — so the `disk` group grants the agent no write access to raw devices
- SMART disk health is least-privilege and tiered by what's detected: no physical disk
  grants nothing; SATA/SAS grants `CAP_SYS_RAWIO`; NVMe additionally grants
  `CAP_SYS_ADMIN` plus a narrow udev rule for the controller device node
- The agent joins the root-equivalent `docker` group only while a Docker socket exists at
  install time; an upgrade run on a host that no longer has one removes the membership
- The installer downloads the binary, the collector config and the systemd unit template
  to temporary paths, verifies all three against the published `SHA256SUMS`, and installs
  nothing unless every one of them matches. A failed download, a mismatch or a missing
  entry leaves the previous install running and untouched (fail-closed): this is also what
  an unsupported `--version=` rollback now hits — a checksum mismatch, not a crash loop
- A non-`https://` endpoint is accepted (development installs need it) but warned about
  loudly: the API key travels in the `Authorization` header

### What `SHA256SUMS` does and does not protect

It protects against a truncated or corrupted transfer, and against a CDN serving new
checksums over stale binaries (the install fails closed instead of running a mismatched
pair).

It does **not** protect against a compromise of the distribution bucket or of the TLS
channel: `SHA256SUMS` is unsigned and is fetched from the same origin as `install.sh`
itself, so whoever can rewrite one can rewrite the other. The only root of trust today is
the install command you copy out of the Monitorable dashboard. Signing the checksum file
with a key held outside the bucket is a planned follow-up.

## Build, test, release

```bash
go build ./cmd/monitorable-agent
go test -race ./...
scripts/publish-dist.sh staging --render-only   # render the distribution locally, no upload
```

Releasing publishes a versioned distribution (binaries, checksums, configs, installer)
to the environment's R2 bucket: push a `v*` tag.

See [`distribution/README.md`](distribution/README.md) for the distribution's layout,
publishing mechanics, and the retry/offline-buffering design.

## Security

See [SECURITY.md](SECURITY.md) for how to report a vulnerability privately.

## License

[Apache License 2.0](LICENSE), like the OpenTelemetry Collector it is built on.
