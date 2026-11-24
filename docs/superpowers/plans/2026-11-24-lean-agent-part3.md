# Lean agent, Part 3 (agent v1.10.0) — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans (Native) or superpowers:subagent-driven-development to implement this plan task-by-task, as the founder chooses. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship agent v1.10.0, which inspects a container only when it may have changed (today: every container every minute) and sends the same `docker.containers` snapshot as v1.9.0.

**Architecture:** `DockerCollector` remembers, per container ID, what the last successful inspect returned. Each cycle a pure decision function compares the list row's state and status text with the text built from the remembered values (`units.HumanDuration`, the daemon's own formatter) and says reuse or inspect. Any doubt means inspect, which is v1.9.0's behaviour. Nothing outside `internal/collectors` changes.

**Tech Stack:** Go 1.27; `github.com/moby/moby/client` v0.6.1 and `github.com/moby/moby/api` v1.56.1 (already used); `github.com/docker/go-units` v0.5.0 (already in the module graph, becomes a direct dependency); standard `testing` with the existing `fakeAPI`.

**Spec:** `docs/superpowers/specs/2026-11-24-lean-agent-part3-design.md` (read it first; every rule and value below comes from it, and section 4.2 says why each rule exists).

## Global Constraints

- The snapshot is byte-for-byte what v1.9.0 would send in the same cycle: same containers, fields and values. No change to `ContainerStats`, the receiver, the config, the backend or the app.
- A container is reused only when all seven rules of spec 4.2 hold. Constants: refresh 1 hour (`inspectRefresh`), slack 60 s (`inspectSlack`), retention 24 h (the existing `exitedRetention`).
- Expected texts, exactly: `Up <d>` for `running` and `paused` (a match is equality, or the list text starting with it followed by ` (`); `Exited (<code>) <d> ago` for `exited` (equality only). `<d>` is `units.HumanDuration`.
- The time since the last look is the larger of the monotonic and the wall-clock difference. The cycle's "checked" reading is taken before the list call, the "now" reading after it.
- Never more Docker calls in a cycle than v1.9.0 makes. The stats call is untouched and keeps its own `time.Now()`.
- The memory lives in the process only and is rebuilt every cycle from the containers handled in that cycle.
- Log nothing at Info or above. One Debug line per cycle with the number of inspect calls.
- No new module. `go mod tidy` moves `github.com/docker/go-units` out of the `// indirect` block and changes nothing else.
- Release binaries stay within the CI budgets: ≤ 33,000,000 B (amd64), ≤ 31,000,000 B (arm64).
- Local builds and tests: `env -u GOROOT GOTOOLCHAIN=auto go …`. Stop processes by PID, never `pkill -f`.
- Commits follow Conventional Commits; no Claude attribution lines. Branch `feat/lean-agent-part3`, created from `docs/lean-agent-part3` (which holds the spec and this plan), PR to `main`. The founder merges, tags and dispatches prod; no session merges or runs `/ship` on its own.
- No changelog entry (founder ruling, 2026-11-24).

## Review Focus

Conditions the spec implies that a first pass of tests would miss. Each has its test pinned to the owning task.

1. **A container that crash-loops faster than the cycle.** Its state reads `running` or `restarting` at random and its restart count grows every cycle; the snapshot must carry the current count each time (Task 2, `TestCollectCrashLoopTracksRestartCount`, and the model test).
2. **A cycle that did not happen** (the Docker step timed out, the list failed, the host was suspended). The next cycle must distrust every entry whose age reads like the time that passed (Task 1, the "gap" rows; Task 2, `TestCollectMissedCycleWidensTheGap`).
3. **A health change without a restart.** `healthy` to `unhealthy` changes the list text's suffix only: no inspect, and `health` in the snapshot still follows the list (Task 2, `TestCollectHealthFlipDoesNotInspect`).
4. **A container recreated under the same name** (`docker compose up` after an image update). It has a new ID: inspected at once, and the old ID's entry is gone (Task 2, `TestCollectRecreatedContainerGetsItsOwnEntry`).
5. **A daemon that words its status differently, or sends none.** Every container is inspected every cycle and the snapshot is as today (Task 2, `TestCollectUnknownStatusWordingInspectsEveryCycle`; spec 7.1 check 7).

---

### Task 1: The decision function

**Files:**
- Create: `internal/collectors/docker_memo.go`, `internal/collectors/docker_memo_test.go`
- Modify: `go.mod` (by `go mod tidy`)

**Interfaces:**
- Produces:

```go
package collectors

const (
	inspectRefresh = time.Hour        // spec 4.4
	inspectSlack   = 60 * time.Second // spec 4.2, "why the slack"
)

// instant is one reading of both clocks.
type instant struct {
	Wall time.Time     // wall clock, monotonic reading stripped (t.Round(0))
	Mono time.Duration // monotonic clock, from a process-wide origin
}

// since returns the time from then to i: the larger of the two clocks' differences.
func (i instant) since(then instant) time.Duration

// systemInstant reads both clocks now.
func systemInstant() instant

// inspectMemo is what the last successful inspect said about one container.
type inspectMemo struct {
	RestartCount int
	StartedAt    time.Time // zero when Docker gave none
	FinishedAt   time.Time // zero when Docker gave none
	ExitCode     int
	State        string  // the list's state in the cycle that last confirmed the entry
	Checked      instant // start of that cycle, read before its list call
	Inspected    instant // start of the cycle that last inspected
}

// canReuse reports whether m still describes the container whose list row has
// this state and status text (spec 4.2, rules 1 to 7). m is nil when nothing is remembered.
func canReuse(state, status string, m *inspectMemo, now instant) bool
```

- [ ] **Step 1: Write the failing tests** in `docker_memo_test.go`.

`TestHumanDurationWording` pins the formatter at every boundary (spec 4.5), as a table of duration to text: `-5s` and `0` and `999ms` "Less than a second"; `1s` "1 second"; `59s` "59 seconds"; `60s` and `119s` "About a minute"; `120s` "2 minutes"; `59m59s` "59 minutes"; `60m` and `89m` "About an hour"; `90m` "2 hours"; `47h29m` "47 hours"; `47h30m` "2 days"; `335h` "13 days"; `336h` "2 weeks"; `1439h` "8 weeks"; `1440h` "2 months"; `17519h` "24 months"; `17520h` "2 years".

`TestHumanDurationNeverGoesBack`: walk durations from 0 in 1 s steps to 3 h, then in 1 h steps to 3 years; record each wording the first time it appears; fail if a wording that was left appears again. (Rule 7 rests on this.)

`TestInstantSince`, with `base := instant{Wall: time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC), Mono: 1000 * time.Second}`:

| Later reading | `since(base)` |
|---|---|
| Wall +60 s, Mono +60 s | 60 s |
| Wall +3 h, Mono +60 s (suspended) | 3 h |
| Wall +30 s, Mono +60 s (clock stepped back) | 60 s |
| Wall −30 min, Mono +60 s | 60 s |

`TestCanReuse`, a table. `now` is `base` above advanced by 60 s on both clocks; unless a row says otherwise the entry has `State` equal to the row's state, `Checked: base`, `Inspected` 10 minutes before `now`, `RestartCount: 0`, and `ago(d)` means `now.Wall.Add(-d)`.

| # | Row state, status | Entry | Want |
|---|---|---|---|
| 1 | running, `Up 3 days` | started `ago(72h)` | reuse |
| 2 | running, `Up 3 days (healthy)` | same | reuse |
| 3 | paused, `Up 3 days (Paused)` | same, State paused | reuse |
| 4 | running, `Up 30 seconds` | same as 1 | inspect |
| 5 | running, `Up 3 days` | nil | inspect |
| 6 | restarting, `Restarting (1) 5 seconds ago` | finished `ago(5s)`, State restarting | inspect |
| 7 | dead, `Dead` | State dead | inspect |
| 8 | exited, `Exited (0) 3 days ago` | finished `ago(72h)`, code 0, State running | inspect |
| 9 | running, `Up 3 days` | as 1, `Inspected` exactly 1 h before `now` | inspect |
| 10 | running, `Up 3 days` | as 1, `Inspected` 59m59s before `now` | reuse |
| 11 | exited, `Exited (255) 5 months ago` | finished `ago(150*24h)`, code 255 | reuse |
| 12 | exited, `Exited (0) 5 months ago` | same as 11 | inspect |
| 13 | exited, `Exited (255) 5 months ago (x)` | same as 11 | inspect |
| 14 | exited, `Exited (1) 2 hours ago` | finished `ago(2h)`, code 1, restart count 3 | inspect |
| 15 | exited, `Exited (1) 25 hours ago` | finished `ago(25h)`, code 1, restart count 3 | reuse |
| 16 | exited, `Exited (1) 2 hours ago` | finished `ago(2h)`, code 1, restart count 0 | reuse |
| 17 | running, `Up About a minute` | started `ago(80s)` | inspect |
| 18 | running, `Up 2 minutes` | started `ago(130s)` | inspect |
| 19 | running, `Up 3 minutes` | started `ago(180s)` | reuse |
| 20 | running, `Up 2 minutes` | started `ago(130s)`, `Checked` 59.9 s before `now` | reuse |
| 21 | running, `Up 3 hours` | started `ago(3h10m)`, `Checked` 3 h before `now` on both clocks | inspect |
| 22 | running, `Up 3 hours` | started `ago(3h10m)`, `Checked` 3 h before on Wall only (Mono 60 s) | inspect |
| 23 | running, `Up 3 hours` | started `ago(3h10m)` | reuse |
| 24 | running, `Up 30 minutes` | started `ago(3m)` | inspect |
| 25 | running, `running since Tuesday` | as 1 | inspect |
| 26 | running, `` (empty) | as 1 | inspect |
| 27 | running, `Up Less than a second` | started 30 minutes after `now` (remote clock ahead) | inspect |
| 28 | exited, `` (empty) | finished zero, code 0 | inspect |

- [ ] **Step 2: Run them, expect a compile failure**

Run: `env -u GOROOT GOTOOLCHAIN=auto go test ./internal/collectors/ -run 'HumanDuration|InstantSince|CanReuse'`
Expected: FAIL, `undefined: canReuse` (and `instant`).

- [ ] **Step 3: Implement `docker_memo.go`.** `systemInstant` reads `time.Now()` once: `Wall` is `t.Round(0)`, `Mono` is `t.Sub(processStart)` with a package-level `processStart = time.Now()`. `since` is the larger of `i.Mono - then.Mono` and `i.Wall.Sub(then.Wall)`. The decision, in full, because the order and the comparisons are the design:

```go
func canReuse(state, status string, m *inspectMemo, now instant) bool {
	if m == nil || state != m.State { // rules 2 and 3
		return false
	}
	var ref time.Time
	switch state { // rule 1
	case "running", "paused":
		ref = m.StartedAt
		want := "Up " + units.HumanDuration(now.Wall.Sub(ref))
		if status != want && !strings.HasPrefix(status, want+" (") { // rule 6
			return false
		}
	case "exited":
		ref = m.FinishedAt
		if m.RestartCount > 0 && now.Wall.Sub(ref) <= exitedRetention { // rule 5
			return false
		}
		if status != fmt.Sprintf("Exited (%d) %s ago", m.ExitCode, units.HumanDuration(now.Wall.Sub(ref))) { // rule 6
			return false
		}
	default:
		return false
	}
	if now.since(m.Inspected) >= inspectRefresh { // rule 4
		return false
	}
	age, horizon := now.Wall.Sub(ref), now.since(m.Checked)+inspectSlack // rule 7
	return age > horizon && units.HumanDuration(age) != units.HumanDuration(horizon)
}
```

Then `env -u GOROOT GOTOOLCHAIN=auto go mod tidy`; `git diff go.mod go.sum` shows only the `go-units` line leaving the indirect block.

- [ ] **Step 4: Run the tests, expect PASS**

Run: `env -u GOROOT GOTOOLCHAIN=auto go test -race ./internal/collectors/`
Expected: `ok`.

- [ ] **Step 5: Commit**

```bash
git add internal/collectors/docker_memo.go internal/collectors/docker_memo_test.go go.mod go.sum
git commit -m "feat(docker): decide from the container list whether an inspect is needed"
```

### Task 2: The collector reuses what it remembers

**Files:**
- Modify: `internal/collectors/docker.go` (`DockerCollector`, `NewDockerCollector`, `Collect`, `snapshotFor`), `internal/collectors/docker_compute.go` (`dockerTimeMs`), `internal/collectors/docker_test.go`, `CLAUDE.md` ("Docker container snapshot")
- Create: `internal/collectors/docker_model_test.go`

**Interfaces:**
- Consumes: `instant`, `systemInstant`, `inspectMemo`, `canReuse` from Task 1.
- Produces (all unexported, same package):

```go
// DockerCollector gains:
//   memo  map[string]inspectMemo // rebuilt every cycle
//   clock func() instant         // systemInstant; overridable in tests

// dockerTime parses Docker's RFC3339Nano timestamps; the zero sentinel
// ("0001-01-01T00:00:00Z") and unparseable values give the zero time.
func dockerTime(s string) time.Time

// dockerTimeMs keeps its signature and results; it becomes dockerTime + UnixMilli (0 for zero).
```

Behaviour to keep exactly, from the current `snapshotFor`:

- An inspect reply with a nil `State`: a running container keeps the reply's `RestartCount` and no times; an exited or dead one is left out. Nothing is remembered.
- An inspect error: a running container stays without the four fields; an exited or dead one is left out. Nothing is remembered.
- An exited or dead container with a zero finish time, or one older than `exitedRetention`, is left out.

New behaviour:

- `Collect` reads `start := d.clock()` before `ContainerList` and `now := d.clock()` after it. `now.Wall` replaces `time.Now()` for the retention check; the stats path keeps its own `time.Now()`.
- Per container that passes the `created`/`removing` skip: `canReuse(state, c.Status, entry-or-nil, now)`. Reuse fills the four fields from the entry and stores it in the new map with `Checked = start`. Otherwise inspect as today and, on a reply with a `State`, store a new entry with `State` = the list's state, `Checked = Inspected = start`.
- An entry is stored **also when the container is then left out of the snapshot** for being past retention. That is what makes a long-exited container free in the next cycle.
- After the loop `d.memo` is replaced by the new map, and one Debug line `docker: cycle` carries `containers` (rows handled) and `inspected` (inspect calls made).

- [ ] **Step 1: Extend the test helpers** in `docker_test.go`: `fakeAPI` counts inspect calls per ID (`inspectCalls map[string]int`, incremented also when it returns an error); `fakeClock{at instant}` with `advance(d time.Duration)` moving both readings and `stepWall(d)` moving the wall clock only; `newCollectorWithClock(api, clock)` sets `d.clock`; `upStatus(now, started)` returns `"Up " + units.HumanDuration(now.Sub(started))` and `exitedStatus(now, finished, code)` the exited form. Every existing test keeps passing untouched: their rows have no status text, so they inspect every cycle.

- [ ] **Step 2: Write the failing tests** in `docker_test.go`. Each drives several cycles with `fakeClock`, sets each row's `Status` before every `Collect`, and asserts both the snapshot and `api.inspectCalls`.

| Test | Setup and cycles | Assert |
|---|---|---|
| `TestCollectReusesInspectOnQuietHost` | `c1` running, started 3 days ago, restart count 2, status with ` (healthy)`; `c2` exited 5 months ago, code 255. Three cycles 60 s apart | calls after cycle 1: 1 each; after cycle 3: still 1 each. `c1`'s `RestartCount` and `StartedAtUnixMs` equal in all three cycles. `c2` never in the snapshot |
| `TestCollectInspectsAfterRestart` | `c1` as above for two cycles; before cycle 3 the fake's inspect returns a start 20 s ago and restart count 3, status `Up 20 seconds` | cycle 3 inspects; the snapshot carries the new start and 3 |
| `TestCollectYoungContainerInspectedUntilThreeMinutes` | `c1` started 30 s before cycle 1; four cycles 60 s apart, status from `upStatus` | calls 3 after cycle 3 (ages 30, 90, 150 s), still 3 after cycle 4 (210 s) |
| `TestCollectStateChangeInspects` | `c1` running 3 days for two cycles; cycle 3 the row is `exited`, status `Exited (0) 10 seconds ago`, inspect returns finish 10 s ago, code 0 | cycle 3 inspects; snapshot `state` exited, `exit_code` 0, `finished_at_unix_ms` set |
| `TestCollectHourlyRefresh` | `c1` running, started 3 days before cycle 1; 61 cycles 60 s apart | 2 calls in total: cycle 1 and cycle 61 |
| `TestCollectExitedWithRestartCountInspectsEveryCycle` | `c1` exited 2 h ago, code 1, restart count 3; three cycles | 3 calls; `restart_count` 3 in each snapshot |
| `TestCollectInspectErrorIsNotRemembered` | `c1` running 3 days; cycle 1 with `inspectErr` set, cycles 2 and 3 without | cycle 1: container present, `StartedAtUnixMs` 0; cycle 2: inspected, fields present; cycle 3: no call |
| `TestCollectNilStateIsNotRemembered` | `c1` running; inspect reply with `State: nil`, `RestartCount: 4`; two cycles | 2 calls; `RestartCount` 4 and no start time in both |
| `TestCollectCrashLoopTracksRestartCount` | `c1` over five cycles alternating `running` (`Up 12 seconds`) and `restarting` (`Restarting (1) 3 seconds ago`), inspect returning restart count 1, 2, 3, 4, 5 | 5 calls; the snapshots carry 1 to 5 |
| `TestCollectMissedCycleWidensTheGap` | `c1` started 90 s and `c2` 10 minutes before cycle 1; `clock.advance(30*time.Minute)`; cycle 2 with `Up 31 minutes` and `Up 40 minutes` | cycle 2 inspects `c1` (its age reads like the gap) and reuses `c2` |
| `TestCollectSuspendWidensTheGap` | same two containers, but `clock.stepWall(30*time.Minute)` and `advance(60*time.Second)`; cycle 2 with `Up 32 minutes` and `Up 41 minutes` | cycle 2 inspects `c1` and reuses `c2` |
| `TestCollectHealthFlipDoesNotInspect` | `c1` running 3 days, `Health` healthy then unhealthy, status suffix following | 1 call; `health` is `healthy` then `unhealthy` |
| `TestCollectPausedContainer` | `c1` running 3 days (cycle 1), paused with `Up 3 days (Paused)` (cycles 2, 3), running again (cycle 4) | calls after each cycle: 1, 2, 2, 3; `StartedAtUnixMs` equal throughout |
| `TestCollectRecreatedContainerGetsItsOwnEntry` | cycle 1 `c1` named `app`; cycle 2 `c2` named `app`, `c1` gone | cycle 2 inspects `c2`; `len(d.memo) == 1` and it is `c2`'s |
| `TestCollectUnknownStatusWordingInspectsEveryCycle` | `c1` running, `c2` exited 1 h ago; statuses `running since Tuesday` and empty; three cycles | 3 calls each; snapshots equal to what the same fake gives a collector with no memory |
| `TestCollectMemoBoundedByCap` | 250 running containers, each with a status and an inspect reply that has a `State`; two cycles | `len(d.memo) == 200` after each |
| `TestCollectExcludedImageIsNotRemembered` | two containers, one matching `excluded_images` | no entry and no inspect call for the excluded one |
| `TestCollectLogsInspectCount` | Debug logger; quiet host, two cycles | the second cycle's `docker: cycle` line has `inspected=0` |

- [ ] **Step 3: Write the model test** in `docker_model_test.go`, `TestReuseNeverDiffersFromInspect`. It checks the anti-goal directly: a collector with memory against one without, on the same daemon, for thousands of cycles.

`modelDaemon` implements `dockerAPI`. Per container it keeps a list of *versions* `{state, startedAt, finishedAt, exitCode, restartCount, visibleFrom}`; `ContainerList` and `ContainerInspect` both answer from the newest version with `visibleFrom <= now` (the model's clock), and the list's `Status` is built as the daemon builds it:

```go
func modelStatus(v version, now time.Time) string {
	switch v.state {
	case "running":
		return "Up " + units.HumanDuration(now.Sub(v.startedAt))
	case "paused":
		return "Up " + units.HumanDuration(now.Sub(v.startedAt)) + " (Paused)"
	case "restarting":
		return fmt.Sprintf("Restarting (%d) %s ago", v.exitCode, units.HumanDuration(now.Sub(v.finishedAt)))
	default: // exited
		return fmt.Sprintf("Exited (%d) %s ago", v.exitCode, units.HumanDuration(now.Sub(v.finishedAt)))
	}
}
```

A version that starts a container is visible 0 to 50 ms after its `startedAt`; one that stops it, 0 to 32 s after its `finishedAt` (spec 4.2, "why the slack"); never earlier than the version before it. `ContainerStats` returns `sampleStats`.

Between two cycles each container gets, with probability 0.15, one event at a uniformly random moment of the interval (so some fall within milliseconds of a cycle):

| From | Event | New versions |
|---|---|---|
| running | stop | exited, finish = t, code 0, 1 or 137 |
| running | crash under a restart policy | restarting (finish = t, code 1, count +1), then running (start = t + 0.1 to 2 s) |
| running | crash, then `docker stop` during the backoff, old daemon | restarting (finish = t + 0.3 s, count +1), then exited with finish moved **back** to t |
| running | `docker restart` | exited (finish = t), then running (start = t + 0.05 to 1 s, count 0) |
| running | pause | paused, times unchanged |
| paused | unpause | running, times unchanged |
| exited | start | running (start = t, count 0) |
| exited | start, exits again within 5 s | running, then exited (new finish, code 0 or 1) |
| exited | start that fails early | exited, times unchanged, count 0, code 128 if it was 0 |

Cycles are 59.5 to 60.5 s apart; one in twenty is followed by a gap of 2 minutes to 3 hours, and in one in fifty the wall clock alone jumps forward by up to 3 hours. 20 containers with start times spread from seconds to months before the first cycle, five of them exited with restart counts between 0 and 4; 5,000 cycles; `rand.New(rand.NewPCG(3, 2026))`.

Each cycle: the collector under test (one instance for the whole run) and a fresh `NewDockerCollector` (no memory) both run `Collect` on the same clock. Assert the two snapshots have the same IDs in the same order and, per container, equal `State`, `RestartCount`, `StartedAtUnixMs`, `FinishedAtUnixMs` and `ExitCode`. At the end assert the collector under test made fewer than a quarter of the other's inspect calls, so the test cannot pass by always inspecting.

- [ ] **Step 4: Run, expect failures**

Run: `env -u GOROOT GOTOOLCHAIN=auto go test ./internal/collectors/ -run 'TestCollect|TestReuse'`
Expected: the new tests FAIL (inspect counts too high, `d.memo` undefined); the old ones pass.

- [ ] **Step 5: Implement** the changes listed under "Behaviour to keep" and "New behaviour". The reuse path and the inspect path must fill the four fields through one shared piece of code that takes an `inspectMemo`, so they cannot drift apart.

- [ ] **Step 6: Run everything, expect PASS**

Run: `env -u GOROOT GOTOOLCHAIN=auto go test -race ./...`
Expected: every package `ok`.

- [ ] **Step 7: Prove the model test bites.** One at a time, and reverting each: set `inspectSlack` to 0; delete the `state != m.State` condition; delete the rule 5 condition; delete the `age > horizon` half of rule 7's result. `TestReuseNeverDiffersFromInspect` must fail for the first three. For the fourth it may pass (the model has no remote clock; `TestCanReuse` row 27 covers it): note which. If the model test passes with rule 3 or rule 5 removed, the model is missing the event that needs it; fix the model, not the rule.

- [ ] **Step 8: Update `CLAUDE.md`,** "Docker container snapshot": the receiver lists containers every cycle, reads stats for the running ones, and inspects a container only when its list row does not prove the remembered restart count, start time, finish time and exit code are current (first sight, a state change, the first three minutes after a start or stop, an exited container with a restart count above zero, once an hour, or a status text it cannot match). Point to the spec.

- [ ] **Step 9: Commit**

```bash
git add internal/collectors/ CLAUDE.md
git commit -m "feat(docker): inspect a container only when it has changed"
```

### Task 3: Acceptance run, PR, review

Spec 7.1. Needs the dev server (Docker running, image `alpine:3` present). `$W` is a scratch directory outside the repo. Tools from the platform checkout, `docs/superpowers/notes/2026-11-24-agent-resource-analysis/`: `part2/sink2.py`, `part2/zz-accept.go`, `part2/run10.sh` as the pattern. New scripts go in `part3/` there; nothing of this is committed to the agent repo.

The dev host's own agent reports the throwaway containers to the dev backend for the length of the run; a container-down rule bound to dev server 1 will open and resolve incidents for them.

- [ ] **Step 1: Tools.**
  - `sockproxy.py <listen.sock> <log.jsonl>`: a unix-socket proxy in front of `/var/run/docker.sock` that copies bytes both ways and logs one line per request seen from the client (`ts`, method, path). An inspect is `GET /v<api>/containers/<id>/json`; the list is `/containers/json`; stats is `/containers/<id>/stats`.
  - `zz-accept`: add the `docker.containers` resource attribute to its output, one object per body: the container list reduced to `container_id`, `name`, `image`, `state`, `health`, `exit_code`, `restart_count`, `started_at_unix_ms`, `finished_at_unix_ms`.
  - `events12.sh <t0>`: the scripted actions, each at `t0` + whole minutes + 30 s, all containers named `p3-*` from `alpine:3`: at 3:30 `p3-run` and `p3-pause` (`sleep 3600`), `p3-loop` (`--restart on-failure`, `sh -c 'sleep 20; exit 1'`), `p3-ok` (`true`), `p3-fail` (`sh -c 'exit 3'`); at 4:30 `docker restart -t 1 p3-run`, `docker pause p3-pause`; at 5:30 `docker kill p3-run`, `docker unpause p3-pause`; at 6:30 `docker rm -f p3-loop p3-run p3-ok`. `p3-pause` and `p3-fail` stay to the end and are removed after the run. `docker events --filter type=container` is recorded for the whole run.
- [ ] **Step 2: Build both.** Branch: `CGO_ENABLED=0 env -u GOROOT GOTOOLCHAIN=auto go build -ldflags "-s -w -X github.com/monitorable-io/monitorable-agent/internal/version.Version=v1.10.0" -o "$W/bin/new" cmd/monitorable-agent/main.go`. Old: `git worktree add "$W/v190" v1.9.0`, the same build there with `Version=v1.9.0` to `$W/bin/old`. Cross-build the branch for arm64 with the release flags.
- [ ] **Step 3: Run both for 12 minutes,** started in the same second, each with its own sink port, its own `MONITORABLE_STATE_DIR`, `MONITORABLE_API_KEY=acceptance-not-a-key`, its own proxy socket, and two `--config` files: its tree's shipped config and a one-key override `receivers::monitorable::docker::endpoint: unix://<its proxy socket>`. Every 10 s record `Pss:` from `/proc/$PID/smaps_rollup` and fields 14+15 of `/proc/$PID/stat`. Waits are foreground until-loops of at most 10 minutes. Stop both with SIGTERM, by PID.
- [ ] **Step 4: Check 1.** Pair the two agents' bodies by cycle number (their arrival times differ by under 2 s; fail the run if not). Per cycle the reduced `docker.containers` lists are equal. A difference is allowed only for a container with a `docker events` line within 1 s of that cycle's list request (from the proxy log), and that container must be equal in the next cycle.
- [ ] **Step 5: Checks 2, 3 and 4** from the branch's proxy log, cycle by cycle (a cycle is the requests from one list request to the next): inspect requests are 0 in every cycle after the first until the script starts, and in every cycle from minute 10; after each scripted action the container it touched is inspected in the next cycle, in every cycle until its start or stop is at least 2 minutes old, and in none once it is 4 minutes old; no other container is inspected in those cycles; `p3-loop` is inspected in every cycle it exists. Per cycle, the branch's request count is never above v1.9.0's.
- [ ] **Step 6: Checks 5, 6 and 8.** CPU ticks over the run, branch ≤ old (expected about 16 ms a minute less; v1.9.0 measured 103 ms/min). PSS median branch ≤ old + 1 MB. `$W/bin/new` ≤ 33,000,000 B and the arm64 build ≤ 31,000,000 B. Both logs: no `error` line, and no `warn` line in the branch's that v1.9.0's does not have.
- [ ] **Step 7: Clean up.** `docker rm -f` every `p3-*` container; `git worktree remove "$W/v190"`; confirm `docker ps -a --filter name=p3-` is empty.
- [ ] **Step 8: PR.** Push `feat/lean-agent-part3`, open the PR: title `feat: inspect a container only when it has changed (v1.10.0)`; body: spec link, the table of spec 7.1 with the measured numbers, the five Review Focus points, the three known limits of spec 4.7, and the result of Task 2 Step 7. Request a fresh review of the whole branch on the most capable model; answer every finding on the branch.

### Task 4: Release gate, prod, handoff

- [ ] **Step 1 (founder):** merge; tag `v1.10.0` (`release.yml` publishes to staging; `publish-dist.sh` sets the installer's `MIN_VERSION` to the tag as it does for every release).
- [ ] **Step 2: Staging acceptance (spec 7.2)** on a staging server that runs containers (pick one from the dashboard; record which). Before the update note the Containers panel's rows: name, state, uptime, exit code. Update to v1.10.0 from `install-mon.ok9k.com`. After two cycles the rows are the same. `docker restart` one container (with the founder's word, since it is a staging workload): its Uptime restarts from zero within two cycles. After 30 minutes: `journalctl -u monitorable-agent --since "-30min" -p warning` shows nothing new; `NRestarts=0`. Put the results in the PR.
- [ ] **Step 3 (founder):** `publish-prod` dispatch for `v1.10.0`, then the fleet update (the founder's own words are required for it).
- [ ] **Step 4: GitHub release notes** for `v1.10.0`, two sentences: the agent now asks Docker for a container's details only when the container has started, stopped or restarted, where it asked for every container every minute; the Containers panel, alerts and API show the same data. No changelog entry on the site.
- [ ] **Step 5: Docs.** `grep -rn -i 'inspect' agent_docs/` in the platform checkout: on 2026-11-24 nothing there describes the per-cycle Docker calls, so the agent's `CLAUDE.md` (Task 2) is the only doc change. If that has changed, update it in a platform PR.
- [ ] **Step 6: Hand off:** `/handoff` with the acceptance and staging numbers. Open afterwards: candidate Part 4 (stats without Docker, spec section 10) and the benchmark re-measurement decision (Part 1, Q5), each its own session.
