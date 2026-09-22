# CPU Metrics: Design Notes

## Background

On LXC guests (and any host subject to CPU hotplug or Proxmox cpuset re-pinning), CPU usage
metrics would flatline while CPU load averages remained healthy. This was not a configuration
problem — the collector was running, other metrics were flowing, and `system.cpu.load_average.*`
was correct. Only `system.cpu.time` (and derived utilisation) was missing.

## Root Cause: hostmetrics `cpu` scraper wedge

The upstream OTel `host_metrics` receiver's `cpu` scraper computes per-core utilisation by
diffing successive readings of `/proc/stat`. It keys its "previous times" map by the set of
logical CPU IDs seen on the first scrape. If a subsequent scrape returns a *different* set of
CPU IDs (e.g. Proxmox re-pins the container's cpuset, a CPU is hot-added/removed, or ARM cores
park/unpark), the scraper tries to look up a core that is not in its stored map, returns an
error (`cannot find TimesStat for cpu<N>`), and the OTel pipeline treats the entire `cpu`
scrape as failed — dropping all CPU metrics for that interval.

Critically, `previousCPUTimes` is **never refreshed** on error, so the mismatch persists
across all future scrapes until the collector is restarted. The result is a permanent flatline.

## Fix: stateless cumulative counter from `/proc/stat`

`system.cpu.time` is now emitted by the custom `monitorable` receiver (Linux only), reading
`/proc/stat` directly on each scrape interval and emitting **cumulative monotonic Sum** data
points — one per `(cpu, state)` pair. There is no diffing, no stored previous state, and
therefore no possibility of a wedge. If a CPU disappears between scrapes, its series simply
stops incrementing; if a new one appears, a new series starts. The Prometheus `rate()` function
used by the backend handles both cases correctly.

## `/proc/stat` column → OTel state mapping

| Column index | `/proc/stat` field | `state` attribute value |
|:---:|---|---|
| 1 | user | `user` |
| 2 | nice | `nice` |
| 3 | system | `system` |
| 4 | idle | `idle` |
| 5 | iowait | `wait` |
| 6 | irq | `interrupt` |
| 7 | softirq | `softirq` |
| 8 | steal | `steal` |

`guest` and `guest_nice` (columns 9–10) are excluded because they are already counted inside
`user`/`nice` in the kernel accounting; including them would double-count CPU time.

## Series shape

```
system_cpu_time_total{
    cpu="cpu0",          # logical CPU id from /proc/stat ("cpu0", "cpu1", …)
    state="user",        # one of the states above
    server_id="…",       # injected by backend from the API key
    user_id="…",         # injected by backend from the API key
}
```

The backend's existing PromQL query is unchanged:

```promql
avg without(cpu)(rate(system_cpu_time_total{state="user"}[5m])) * 100
```

## Platform scope

**The published distribution is Linux-only** (amd64 + arm64): no Windows or macOS binary
is built or served today (see `scripts/publish-dist.sh`). The Windows/macOS rows below
record what the shipped config would do on those platforms if they were ever built — they
are not an existing build.

| Platform | Source of `system.cpu.time` | Source of `system.cpu.load_average.*` |
|---|---|---|
| Linux (published) | `monitorable` receiver (`/proc/stat`) | hostmetrics `load` scraper |
| Windows (not built) | hostmetrics `cpu` scraper | hostmetrics `load` scraper |
| macOS (not built) | hostmetrics `cpu` scraper | hostmetrics `load` scraper |

The LXC/hotplug wedge is a Linux-specific phenomenon tied to how the upstream scraper uses
`/proc/stat`. Windows and macOS use platform APIs that do not have the same CPU-set-change
problem, so they continue to use the hostmetrics `cpu` scraper unchanged.
