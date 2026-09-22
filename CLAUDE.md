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
├── collectors/                    # Metric collectors (CPU, RAM, disk, etc.)
├── receiver/                      # OTel receiver implementation
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
  the receiver logs an actionable WARN.
- **Persistent export queue (Linux)**: the `otlp_http` exporter retries forever
  (`max_elapsed_time: 0`) with a `file_storage`-backed persistent queue at
  `/var/lib/monitorable/queue` (1440 requests); `newFactories()` in
  `cmd/monitorable-agent/main.go` registers the extension and
  `TestShippedConfigsLoad` guards config/factory drift.
- **OTLP Export**: Sends metrics to backend via OTLP/HTTP
- **API Key Auth**: `Authorization` header with server API key

## Distribution

The Linux-only installer (`distribution/install.sh`) downloads the binary, config and
systemd unit to `.tmp` paths, verifies them against a published `SHA256SUMS`, and only
then installs and starts the service. See `distribution/README.md` for the full publish
pipeline (R2 buckets per env, checksum integrity, retry/offline buffering, rollback
semantics).

## Testing

```bash
# Check agent status (after installation)
sudo systemctl status monitorable-agent

# View logs
sudo journalctl -u monitorable-agent -f
```

## Dependencies

Requires backend OTLP endpoint to be available at configured endpoint.
