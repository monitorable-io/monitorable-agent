# Lean agent Part 2: send when collected, TCP state counts without the process scan — Design

Date: 2026-11-24. Status: approved by the founder on 2026-11-24 (design and plan; execution Native). Target: agent v1.9.0.
Part 1: `2026-11-24-lean-agent-build-design.md` (v1.8.0, live).

## 1. Why

Two things the v1.8.0 agent does cost something and buy nothing.

**It sends every sample a minute late.** The batch step has a 60 s timer that
starts when the agent starts, a moment before the first collection, and never
re-aligns. Each batch therefore leaves just before the next collection.
Measured on the development host on 2026-11-24, v1.8.0 against a local sink:

| | v1.8.0 |
|---|---|
| First request after process start | 60.2 s |
| Age of the samples in a request when it arrives | 59.0 to 60.0 s |

A new user waits that minute after the install command: the docs say "the
first batch arrives about 60 seconds after the agent service starts" in eight
places on four pages. Every alert also fires a minute later than the data allows. And a hard
kill or power cut loses up to a minute of collected samples, because they are
still in the batch step's memory.

**It scans every process to count TCP sockets.** `system.network.connections`
comes from the hostmetrics network scraper, which calls a library function
that first tries to open `/proc/<pid>/fd` of every process (to map sockets to
processes, which the agent never uses) and then reads `/proc/net/tcp` and
`/proc/net/tcp6`. One call costs 13.3 ms of CPU on the development host (326
processes), once a minute, out of about 141 ms a minute in total. The
`monitorable` receiver's ports sampler already reads the same two tables every
10 s and throws away every state but LISTEN and ESTABLISHED.

## 2. Goals, non-goals, anti-goals

Goals:

1. A sample leaves the host within seconds of being taken, from the first
   cycle on, and stays that way for the life of the process.
2. The agent opens no `/proc/<pid>/fd`.
3. The backend stores the same series, with the same names and labels, that
   the dashboard, the public API and the MCP server read today. Nothing
   changes in the backend or the dashboard.

Non-goals:

- Part 3 (stop inspecting every container every minute).
- A different collection interval, sampling interval, or more samples a minute.
- Dropping the eight TCP states nothing reads. They stay, so the series set
  stays the same.
- A re-run of the footprint benchmark. That is a separate decision once
  v1.9.0 is live (Part 1, decision Q5).

Anti-goals (the change has failed if any of these happens, even with goals 1
to 3 met):

- More than one request per collection cycle in normal operation.
- A request the backend refuses for its size that v1.8.0 would have split.
- A gap in any dashboard panel across the update.
- An agent that does not start after an update.

## 3. Approaches

### 3.1 Send when collected

| | What it is | Trade-off |
|---|---|---|
| **1. A batch step of our own whose clock starts at the first sample (chosen)** | Gathers one collection cycle into one request and sends it at once | About 100 lines plus tests that we own. Everything after it (queue, write-behind, backend) sees what it sees today, one request a minute |
| 2. No batch step | Each collector's output leaves as collected | Two requests a minute per agent for good: ingest requests double fleet-wide, bytes per minute rise about 5 % (measured: 6,665 B of bodies and two sets of headers against 6,600 B and one), the queue must double to keep a day |
| 3. Stock batch step with a 5 s timer | Config only | The timer still runs free: the delay is anywhere from 0 to 5 s, and whether a minute goes as one request or two depends on the phase at start, so the numbers are luck per host |

Dropped early: the exporter's built-in batching. Its clock does start at the
first item, but it sits after the queue. After an outage it would merge many
minutes into one large request, and the backend answers an oversized request
with 413, which the agent drops.

The founder chose 1 on 2026-11-24: it is the only one that fixes the delay and
leaves the request count, the queue and the drain behaviour as they are.

### 3.2 TCP state counts

| | What it is | Trade-off |
|---|---|---|
| **A. The ports sampler counts every state (chosen)** | It already parses every row of both tables every 10 s; it also keeps a count per state | No extra reads. The count is from the newest sample, up to 10 s before the collection |
| B. A separate count at collection time | One more read of both tables per cycle | Independent of the sampler, but a seventh read each minute on the default setup, and each read makes the kernel walk its whole TCP table |

Dropped early: `/proc/net/sockstat` (constant cost, but it gives only
TIME_WAIT and a total) and netlink `sock_diag` (a new code path for no gain
while the sampler already reads the tables).

The founder chose A on 2026-11-24, with these four rulings:

- **Which number:** the count from the newest sample of the minute, not a mean
  over the window. It means what it means today and can be checked against
  v1.8.0 side by side.
- **Which states:** all twelve labels hostmetrics sends today.
- **`ports.mode: off`:** only removes the ports list. The state counts continue.
- **Shared-port listeners:** the real count is accepted (section 5.4), and the
  spec and the changelog say so.

## 4. Design: send when collected

### 4.1 A new processor, `cycle_batch`

New package `internal/processor/cyclebatch`, registered in `newFactories()`.
It is a metrics processor with two settings:

```yaml
processors:
  cycle_batch:
    deliveries: 2   # the number of receivers in the pipeline
    max_wait: 10s
```

A *delivery* is one `ConsumeMetrics` call from a receiver. Each receiver makes
one per collection cycle. Behaviour:

- A delivery is appended to the pending group (its `ResourceMetrics` are moved,
  not copied). The call returns at once with no error, as the stock batch step
  does.
- The first delivery of a group starts a `max_wait` timer.
- The group is sent downstream as one `pmetric.Metrics` as soon as
  `deliveries` deliveries have arrived, or when the timer fires, whichever is
  first. The timer is then stopped, and the step holds nothing and has no
  running timer until the next delivery.
- `Shutdown` hands a pending group downstream before it returns. The exporter
  is stopping at that moment, so the group is either sent or written to the
  queue file and sent at the next start, exactly as the stock step's last
  batch is today. Nothing that was collected is lost at a graceful stop.
- A downstream error is logged at Warn, once per failed send.
- The step declares that it mutates data.

Validation: `deliveries` ≥ 1, `max_wait` > 0. Defaults: `deliveries: 1` (every
delivery is passed through at once), `max_wait: 10s`.

Sending on the count, and not after a fixed wait, is my call. Measured on the
development host, the `monitorable` receiver delivers 0.33 to 0.56 s after its
tick. hostmetrics makes its first scrape 1 s after start (its `initial_delay`)
and ticks from there, so its delivery comes 1.23 s after the receiver's tick.
A fixed wait would have to be longer than the gap between the two (0.7 to
0.9 s) with a margin, and every cycle would pay it. With the count, the
request leaves when the second delivery arrives, about 1.3 s after the tick,
and `max_wait` only matters in a cycle where one collector is slow or silent.

`max_wait: 10s` is my call too. The receiver makes its single delivery after
Docker, systemd and SMART have answered, and each has a timeout: Docker 5 s
every cycle, systemd 10 s every 5 minutes, SMART 10 s every 15 minutes, the
temperature read 10 s. hostmetrics opens the group about 1.2 s after the tick,
so 10 s covers a Docker daemon that uses its whole timeout, with 5 s to spare.
A cycle slower than that has a hung D-Bus or a hung disk read, which is a
fault and not normal operation.

What such a cycle looks like: the first delivery goes out alone 10 s after it
arrived. The late one then starts a group of its own and goes out 10 s after
it arrived. That minute has two requests. The next cycle is back to one.

`TestShippedConfigsLoad` pins `cycle_batch::deliveries` to the number of
receivers in the metrics pipeline, so adding a receiver without changing the
count fails CI.

### 4.2 The stock batch step stays, as a size limit only

This is my call and differs from what the approaches message said ("it
replaces the stock batch step"). Today `send_batch_max_size: 5000` splits a
cycle of more than 5,000 datapoints into several requests. The backend relies
on it: it refuses a request above 20,000 datapoints with 413, which the agent
drops, and its headroom test assumes the largest agent request is 5,000.
Splitting a `pmetric.Metrics` by datapoint count is the complicated part of
the stock step and there is no reason to own a copy of it.

So the stock step stays behind `cycle_batch`, with its timer off:

```yaml
processors:
  cycle_batch:
    deliveries: 2
    max_wait: 10s
  batch:
    # Size limit only. timeout 0 passes every group through at once and
    # ignores send_batch_size; a group above send_batch_max_size is split.
    timeout: 0s
    send_batch_size: 5000
    send_batch_max_size: 5000
  resource:
    # unchanged

service:
  pipelines:
    metrics:
      receivers: [host_metrics, monitorable]
      processors: [cycle_batch, batch, resource]
      exporters: [otlp_http]
```

`send_batch_size` has to be stated: its default (8192) is above 5,000 and the
config would be refused. Checked on v1.8.0 with `timeout: 0s`: both deliveries
passed through within 1.2 s of start, no errors.

### 4.3 What stays the same

- One request per collection cycle; the same 190 datapoints a minute on the
  development host; the same bytes.
- The queue (1,440 requests = one day), write-behind storage, retry settings
  and the drain after an outage (one request per minute of data, none merged).
- Sample timestamps, except for the TCP state counts (section 5.3).
- Collection intervals: 60 s for both receivers.

### 4.4 What changes for the better

- First data about 1.3 s after the service starts, where it took 60 s.
- Samples arrive about 1 s old, where they were 59 to 60 s old.
- A hard kill or power cut loses at most the cycle being gathered (about 1 s
  normally, 10 s at worst) plus the write-behind window (15 s), where it could
  lose 60 s plus 15 s.

CPU and network charts need two samples, so they fill about a minute after
start (today: two minutes).

## 5. Design: TCP state counts from the ports sampler

### 5.1 The sampler keeps a count per state

`summarizeSockets` in `internal/collectors/ports_parse.go` already sees every
parsed row of both tables. It also returns a count of rows per state code. The
`PortsCollector` keeps the counts and the time of its newest good sample, and
hands them out with `Snapshot` in `PortsResult` (not in `PortsSnapshot`, which
is the JSON of the `listening.ports` attribute and must not change).

A row counts under the state the kernel printed for it. Rows the parser
already drops as malformed are not counted. A state code outside the table
below is ignored.

| Code | Label | Code | Label |
|---|---|---|---|
| `01` | `ESTABLISHED` | `07` | `CLOSE` |
| `02` | `SYN_SENT` | `08` | `CLOSE_WAIT` |
| `03` | `SYN_RECV` | `09` | `LAST_ACK` |
| `04` | `FIN_WAIT_1` | `0A` | `LISTEN` |
| `05` | `FIN_WAIT_2` | `0B` | `CLOSING` |
| `06` | `TIME_WAIT` | none | `DELETE` (always 0) |

These are the twelve labels of hostmetrics v0.162.0 (`allTCPStates`). All
twelve are sent every cycle, zero included.

### 5.2 The receiver sends the metric

Once per collection cycle the `monitorable` receiver appends one metric, in
the same shape hostmetrics uses, so the backend converts it to the same series:

- name `system.network.connections`, unit `{connections}`, description "The
  number of connections.";
- a Sum, not monotonic, cumulative, integer values;
- twelve datapoints, attributes `protocol="tcp"` and `state=<label>`;
- timestamp: the time of the sample the counts came from.

It rides in the receiver's own resource and scope. Checked against the
backend: the stored labels are `protocol`, `server_id`, `state` and `user_id`
only, so the resource and scope it arrives in do not reach the series.

A test runs the hostmetrics factory with only the network scraper and this
metric switched on, captures what it emits with a sink (as
`hostmetrics_shipped_test.go` does), and compares it with what our builder
emits: name, type, monotonicity, temporality, unit, attribute keys and the
twelve declared states. (hostmetrics' generated builder is in an `internal`
package and cannot be imported.)

The first cycle must carry the counts, because the first request is now what a
new user sees. If no sample exists yet when the first cycle runs (the sampler
goroutine and the first cycle start together), the cycle takes one itself.

### 5.3 Settings

A new block on the receiver, in the pattern of the existing ones:

```yaml
  monitorable:
    tcp_states:
      mode: auto   # auto | on | off
```

- `auto`: send the counts; stay quiet (one Debug line per change) when
  `/proc/net/tcp` cannot be read.
- `on`: the same, but one Warn per change when it cannot be read.
- `off`: never send them.

The default **in the binary** is `off`, and the shipped config sets `auto`.
This is my call. Every other block defaults to `auto`, but here an old config
still has hostmetrics sending the metric, and `off` makes a v1.9.0 binary with
a v1.8.0 config behave exactly as v1.8.0: each series once a minute.

How the two blocks combine:

| `ports.mode` | `tcp_states.mode` | Table reads | Sent |
|---|---|---|---|
| auto/on | auto/on | sampler, every `sample_interval` | ports list, per-port gauges, state counts |
| auto/on | off | sampler, every `sample_interval` | ports list, per-port gauges |
| off | auto/on | one read inside each collection cycle, no sampler goroutine | state counts |
| off | off | none | none |

The shipped config turns the metric off in hostmetrics, which removes the
library call and with it the process scan:

```yaml
      network:        # system.network.{io,packets,dropped,errors}
        exclude:
          interfaces: ['^(lo|docker0|veth.+|br-.+)$']
          match_type: regexp
        metrics:
          system.network.connections: {enabled: false}
```

`TestShippedHostMetricsEmitOnlyStoredSeries` checks emitted names against the
names the backend stores, so it passes either way and the name stays in its
list. A new assertion pins the change: the shipped `host_metrics` receiver
emits no `system.network.connections`.

### 5.4 Where the numbers differ from v1.8.0

1. **Listeners that share a port.** The library drops a socket whose local
   address, remote address and state equal one it has already seen. Processes
   that each listen on the same port (`SO_REUSEPORT`: nginx with `reuseport`,
   Envoy and HAProxy workers) are therefore one LISTEN today. Counting rows
   gives one per listening socket, which is what `ss -ltn` shows. On such a
   host the LISTEN line of the Connections panel steps up once, at the update.
   The development host has none: ten paired reads gave identical counts.
2. **FIN_WAIT.** hostmetrics declares `FIN_WAIT_1` and `FIN_WAIT_2`, but the
   library names them `FIN_WAIT1` and `FIN_WAIT2`. Today the declared labels
   are always 0 and the real counts go to two extra series that appear only
   when such a socket exists. From v1.9.0 the real counts go to the declared
   labels and the two extra series stop. Nothing reads any of the four.
3. **Time of the sample.** The count is from the newest sample, up to
   `sample_interval` (10 s) before the collection, and carries that sample's
   time. hostmetrics counted at the scrape. At one point a minute this moves
   nothing on a chart.

## 6. Compatibility and rollout

- **v1.9.0 config needs the v1.9.0 binary** (a new processor type and a new
  receiver key). This differs from Part 1, where both pairs ran. The installer
  already refuses to install a binary older than its `MIN_VERSION` next to the
  current config; `MIN_VERSION` becomes `v1.9.0` in the same release, as it
  became `v1.8.0` in Part 1.
- **v1.9.0 binary with a v1.8.0 config** starts and behaves as v1.8.0: stock
  batch timing, state counts from hostmetrics only. It is in the acceptance gate.
- **A failed update** restores the previous binary and config together
  (`.prev`), as today.
- **The queue file** is unchanged. Requests queued by v1.8.0 are sent by v1.9.0.
- Release order as for v1.8.0: tag, staging publish, staging acceptance, prod
  publish, fleet update.

## 7. Acceptance gate

### 7.1 Before the PR, on the development host

v1.8.0 and the branch build run side by side for 10 minutes, each against its
own local sink (`sink.py`, extended to keep every request body).

| # | Check | Pass when | v1.8.0, measured |
|---|---|---|---|
| 1 | First request after process start | ≤ 5 s | 60.2 s |
| 2 | In every later request, arrival time minus the newest sample time | ≤ 5 s | 59.0 to 60.0 s |
| 3 | Requests to `/v1/metrics` | one per collection cycle, plus one at stop if a group was pending | one per cycle |
| 4 | Series (name and label set) seen over the run | the same set as v1.8.0, apart from `state="FIN_WAIT1"` and `"FIN_WAIT2"` | reference |
| 5 | Mean compressed body after the first request | ≤ 1.02 × v1.8.0's | 6,600 B |
| 6 | Opens of `/proc/<pid>/fd` over 3 minutes (`strace`) | 0 | 321 in one scrape (322 processes) |
| 7 | CPU time over the run | ≤ v1.8.0's (expected: about 13 ms a minute less) | 141 ms/min |
| 8 | PSS median; release binaries | ≤ v1.8.0 + 1 MB; within the CI budgets (33,000,000 B amd64, 31,000,000 B arm64) | 34.5 MB; 29.2 MB |
| 9 | TCP counts, ten paired reads in one process (rows against the library) | LISTEN equal in all ten; every other state equal in at least eight | identical in 10 of 10 on 2026-11-24 |
| 10 | SIGTERM while a group is pending | the group is at the sink before exit, or in the queue file and at the sink after the next start; exit within `TimeoutStopSec` | same rule for its last batch |
| 11 | Sink answers 503 for 5 minutes, then accepts | five queued requests arrive, one per cycle, none merged, no datapoint missing | same |
| 12 | A cycle of 12,000 datapoints (test) | leaves as requests of at most 5,000 | same |
| 13 | Branch binary with the v1.8.0 config | starts, logs no error, first request about 60 s after start, each state series once a minute | reference |
| 14 | `ports.mode: off` with `tcp_states.mode: auto` | no `listening.ports`, no per-port gauges, twelve state datapoints each cycle | n/a |

Check 2 is on the samples taken in the cycle. The per-port gauges and the
state counts carry the sampler's time and may be up to `sample_interval`
older; for them the limit is 15 s.

Check 7 is noisy at this size (Part 1 needed pooled runs). Check 6 is the
strict form of the same goal.

### 7.2 On staging, after the tag

On staging 19, updated from v1.8.0:

1. The series for the server in the 20 minutes before the update and in 20
   minutes after it: the same names and label sets, apart from the two
   `FIN_WAIT` extras.
2. Every panel of the server page that had data under v1.8.0 has data for 30
   minutes, with no gap.
3. After `systemctl restart monitorable-agent`, a sample newer than the
   restart is stored within 15 s.
4. No warnings, no `error` lines, `NRestarts=0`.

## 8. Public statements

- **Changelog**, dated the day v1.9.0 is on prod. A user sees two things:
  metrics appear seconds after install and arrive a minute sooner from then
  on; and on hosts where several processes listen on one port, the
  Connections panel now counts each listener.
- **Docs wording, founder ruling needed.** Eight places on four pages of
  `monitorable-landing/src/content/docs/docs/` say the first batch arrives
  about a minute after the service starts: `faq.md` (the description and "How
  quickly do metrics appear?"), `getting-started.md` (the description and two
  places in the text), `agent/install.md` (two places) and
  `troubleshooting/no-data.md` ("Give a fresh install 60 seconds"). Proposed
  default: "The first batch arrives a few seconds after
  the agent service starts. CPU and network charts need two samples, so they
  fill about a minute later." The wording goes live with the changelog, not
  before.
- The dashboard's "Install the agent on a host to see it here within a
  minute" stays true and stays.
- `agent_docs/procedures/collector-distribution.md`, the agent's `CLAUDE.md`
  and `distribution/README.md` are updated with the release.

## 9. Left to the builder

- The internals of `cyclebatch` (locking, which goroutine sends), as long as
  section 4.1 holds and the race detector is clean.
- The start timestamp of the state datapoints: the backend does not read it.
- Log texts, test names, and how the sink keeps bodies for checks 2 and 4.
- How staging check 3 reads the stored sample (a raw export from
  VictoriaMetrics is the obvious way; the address probe also contacts the
  backend at start, so `last_seen_at` is not usable for it).

## 10. Out of scope

- hostmetrics `initial_delay` (1 s). Removing it would bring the request
  about a second earlier; not worth a behaviour change.
- The ports sampling interval (Part 1, section 3.7).
- A mean or a peak of the state counts over the window.
- UDP, or connection tracking counts.

## 11. Evidence

Measured on 2026-11-24 on the development host with the Part 1 tools
(platform checkout, `docs/superpowers/notes/2026-11-24-agent-resource-analysis/`):

- v1.8.0 with the shipped config: requests at 60.2 s after start and every
  60.0 s after; samples stamped 13:40:43.6 to 13:40:44.6 arrived at 13:41:43.67.
- v1.8.0 with the batch step taken out of the pipeline: the receiver's
  delivery 0.54 s after start, then 0.33 to 0.56 s after each tick;
  hostmetrics 1.23 s after start and after each tick. Bodies 4,585 B and
  2,080 B against 6,600 B batched.
- v1.8.0 with `batch::timeout: 0s`: the same two requests, no errors.
- One ports sample, spaced as the agent spaces them: 5.1 ms of CPU. One
  library TCP scan, spaced the same way: 13.3 ms.
- `strace` of v1.8.0 through its first hostmetrics scrape: 321 opens of
  `/proc/<pid>/fd` with 322 processes running.
- Ten paired reads, one second apart (rows of both tables against the
  library, in one process): identical counts in all ten.
- Stored series for `system.network.connections` on the development stack: 14
  states per server (the twelve declared and the two `FIN_WAIT` extras), labels
  `protocol`, `server_id`, `state`, `user_id`.
