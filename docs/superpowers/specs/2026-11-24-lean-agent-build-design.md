# Lean agent: smaller binary, less memory, no discarded data — Design

Date: 2026-11-24. Status: draft for founder review. Target: agent v1.8.0 (Part 1).

## 1. Why

The 2026 footprint benchmark shows the agent at 45.8 MB of memory (PSS) and a
57 MB binary; Beszel's agent shows 17.9 MB and 10.8 MB. On 2026-11-24 the
running v1.7.0 agent was taken apart on a development host (32 GB RAM, 21
containers, 193 systemd units, 31 network interfaces) to find out where its
resources go and which of them buy nothing.

| Resource | Measured (v1.7.0) | Finding |
|---|---|---|
| Memory (PSS) | 42.5 MB = 31.0 MB of the binary's own pages mapped from disk + 11.6 MB heap, stacks and runtime; live heap about 3.5 MB | PSS follows binary size. 20 of the 26 MB between us and Beszel are binary pages. |
| Binary | 57.2 MB = 17.5 MB debug info and symbols, 18.1 MB code, 14.3 MB function tables, 4.5 MB type data | A quarter of the code is the collector's self-telemetry stack, which the shipped config turns off. |
| Network | 9.1 KB per minute compressed, 529 datapoints | The backend discards 339 of the 529 datapoints (27 % of the bytes) at ingest. |
| CPU | 139 ms per minute, 0.23 % of one core | Small. Two avoidable parts, left to Part 2. |
| Disk | 108 KB written in 2.5 days | Nothing to do. |

Three changes remove what buys nothing. All three were built and run beside the
stock agent on the same host at the same time:

| | v1.7.0 | with the three changes |
|---|---|---|
| Binary, linux/amd64 | 57,202,801 B | 29,204,640 B |
| Binary, linux/arm64 | not measured | 26,869,920 B |
| Memory (PSS, median of 48 samples over 8 min) | 42.2 MB | 33.5 MB |
| CPU | 140 ms/min | 141 ms/min |
| Modules linked | 208 | 163 |
| Datapoints per minute | 529 | 190 |
| Compressed bytes per minute | 9,097 | 6,615 |

The backend stores exactly the same series before and after: the 190
datapoints the trimmed config sends are the 190 the backend kept out of the
stock agent's 529.

## 2. Parts

- **Part 1 (this spec, v1.8.0):** the three changes of section 3.
- **Part 2 (own spec later):** send data when it is collected (a freshly
  started agent sends every sample 59 to 60 s after collecting it, because the
  batch timer fires just before each collection); produce the TCP state counts
  from the ports sampler instead of the hostmetrics scan that tries to open
  `/proc/<pid>/fd` of every process each minute.
- **Part 3 (own spec later):** stop inspecting every container every minute
  (16 of the 46 ms of dockerd CPU each cycle costs on a 21-container host).

Parts 2 and 3 change behaviour and need their own design. Part 1 changes none.

## 3. Part 1 design

### 3.1 A telemetry factory of our own

`factories.Telemetry = otelconftelemetry.NewFactory()` is the only importer of
`go.opentelemetry.io/contrib/otelconf/x`, which links the AWS SDK, the
Kubernetes client, the GCP detectors and `golang.org/x/oauth2`. The shipped
config sets `service::telemetry::metrics::level: none`, and the stock factory
already logs "Internal trace telemetry disabled", so of everything it builds
the agent uses the logger only.

New package `internal/selftelemetry` with `NewFactory() telemetry.Factory`,
built with `telemetry.NewFactory(createDefaultConfig, telemetry.WithCreateLogger(createLogger))`.
The meter and tracer providers are the no-op defaults of
`go.opentelemetry.io/collector/service/telemetry`. It must be a package, not a
second file in `cmd/monitorable-agent`: `scripts/publish-dist.sh` compiles
`cmd/monitorable-agent/main.go` as a single file.

The logger is the stock one, minus the `resource` block:

- zap production encoder config, `EncodeTime = ISO8601TimeEncoder`, outputs
  and error outputs `stderr`, caller on, stack traces from `error` up;
- `logs::level` (default `info`) and `logs::encoding` (`console`, the default,
  or `json`);
- sampling: tick 10 s, first 10, thereafter every 100th;
- `set.BuildZapLogger` is used when it is set, `zap.Config.Build` otherwise;
- `otel.SetErrorHandler` routes SDK errors to this logger, as the stock one does;
- no `"resource": {service.instance.id, service.name, service.version}` field
  on every line. The version is in the "Starting monitorable-agent..." line.

Config: `logs::level`, `logs::encoding`, `metrics::level` (default `none`).
Every other key under `service::telemetry` is accepted and ignored
(`mapstructure:",remain"` fields at all three levels; verified against
collector v0.162.0: a config with `logs::sampling`, `metrics::readers`,
`traces` and `resource` loads). The agent warns once at start, naming the
ignored keys, and warns once when `metrics::level` is not `none` ("internal
metrics are not available in this build"). An agent that refuses to start
after an update is worse than one that ignores a setting.

Removing the import lets `go mod tidy` drop 45 modules (`go.mod` 229 → 184
lines, `go.sum` 584 → 450).

The upstream factory API is marked experimental. A collector upgrade that
changes it fails the build, not a host.

### 3.2 Release binaries without debug info

`scripts/publish-dist.sh` links with `-s -w` in addition to the version flag.
Debug info and the symbol table are never loaded, so memory does not change;
the download halves. Panic stack traces, `--version` and `go version -m` keep
working.

CI keeps the result from growing back: the cross-compile step builds with the
release flags and fails above 33,000,000 bytes (amd64) or 31,000,000 bytes
(arm64), and a test fails when the binary's import graph contains the stock
telemetry factory or anything it alone pulled in.

### 3.3 Collect only what is stored

The backend's data catalog keeps 16 of the metric names hostmetrics emits and
drops every sample whose `device` matches
`^(lo|docker0|veth.+|br-.+|loop\d+|/dev/loop\d+)$`. The shipped config stops
producing what is dropped:

```yaml
      disk:
        exclude:
          devices: ['^loop\d+$']
          match_type: regexp
        metrics:
          system.disk.merged: {enabled: false}
          system.disk.operation_time: {enabled: false}
          system.disk.weighted_io_time: {enabled: false}
          system.disk.pending_operations: {enabled: false}
      filesystem:
        # existing include_virtual_filesystems + exclude_fs_types stay
        exclude_devices:
          devices: ['^/dev/loop\d+$']
          match_type: regexp
        metrics:
          system.filesystem.inodes.usage: {enabled: false}
      network:
        exclude:
          interfaces: ['^(lo|docker0|veth.+|br-.+)$']
          match_type: regexp
      paging:
        metrics:
          system.paging.operations: {enabled: false}
          system.paging.faults: {enabled: false}
```

The 16 names that remain from hostmetrics: `system.cpu.load_average.{1m,5m,15m}`,
`system.memory.{usage,utilization,limit}`, `system.filesystem.usage`,
`system.disk.{io,operations,io_time}`,
`system.network.{io,packets,dropped,errors,connections}`, `system.paging.usage`.

The disk, network and paging blocks were run for 15 batches; the filesystem
`exclude_devices` line is new and is proven by the test of the plan's Task 2.

The agent config and the backend catalog are now coupled in one direction: a
metric added to the catalog later has to be switched on here in an agent
release. The config comment says so.

### 3.4 Compatibility and rollback

- The published config object is unversioned while binaries are versioned. The
  v1.8.0 config uses only keys the v1.7.0 binary knows (same hostmetrics
  v0.162.0), and the v1.8.0 binary reads the v1.7.0 config. Both pairs are run
  in the acceptance gate.
- The installer replaces the config on every update and keeps the previous one
  as `.prev`, so hand edits do not survive an update today either.
- Nothing changes in the backend, the dashboard or the data on the wire that
  the backend keeps.

### 3.5 Acceptance gate

Before the PR, on a host with Docker, v1.7.0 and the branch build run side by
side for 6 minutes against a local sink:

1. Branch binary: amd64 ≤ 33,000,000 B, arm64 ≤ 31,000,000 B.
2. PSS median of the branch build at least 6 MB below v1.7.0's.
3. CPU time of the branch build no more than 5 % above v1.7.0's.
4. Mean compressed body of the branch build's batches below v1.7.0's.
5. v1.7.0 binary + v1.8.0 config, and v1.8.0 binary + v1.7.0 config: both
   start, send, and log no error.

On staging, after the tag: every panel of the server page that had data under
v1.7.0 has data under v1.8.0 for 30 minutes, with no gap.

### 3.6 Public statements

- A changelog entry, dated the day v1.8.0 is on prod (the change alters what
  the agent collects, so it is always listed).
- **Founder ruling needed** before any site wording changes. The site says "a
  binary of about 60 MB, less than 50 MB of memory" in four places and cites
  the benchmark, whose page records v1.7.0 at 57.3 MB. Default proposed here:
  say "about 30 MB (since v1.8.0)" for the binary, keep "less than 50 MB of
  memory", leave the benchmark page as the record of v1.7.0. A re-measurement
  on the benchmark bed is a separate decision.

### 3.7 Out of scope

- A tighter garbage-collection target: `GOGC=50` saved 3.3 MB and cost 34 % more CPU.
- Removing what is left of the framework (the collector's pipeline graph links
  1.2 MB of a linear-algebra library; `configtls` links go-tpm): that is a fork.
- `-trimpath`: 74 KB.
- `system.memory.utilization`: stored and never queried; a backend decision.
- The listening-ports sampling interval: each sample makes the kernel walk its
  TCP hash table, whose size follows RAM; sampling less often loses peak accuracy.

## 4. Evidence

Measurement tools and the experiment patch are in the platform checkout,
`docs/superpowers/notes/2026-11-24-agent-resource-analysis/`
(`experiment.patch`: the experimental factory, a pprof hook, an OTLP batch
decomposer and a `/proc/net/tcp` cost benchmark; `collector-config.trimmed.yaml`;
`sink.py`, a local OTLP sink that records wire and plain sizes).
