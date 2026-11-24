# Monitorable agent (monitorable-agent)

Custom OTel Collector distribution with the monitorable receiver for system metrics.

## Tech Stack

- **Language**: Go 1.27+ (hard minimum: the `go` directive in go.mod)
- **Framework**: OpenTelemetry Collector core + contrib components compiled in via `newFactories()`
- **Receiver**: Custom `monitorable` receiver

## Commands

```bash
# Build the agent
go build -o monitorable-agent cmd/monitorable-agent/main.go

# Run with config
export MONITORABLE_API_KEY=your-key
export MONITORABLE_ENDPOINT=http://localhost:8080
# The shipped config's file_storage extension defaults to /var/lib/monitorable/queue,
# which a non-root dev user can't write; point it somewhere writable.
export MONITORABLE_STATE_DIR=/tmp/agent-queue
./monitorable-agent --config distribution/configs/linux/collector-config.yaml

# Cross-compile (published surface is Linux-only; see scripts/publish-dist.sh)
GOOS=linux GOARCH=amd64 go build -o monitorable-agent-linux-amd64 cmd/monitorable-agent/main.go
GOOS=linux GOARCH=arm64 go build -o monitorable-agent-linux-arm64 cmd/monitorable-agent/main.go
```

## File Structure

```
cmd/monitorable-agent/main.go   # Entry point
internal/
├── capabilities/                  # System capability detection
├── extension/writebehindstorage/  # write_behind_storage: write-behind layer for the persistent queue
├── processor/cyclebatch/          # cycle_batch: sends a collection cycle as soon as every receiver delivered
├── collectors/                    # Metric collectors (CPU, RAM, disk, etc.)
├── receiver/                      # OTel receiver implementation
├── selftelemetry/                 # Telemetry factory: console logger, no-op meters/tracers
├── sensors/                       # Temperature sensor reads (hwmon/thermal_zone)
└── version/                       # Build-time version string (ldflags injection)
```

## Key Concepts

- **Monitorable Receiver**: Custom receiver that collects system metrics
- **Linux CPU usage**: `system.cpu.time` is emitted by the `monitorable` receiver from `/proc/stat`
  (stateless, per-(cpu,state) cumulative Sum), NOT the hostmetrics `cpu` scraper — the latter wedges
  on LXC/hotplug hosts when the logical-CPU set changes between scrapes. CPU *load* still comes from
  hostmetrics `load`. Linux only — see `docs/cpu-metrics.md`.
- **Docker container snapshot**: the `monitorable` receiver probes the Docker socket each
  60s cycle and attaches a JSON snapshot of containers (running + exited/dead within 24h,
  incl. per-container CPU%, memory, net rates computed in-receiver) as the
  `docker.containers` resource attribute — same pattern as `systemd.services` /
  `smart.disks`. No `container.*` metrics are emitted any more; the backend stores the
  snapshot in Postgres (`docker_containers`, current-only). Config:
  `monitorable.docker.{mode: auto|on|off, endpoint, excluded_images, timeout}`. The
  collector user must be in the `docker` group; if the socket is present but inaccessible
  the receiver logs an actionable WARN. Each cycle it lists the containers and reads stats
  for the running ones; since v1.10.0 it inspects a container only when its list row does
  not prove the remembered restart count, start time, finish time and exit code current
  (`canReuse` in `internal/collectors/docker_memo.go`: first sight, a state change, the
  first three minutes after a start or stop, an exited container with a restart count above
  zero, once an hour, or a status text it cannot match; any doubt inspects, which is
  v1.9.0's behaviour). The memory is in-process only, rebuilt every cycle.
  `TestReuseNeverDiffersFromInspect` runs a model daemon for 5,000 cycles against a
  collector without memory. Spec: `docs/superpowers/specs/2026-11-24-lean-agent-part3-design.md`.
- **Listening ports**: the `monitorable` receiver samples `/proc/net/tcp{,6}` every 10s
  (the ports list: LISTEN = state `0A`, per-port connection counts = ESTABLISHED `01`, keyed
  by local port; the sampler also counts every state, see "TCP state counts") and,
  once per 60s cycle, attaches the `listening.ports` resource attribute plus
  `monitorable.port.connections{,.peak}` gauges (avg/peak over the window, `port` label) for the
  30 lowest ports; `all_ports` lists up to 1000. No addresses, peers, PIDs or command lines.
  Config: `monitorable.ports.{mode: auto|on|off, sample_interval, max_ports}`, defaults
  auto/10s/30, bounds 5-60s / 1-100. No privilege needed. Known limitation: ports Docker
  publishes via iptables DNAT (and kube-proxy NodePorts) keep their client connections in the
  container's network namespace, so the host-side count reads near zero (docker-proxy holds the
  LISTEN; a NodePort has no host listener at all).
- **Send when collected**: the `cycle_batch` processor (`internal/processor/cyclebatch`,
  first in the pipeline) gathers one collection cycle — one `ConsumeMetrics` per receiver —
  and sends it as one request as soon as `deliveries` have arrived (about 1.3 s after the
  tick), or after `max_wait` (10 s) for a slow or silent collector; its clock starts at the
  first delivery, and `Shutdown` hands a pending group downstream. `deliveries` must equal
  the number of receivers in the metrics pipeline (2); `TestShippedConfigsLoad` pins it.
  The stock `batch` behind it is a size limit only (`timeout: 0s`, 5,000 / 5,000): it splits
  a cycle above 5,000 datapoints, because the backend refuses a request above 20,000. Before
  v1.9.0 the stock step's free-running 60 s timer sent every sample about a minute late.
  Spec: `docs/superpowers/specs/2026-11-24-lean-agent-part2-design.md`.
- **TCP state counts**: `system.network.connections` (twelve `state` labels, `protocol=tcp`,
  hostmetrics' shape) comes from the `monitorable` receiver, counted per row by the ports
  sampler, from its newest sample (stamped with that sample's time). The shipped config
  switches the metric off in hostmetrics, whose library opens `/proc/<pid>/fd` of every
  process for it. Config `monitorable.tcp_states.mode: auto|on|off`: the default in the
  binary is `off` (so a v1.9.0 binary with a v1.8.0 config keeps hostmetrics' counts and
  sends each series once), the shipped config sets `auto`. `ports.mode: off` does not stop
  the counts: each cycle then reads the tables itself, with no sampler goroutine. Two known
  differences from hostmetrics: listeners sharing a port (`SO_REUSEPORT`) count one each,
  as `ss -ltn` shows them (the library merged them into one); and `FIN_WAIT_1`/`FIN_WAIT_2`
  carry the real counts (the library sent them as `FIN_WAIT1`/`FIN_WAIT2`, extra series).
- **Persistent export queue (Linux)**: the `otlp_http` exporter retries forever
  (`max_elapsed_time: 0`) with a `file_storage`-backed persistent queue at
  `/var/lib/monitorable/queue` (1440 requests); `newFactories()` in
  `cmd/monitorable-agent/main.go` registers the extensions and
  `TestShippedConfigsLoad` guards config/factory drift. Since v1.7.0 the queue uses
  `write_behind_storage` (`internal/extension/writebehindstorage`), a wrapper around
  `file_storage` that holds writes in memory. It writes a batch out only once the batch has
  waited 15 s (`flush_after`), when more than 4 MiB is pending, or at a graceful stop, so a
  healthy agent writes nothing to disk after start; a hard kill loses at most 15 s. The client lists
  the file's keys once at start (`storage.Walker`), so it knows exactly what is on disk.
  Every flush is one `Batch`, a consistent snapshot. The extension declares `file_storage`
  as a dependency (`Dependencies()`): the collector orders extensions by dependencies,
  not by the `service.extensions` list. A graceful stop flushes only after in-flight sends
  return (up to the 30 s HTTP timeout), so the unit's `TimeoutStopSec` (45) must stay above
  it; `TestShippedConfigsLoad` pins that. Spec:
  `docs/superpowers/specs/2026-11-07-write-behind-queue-storage-design.md`.
- **Self-telemetry**: `factories.Telemetry` is our own `internal/selftelemetry`, not the
  collector's stock `otelconftelemetry`, which links the AWS, Kubernetes, GCP and OAuth
  dependency trees (45 modules, about 15 MB of binary) for internal metrics and traces the
  shipped config turns off. It builds the stock console logger (minus the `resource` block on
  every line; sampling 10 s / first 10 / every 100th) with no-op meters and tracers, and reads
  `service::telemetry::{logs::level, logs::encoding, metrics::level}` only. Every other key
  loads and is ignored, with one WARN naming the keys; `metrics::level` other than `none` gets
  one WARN ("internal metrics are not available in this build"). `TestBinaryLinksNoStockTelemetry`
  guards the import graph and CI's "Release binary size budget" step the stripped release
  artifacts (≤ 33,000,000 B amd64, ≤ 31,000,000 B arm64). Spec:
  `docs/superpowers/specs/2026-11-24-lean-agent-build-design.md`.
- **Collected set**: the shipped `host_metrics` config is pruned to the backend's data catalog
  (16 hostmetrics names; devices matching the backend's ingest deny pattern
  `^(lo|docker0|veth.+|br-.+|loop\d+|/dev/loop\d+)$` are excluded). A metric added to the
  catalog later has to be switched on in the config, in an agent release.
  `TestShippedHostMetricsEmitOnlyStoredSeries` runs the shipped receiver and fails on any
  name or device the backend would drop.
- **OTLP Export**: Sends metrics to backend via OTLP/HTTP
- **API Key Auth**: `Authorization` header with server API key

## Distribution

The Linux-only installer (`distribution/install.sh`) downloads the binary, config and
systemd unit to `.tmp` paths, verifies them against a published `SHA256SUMS`, and only
then installs and starts the service. See `distribution/README.md` for the full publish
pipeline (R2 buckets per env, checksum integrity, retry/offline buffering, rollback
semantics). Enrollment mode (`--enroll-key=`) calls `$API_URL/v1/enroll` (`@@API_URL@@`, rendered per env) after verification and before installing.

## Testing

```bash
# Check agent status (after installation)
sudo systemctl status monitorable-agent

# View logs
sudo journalctl -u monitorable-agent -f
```

## Dependencies

Requires backend OTLP endpoint to be available at configured endpoint.
