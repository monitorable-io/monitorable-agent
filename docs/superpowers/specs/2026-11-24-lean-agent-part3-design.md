# Lean agent Part 3: inspect a container only when it has changed — Design

Date: 2026-11-24. Status: draft for founder review. Target: agent v1.10.0.
Part 1: `2026-11-24-lean-agent-build-design.md` (v1.8.0, live).
Part 2: `2026-11-24-lean-agent-part2-design.md` (v1.9.0, live).

## 1. Why

Every 60 s the agent asks Docker three things: the list of containers, then
for each container its details (*inspect*), then for each running container
its resource counters (*stats*). Inspect supplies four fields of the
`docker.containers` snapshot: restart count, start time, finish time and exit
code. All four change only when a container starts or stops. On a settled host
the agent asks for them again every minute and gets the same answer.

Measured on the development host on 2026-11-24 (Intel N150, 26 containers: 21
running, 5 exited), CPU per collection cycle:

| Step | dockerd | containerd and its shims | The agent | Total |
|---|---|---|---|---|
| List | 12 ms | 0 | 2 ms | 14 ms |
| Inspect, 26 calls | 18 ms | 3 ms | 16 ms | 37 ms |
| Stats, 21 calls | 26 ms | 42 ms | 10 ms | 78 ms |
| Whole cycle | 56 ms | 46 ms | 28 ms | 130 ms |

(Section 11 has the method. The Part 1 figure, "16 of the 46 ms of dockerd
CPU", counted dockerd only and is confirmed: 18 of 56 ms.)

Two things stand out:

- Inspect costs dockerd a third of what the agent makes it do, and costs the
  agent 16 of the 28 ms it spends on Docker, which is 16 of the 103 ms a
  minute it spends in total.
- All five exited containers on this host stopped months ago. The agent
  inspects each of them every minute, reads the finish time, and drops the
  container because it is past the 24-hour limit.

## 2. Goals, non-goals, anti-goals

Goals:

1. On a host where no container started or stopped in the last three
   minutes, the agent makes no inspect call, apart from one per container per
   hour (section 4.4) and one per cycle for an exited container that Docker
   had restarted (section 4.2, rule 5).
2. The `docker.containers` snapshot is the same as v1.9.0 would send in the
   same cycle: the same containers, the same fields, the same values. A
   restart, a stop or a new container shows in the first cycle after it
   happens, as today.
3. Nothing changes in the backend, the dashboard, the alerts, the public API,
   the MCP server, the agent's config or the data on the wire.

Non-goals:

- The stats calls. They cost twice what inspect does, most of it outside
  dockerd, and are a candidate Part 4 with its own design (section 10).
- The list call. It is the presence probe and it carries what this design
  checks against.
- A setting to switch the new behaviour off.

Anti-goals (the change has failed if any of these happens, even with goals 1
to 3 met):

- A stale restart count, start time, finish time or exit code in the snapshot
  for longer than v1.9.0 would have shown it, outside the three cases of
  section 4.7.
- A container missing from the snapshot, or kept in it, where v1.9.0 would
  have done otherwise.
- More Docker calls in any cycle than v1.9.0 makes.

## 3. Approaches

| | What it is | Trade-off |
|---|---|---|
| **1. Remember, and check against the list (chosen)** | The agent keeps each container's four fields. The list it already fetches carries a status text ("Up 3 days", "Exited (255) 5 months ago") that Docker builds from the same start time, finish time and exit code. The agent builds the same text from what it remembers and inspects only when the two differ | Leans on the wording of that text, unchanged for about ten years. A daemon that words it differently never matches, and the agent inspects every cycle as today: more CPU, never wrong data. No new connection, no background work |
| 2. Listen to Docker's event stream | A second, permanent connection; Docker pushes "started", "died" and similar, and the agent inspects the containers named | Depends on no text. Adds a background routine and reconnect handling. An event lost silently leaves stale values with nothing to notice, so it needs a periodic full refresh as well. About three times the code, and harder to test |

Dropped early: inspecting on a five-minute timer (a restart would show up to
five minutes late in the Uptime column and in the container-down alert's
"running for two minutes" check), and never inspecting (the list text has no
exact times and no restart count).

The founder chose 1 on 2026-11-24, and ruled that Part 3 covers inspect only,
with the stats calls noted as a candidate Part 4.

Checked before the choice: on the development host the list's status text
equals the text built from the inspect times for 26 of 26 containers.

A reviewer then read the first draft as the builder would and found three
sequences in which a matching text did not prove the values unchanged. Rules
3 and 5 and the slack in rule 7 of section 4.2 come from that review, and
section 4.7 lists what is left.

## 4. Design

Everything is inside `DockerCollector` (`internal/collectors/docker.go`). The
receiver, the config and the wire format are not touched.

### 4.1 What the agent remembers

Per container ID, after a successful inspect:

- restart count, start time, finish time, exit code (the times at full
  precision, not rounded to milliseconds);
- *state*: the state the list showed for the container in the last cycle
  that confirmed the entry;
- *checked*: the start of that cycle. The time is taken **before** the
  cycle's list call. An entry is confirmed by an inspect or by a reuse
  (section 4.2);
- *inspected*: the start of the cycle that last inspected it.

The memory is rebuilt every cycle from the containers handled in that cycle,
the same way `prevCPU` and `prevNet` are. A removed container therefore drops
out, and the size is bounded by the existing 200-container cap. It lives in
the process only: after an agent restart every container is inspected once.

### 4.2 The decision, per container, per cycle

The exclude filter, the newest-first sort and the 200 cap run first, as today.
`created` and `removing` are skipped, as today. Then the container is
**inspected**, as today, unless every one of these holds, in which case the
remembered values are **reused**:

1. The list shows it as `running`, `paused` or `exited`. (`restarting`,
   `dead` and anything Docker adds later are always inspected.)
2. There is a remembered entry for its ID.
3. The list shows the same state as the entry's *state*.
4. *inspected* is less than an hour ago (section 4.4).
5. It is not the case that the container is `exited`, its remembered finish
   time is within the 24-hour limit, and its remembered restart count is
   above zero (see "Why rule 5").
6. The list's status text equals the text built from the remembered values
   and the current time:
   - `running` or `paused`: `Up <d>`, with *d* the time since the remembered
     start. It matches when the list's text is exactly that, or starts with
     it followed by ` (` (the health and paused suffixes: `Up 3 days
     (healthy)`, `Up 2 hours (Paused)`).
   - `exited`: `Exited (<code>) <d> ago`, with the remembered exit code and
     *d* the time since the remembered finish. It matches only when equal.

   *d* is written by `units.HumanDuration` from `github.com/docker/go-units`,
   the function the daemon uses.
7. The remembered time is old enough for the text to prove something. Let
   *age* be the time since the remembered start (running, paused) or finish
   (exited), *gap* the time since *checked*, and *slack* 60 s. Required:
   *age* is longer than *gap + slack*, **and** `HumanDuration(age)` differs
   from `HumanDuration(gap + slack)`.

**Why rule 7.** The text is coarse: "About a minute" covers 60 to 119 s. Take
a container that started 20 s before one cycle and restarted right after that
cycle read it. If the next cycle comes a moment late, Docker says "Up About a
minute" (the new container has been up 60 s) and the agent, from the old start
time, expects "Up About a minute" (80 s): a match on wrong values. Rule 7
closes this. Anything that happened to the container and that the agent has
not seen carries a time newer than *checked* minus *slack*, so Docker's text
for it is `HumanDuration` of something shorter than *gap + slack*. The
function only moves to a later wording as the duration grows, and each wording
belongs to one range. So when the remembered time reads as a later wording
than *gap + slack* does, and Docker's text agrees with it, Docker's time
cannot be a new one.

**Why the slack.** Docker stamps a finish time when the process exits, but
makes it visible only after it has cleaned up (it allows itself 30 s to delete
the task and 2 s for the output streams), and the list reads what is visible.
A stop can therefore be up to that much older than the agent's last look and
still unseen. 60 s covers it with room. The value is my call.

With the default 60 s cycle, *gap + slack* is about two minutes, so a
container is inspected every cycle for about its first three minutes after a
start or a stop, and never after. *gap* is not exactly 60 s: the Docker step
runs after the systemd and SMART steps of the same tick, and their duration
varies. Nothing depends on it being exact. With a longer
`collection_interval`, after a failed cycle, or after the agent was suspended,
*gap* is longer and the rule widens by itself.

*gap* is the larger of two readings: the difference on the monotonic clock
and the difference on the wall clock (`now.Sub(checked)` and
`now.Round(0).Sub(checked.Round(0))`). The first survives a wall-clock step,
the second counts time spent suspended. This is my call: one line of code,
and without it a clock step of under a minute at the wrong moment could let a
wrong match through.

Requiring *age* to be longer than *gap + slack*, and not only a different
wording, is my call as well. On one host it is always true once the wordings
differ. With `endpoint` pointing at a daemon on another machine whose clock is
ahead, it is what keeps the argument valid.

**Why rule 3.** A change of state is the one thing v1.9.0 and this design
both see in the list itself, and an entry confirmed while the container ran
holds the finish time and exit code of the run *before*. Checking an `exited`
row against those is never useful. It also closes one case that rule 7 alone
does not: Docker up to 28.x first writes its own clock time as the finish time
of a container that is waiting to be restarted, and replaces it with the
earlier, true exit time if the container is then stopped. The state goes from
`restarting` to `exited`, so the agent inspects.

**Why rule 5.** One change is invisible in the list. A manual `docker start`
that fails early (a cgroup, device or runtime error) has already set the
restart count back to 0 and leaves the times, and a non-zero exit code,
untouched. So an exited container with a restart count above zero is
inspected every cycle for as long as it is in the snapshot. A reset from 0 to
0 changes nothing, and a container past the 24-hour limit is not sent at all.
This is my call; the alternative was to accept a stale restart count on such
a container for up to an hour.

The current time used in rules 6 and 7 is taken after the list call returns
(the existing `now`). Docker formatted its text a few milliseconds earlier, so
when a duration crosses from one wording to the next the texts can differ for
one cycle. That costs one inspect and nothing else.

### 4.3 After the decision

- **Reuse:** restart count, start time, exit code and finish time come from
  memory. An exited container is kept or dropped by the 24-hour rule applied
  to the remembered finish time, so a container that stopped long ago costs
  no call at all. *checked* moves to this cycle; *state* and *inspected*
  stay.
- **Inspect, success:** exactly today's handling, and the result is
  remembered with *state* set to what the list showed, and *checked* and
  *inspected* set to this cycle.
- **Inspect, failure, or a reply with no state:** exactly today's handling (a
  running container stays in the snapshot without the four fields, an exited
  one is left out), and nothing is remembered for that ID, so the next cycle
  asks again.

The stats call for a running container is unchanged.

### 4.4 The hourly refresh

Rule 4 inspects every container at least once an hour whatever the text says.
Section 4.2 argues that a reuse cannot be wrong under stated conditions; the
refresh is what bounds the cases outside them (section 4.7), and any I have
not found, to one hour. It costs 26 inspects an hour on the development host
where v1.9.0 makes 1,560. The interval is my call.

The hour runs from each container's own last inspect, so after an agent start
they all fall in the same cycle once an hour. That one cycle costs what every
cycle costs today.

### 4.5 A daemon whose text is different

If the list's text never has the expected form (a Docker-compatible daemon
with its own wording, or a future Docker that changes it), rule 6 never holds
and every container is inspected every cycle. That is v1.9.0's behaviour,
with the cost of building one string per container. The agent logs nothing
special for it. A unit test pins `HumanDuration`'s wording at every boundary,
so a library update that changes it fails CI and gets looked at.

### 4.6 What a host looks like

| Container | Inspect calls |
|---|---|
| Running or paused, started more than about three minutes ago | none (one an hour) |
| Exited more than about three minutes ago, restart count 0, any age | none (one an hour) |
| Exited within the last 24 hours with a restart count above zero | every cycle |
| Started or stopped within about the last three minutes | every cycle |
| Restarted between two cycles | the first cycle after, then as "started within three minutes" |
| `restarting` or `dead` | every cycle, as today |
| First seen (new container, or agent just started) | that cycle |

On the development host in a quiet minute: 0 inspect calls, where v1.9.0
makes 26.

### 4.7 Known limits

In these cases a remembered value can differ from what an inspect would
return, until the hourly refresh or the container's next start or stop,
whichever comes first. I accept them; the founder should know they exist.

- **The clock steps back and then forward again by the same large amount
  within one cycle, and a container restarts in between.** Docker's age for
  the new start then includes the step, and can read like the old one.
- **`docker.endpoint` points at a daemon on another machine, and that
  machine's clock steps forward between a restart and the next cycle.** The
  agent cannot see a step on a remote clock. A constant difference between
  the two clocks is covered.
- **Docker takes longer than the 60 s slack to make a stop visible.** Its own
  timeouts add up to 32 s.

## 5. What stays the same

- The snapshot: field names, values, which containers are in it, the 24-hour
  rule for exited containers, the 200 cap, the exclude filter, truncation and
  control-character stripping.
- When a change shows: a restart, a stop, a start or a new container is in
  the first cycle after it, as today.
- Per-container CPU, memory and network figures (the stats calls).
- The config. No new key.
- The backend, the dashboard, the alerts, the API and the MCP server.

Expected on the development host, per cycle:

| | v1.9.0 | v1.10.0, quiet minute |
|---|---|---|
| Inspect calls | 26 | 0 |
| dockerd CPU for the agent | 56 ms | 38 ms |
| dockerd, containerd and shims together | 102 ms | 80 ms |
| The agent's CPU on Docker | 28 ms | 12 ms |
| The agent's CPU in total, per minute | 103 ms | about 87 ms |

## 6. Compatibility and rollout

- **Config:** unchanged, so the v1.10.0 binary runs with the v1.9.0 config and
  the v1.9.0 binary with the v1.10.0 config. `MIN_VERSION` in the installer
  stays `v1.9.0`.
- **Dependencies:** `github.com/docker/go-units` moves from indirect to direct
  in `go.mod`. It is already linked (the Docker API types import it), so the
  binary does not grow beyond the new code.
- **Rollback:** the previous binary. There is no state on disk.
- **Version:** v1.10.0 is my call, following v1.8.0 and v1.9.0 for the first
  two parts. Nothing a user can see changes, so v1.9.1 would be just as
  correct.
- Release order as for v1.9.0: tag, staging publish, staging acceptance, prod
  publish, fleet update.

## 7. Acceptance gate

### 7.1 Before the PR, on the development host

v1.9.0 and the branch build run side by side for 12 minutes, each against its
own local sink, started together so their cycles fall within a second of each
other. Each process runs its own copy of its binary file (two processes on
one file share pages and halve each other's PSS). Throwaway containers, named
with a `p3-` prefix and removed afterwards, are driven by a script during
minutes 3 to 6, each action in the middle of a cycle: start one, restart it
(`docker restart`), kill it, start one that exits 0, one that exits 3, pause
and unpause one, remove one, and run one with `--restart on-failure` that
exits 1 every 20 s or so and is removed at minute 6. One running and one
exited (code 3) throwaway container stay to the end. Minutes 0 to 2 and 10 to
12 are quiet.

| # | Check | Pass when | v1.9.0 |
|---|---|---|---|
| 1 | `docker.containers`, cycle by cycle, both agents: the container IDs, and per container `state`, `health`, `exit_code`, `restart_count`, `started_at_unix_ms`, `finished_at_unix_ms`, `name`, `image` | identical in every cycle. One exception: a container for which `docker events` shows an event within 1 s of that cycle's read may differ in that cycle, and must be identical in the next | reference |
| 2 | Inspect requests on the Docker socket from the branch build, in the quiet minutes: every cycle after the first, and every cycle from minute 10 | 0 | one per container per cycle (26 before the script starts) |
| 3 | Inspect requests after each scripted action | the changed container is inspected in the first cycle after the action, in every cycle until its start or stop is at least 2 minutes old, and in none once it is 4 minutes old; no other container is inspected | every container, every cycle |
| 4 | Requests on the Docker socket per cycle, any kind | never more than v1.9.0 in the same cycle | reference |
| 5 | CPU time of the agent over the run | ≤ v1.9.0's (expected: about 16 ms a minute less) | 103 ms/min |
| 6 | PSS median; release binaries | ≤ v1.9.0 + 1 MB; within the CI budgets (33,000,000 B amd64, 31,000,000 B arm64) | 34.0 MB; 29.2 MB |
| 7 | A list whose status text has another wording (unit test) | every container inspected every cycle, snapshot as today | n/a |
| 7a | Each rule of section 4.2 (unit tests): a state change, an entry an hour old, an exited container with a restart count above zero, a text that matches while the remembered time is too young, a gap stretched by a missed cycle and by a wall-clock step | inspect in each case; reuse in the matching case next to it | n/a |
| 8 | Agent log over the run | no `error` line, no warning that v1.9.0 does not also print | reference |

Check 5 is noisy at this size, as it was in Parts 1 and 2. Check 2 is the
strict form of the same goal: the Docker-side saving is the measured cost of
the calls that check 2 proves are gone (section 11), because dockerd's CPU
cannot be split between two agents running side by side.

### 7.2 On staging, after the tag

On a staging server that runs containers, updated from v1.9.0:

1. The Containers panel shows the same rows, with the same state, uptime and
   exit codes, before and after the update.
2. After `docker restart` of one container, its Uptime in the panel restarts
   from zero within two collection cycles.
3. No warnings, no `error` lines, `NRestarts=0` over 30 minutes.

## 8. Public statements

- **Founder ruling needed: changelog entry or not.** Nothing a user sees,
  does, receives or pays for changes, so the project's rule says no entry.
  Default proposed here: no changelog entry; the GitHub release notes say
  what changed. An entry would be justified only as part of the lean-agent
  story, and then it needs the founder's wording.
- No site or docs wording depends on this change.
- The agent's `CLAUDE.md` ("Docker container snapshot") and
  `agent_docs/procedures/collector-distribution.md`, if it describes the
  per-cycle Docker calls, are updated with the release.

## 9. Left to the builder

- Names, the shape of the remembered entry, and where the decision function
  lives, as long as it is a pure function of (list row, remembered entry,
  cycle start, current time) that table tests cover without a daemon.
- Log lines. A Debug line with the number of inspect calls in the cycle is
  enough; nothing at Info or above.
- How check 2 to 4 count requests on the socket: `strace` on the agent, or a
  small logging proxy in front of the socket with `docker.endpoint` pointed
  at it.
- The script that drives the throwaway containers, and how check 1 pairs the
  two agents' cycles.
- Which staging server section 7.2 uses.

## 10. Out of scope

- **Candidate Part 4: stats without Docker.** The 21 stats calls cost 26 ms of
  dockerd, 42 ms of containerd and its shims and 10 ms of the agent per cycle,
  twice what inspect costs. On the development host (cgroup v2, systemd
  driver) the agent's user can read the same counters straight from the
  kernel: `cpu.stat`, `memory.current` and `memory.stat` in the container's
  cgroup, and `/proc/<pid>/net/dev` for the network. That replaces where the
  panel's CPU, memory and network figures come from, so it needs a parity
  check against Docker's figures and a fallback for hosts where the files are
  laid out differently or not readable (cgroup v1, rootless Docker, Podman).
  It also needs each container's process ID, which comes from inspect: the
  memory this part adds is what would carry it. Its own spec, later, if the
  founder wants it.
- Docker's event stream (approach 2).
- Keeping the memory across an agent restart.
- Fewer calls for `restarting` and `dead` containers, and for exited
  containers with a restart count above zero. All are rare.
- Dropping long-exited containers before the 200 cap is applied.

## 11. Evidence

Measured on 2026-11-24 on the development host. Tools and raw results are in
the platform checkout,
`docs/superpowers/notes/2026-11-24-agent-resource-analysis/part3/`.

- **The tool** (`zz-dockercost.go`) uses the agent's own Docker client. It
  runs a cycle in one of five shapes (list only; list and inspect of the
  running containers; list and inspect of the exited ones; list and stats;
  the full v1.9.0 cycle) and reads the on-CPU time of dockerd, containerd,
  every containerd shim and itself before and after, from
  `/proc/<pid>/task/*/schedstat`.
- **The table in section 1** is the dense run: one cycle a second for 30 s
  per shape, two rounds, with an idle block of the same length subtracted.
  Per cycle, both rounds: full cycle dockerd 55.9 and 55.6 ms, containerd
  15.4 and 15.2, shims 30.4 and 31.3, the caller 28.1 and 27.3; list and
  stats 37.4 and 37.6, 14.3 and 15.1, 28.6 and 27.7, caller 12.1 and 12.1;
  list only dockerd 11.7 and 11.9, caller 2.1 and 2.2. Inspect is the full
  cycle minus list-and-stats; stats is list-and-stats minus list.
- **Cross-check, single cycles from an idle core** (36 per shape, shuffled
  order, a second of sleep before each, idle windows subtracted): full cycle
  dockerd 58.7 ms, all three daemon-side processes 113.9 ms, caller 27.9 ms;
  list and stats 36.7, 80.3 and 11.8 ms.
- **The same cycles on a warm core** cost about half (full cycle: dockerd
  28.7 ms, daemon side 56.5 ms, caller 15.6 ms). The host's CPU runs the
  `powersave` governor between 0.7 and 3.6 GHz, and a cycle that starts from
  sleep, as the agent's does, runs at the low clock. Absolute figures are for
  this host; the proportions held in all three runs.
- **A trap for whoever measures next:** a fixed rotation of shapes lined up
  with the containers' 10 s healthchecks and put their cost on one shape
  (run 2, discarded). Shuffle the order and jitter the gaps.
- **Background on this host**, with nothing asking: dockerd about 12 ms of
  CPU per second, containerd 7, shims 11 (healthchecks). The agent's whole
  cycle is under 2 ms per second averaged over the minute, which is why the
  saving cannot be read off process totals and has to be attributed per call.
- **Status text:** for 26 of 26 containers the list's `Status` equalled `Up
  <HumanDuration(now − StartedAt)>` or `Exited (<code>) <HumanDuration(now −
  FinishedAt)> ago` built from inspect, allowing the health suffix.
  `HumanDuration` in go-units v0.5.0 has eleven wordings, each for one range,
  in increasing order of duration.
- **Who reads the four fields:** the Containers panel (Uptime from the start
  time, the "Exited (code)" label), the container-down alert (exit code, and
  whether a running container started at least two minutes ago), and the
  container tool of the API and MCP server (all four).
- **Daemon behaviour behind rules 3, 5 and 7**, read in the moby v28.3.3
  source: `daemon/start.go` resets the restart count before the start is
  attempted, and returns early for a running or paused container;
  `container/state.go` `SetRestarting` writes `time.Now()` as the finish time
  and `SetStopped` the shim's exit time; `daemon/monitor.go` allows 30 s for
  the task delete and 2 s for the streams before the exit is made visible;
  the list reads a snapshot without waiting for the container's lock.
- **Where the Docker step sits in the cycle:** after the systemd and SMART
  steps of the same tick (`performUnifiedCollection`), so the time between
  two Docker steps is 60 s give or take the difference in their duration.
- **Response sizes:** the list 58.8 KB for 26 containers; one inspect 6 to 9
  KB; one stats reply 1.9 KB.
