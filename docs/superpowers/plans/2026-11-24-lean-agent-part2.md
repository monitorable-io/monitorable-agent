# Lean agent, Part 2 (agent v1.9.0) — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans (Native) to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Execution mode: Native** (one session implements every task, one fresh Opus review of the whole branch at the end). Reason: four small code tasks in one repo, about 400 lines with tests, that meet in the shipped config and `cmd/monitorable-agent/config_test.go`; the acceptance run needs all of them together.

**Goal:** Ship agent v1.9.0, which sends each collection cycle about a second after it is taken (today 59 to 60 s later) and counts TCP sockets by state from the ports sampler, so it no longer opens `/proc/<pid>/fd` of every process, with the same stored series.

**Architecture:** A new metrics processor `cycle_batch` (`internal/processor/cyclebatch`) gathers the deliveries of one collection cycle and sends them on as one request when the expected number has arrived or `max_wait` has passed. The stock `batch` processor stays behind it with its timer off, only to split a cycle above 5,000 datapoints. The ports sampler also counts every socket state, and the `monitorable` receiver emits `system.network.connections` in hostmetrics' shape; the shipped config switches that metric off in hostmetrics.

**Tech Stack:** Go 1.27; collector core v1.68.0 / v0.162.0 (`processor`, `processor/processortest`, `consumer/consumertest`, `otelcol/otelcoltest`, `receiver/receivertest`, `component/componenttest`, `pdata/pmetric`, `processor/batchprocessor`); contrib `hostmetricsreceiver` v0.162.0; `go.uber.org/zap` with `zaptest/observer`.

**Spec:** `docs/superpowers/specs/2026-11-24-lean-agent-part2-design.md` (read it first; every value below comes from it).

## Global Constraints

- Processor type `cycle_batch`, package `internal/processor/cyclebatch`, metrics only. Settings `deliveries` (int, ≥ 1, default 1) and `max_wait` (duration, > 0, default 10 s). Shipped values: `deliveries: 2`, `max_wait: 10s`.
- Shipped pipeline order: `processors: [cycle_batch, batch, resource]`. Shipped `batch`: `timeout: 0s`, `send_batch_size: 5000`, `send_batch_max_size: 5000`.
- Metric shape, identical to hostmetrics v0.162.0: name `system.network.connections`, unit `{connections}`, description `The number of connections.`, Sum, not monotonic, cumulative, integer; attributes `protocol="tcp"` and `state`.
- The twelve state labels, always all sent, zero included: `CLOSE_WAIT`, `CLOSE`, `CLOSING`, `DELETE`, `ESTABLISHED`, `FIN_WAIT_1`, `FIN_WAIT_2`, `LAST_ACK`, `LISTEN`, `SYN_SENT`, `SYN_RECV`, `TIME_WAIT`. Kernel codes: `01` ESTABLISHED, `02` SYN_SENT, `03` SYN_RECV, `04` FIN_WAIT_1, `05` FIN_WAIT_2, `06` TIME_WAIT, `07` CLOSE, `08` CLOSE_WAIT, `09` LAST_ACK, `0A` LISTEN, `0B` CLOSING. `DELETE` is always 0. Any other code is ignored.
- Receiver key `monitorable::tcp_states::mode`: `auto` | `on` | `off`. Default in the binary: `off`. Shipped config: `auto`.
- `PortsSnapshot` (the JSON of `listening.ports`) does not change.
- Nothing in `monitorable-backend` or `monitorable-app` changes. No new module in `go.mod` beyond promoting `processor/processortest` from indirect if `go mod tidy` does so.
- Release binaries stay within the CI budgets: ≤ 33,000,000 B (amd64), ≤ 31,000,000 B (arm64).
- Commits follow Conventional Commits; no Claude attribution lines. Branch `feat/lean-agent-part2`, created from `docs/lean-agent-part2-design` (which holds the spec and this plan), PR to `main`. The founder merges, tags and dispatches prod; this session never merges and never runs `/ship` on its own.

## Review Focus

Conditions the spec implies that a first pass of tests would miss. Each has its test pinned to the owning task.

1. **A stop inside the gather window.** The installer restarts the service on every update, and a restart can land between the two deliveries. The pending group must be handed downstream, not dropped (Task 1, `TestShutdownHandsOverPendingGroup`; Task 5, check 10).
2. **A collector that is slow or silent for a cycle** (a hung Docker daemon, a hostmetrics scrape that returns nothing). The other collector's data must still leave after `max_wait`, and the next cycle must group correctly again (Task 1, `TestLateDeliveryStartsItsOwnGroup`, `TestSilentPartnerDoesNotHoldData`).
3. **A v1.8.0 config on the v1.9.0 binary** (hosts that manage the config themselves and swap only the binary). It must start and send each state series once, from hostmetrics (Task 4, `TestV180ConfigLoadsAndKeepsTCPStatesOff`; Task 5, check 13).
4. **A cycle above 5,000 datapoints** (hundreds of cores or mounts). It must leave as several requests, never as one the backend refuses (Task 4, `TestShippedProcessorsSplitOversizedCycle`).
5. **`ports.mode: off`.** The state counts must continue, with one table read per cycle and no sampler goroutine (Task 3, `TestTCPStatesWithPortsOff`).

---

### Task 1: The `cycle_batch` processor

**Files:**
- Create: `internal/processor/cyclebatch/config.go`, `factory.go`, `processor.go`, `processor_test.go`
- Modify: `cmd/monitorable-agent/main.go` (`newFactories`: register the processor), `cmd/monitorable-agent/main_test.go` (`TestFactoriesRegisterShippedComponents`: add `cycle_batch` to the processor list)

**Interfaces:**
- Produces:

```go
package cyclebatch

type Config struct {
	Deliveries int           `mapstructure:"deliveries"`
	MaxWait    time.Duration `mapstructure:"max_wait"`
}

func (c *Config) Validate() error // deliveries >= 1; max_wait > 0
func NewFactory() processor.Factory // type "cycle_batch", processor.WithMetrics(..., component.StabilityLevelDevelopment)
```

- [ ] **Step 1: Write the failing tests** in `processor_test.go`. Helper: `newTestProcessor(t, deliveries int, maxWait time.Duration, next consumer.Metrics) processor.Metrics` builds through `NewFactory().CreateMetrics` with `processortest.NewNopSettings(NewFactory().Type())`, starts it, and registers `Shutdown` in `t.Cleanup`. Helper `oneResource(name string) pmetric.Metrics` returns a `Metrics` with one `ResourceMetrics` holding one gauge named `name` with one datapoint. "Sink" is `consumertest.MetricsSink`.

| Test | Arrange and act | Assert |
|---|---|---|
| `TestSendsWhenCountIsReached` | `deliveries: 2`, `max_wait: time.Hour`; two `ConsumeMetrics` calls | both return nil; within 1 s the sink holds exactly 1 `Metrics` with 2 `ResourceMetrics`, in delivery order |
| `TestHoldsBelowCount` | `deliveries: 2`, `max_wait: time.Hour`; one call | sink is empty 100 ms later |
| `TestSendsAfterMaxWait` | `deliveries: 2`, `max_wait: 50ms`; one call | sink empty right after the call; within 1 s it holds 1 `Metrics` with 1 `ResourceMetrics` |
| `TestLateDeliveryStartsItsOwnGroup` | `deliveries: 2`, `max_wait: 50ms`; one call, wait for the timer send, a second call, wait | sink holds 2 `Metrics`, 1 `ResourceMetrics` each; then two more calls → a third `Metrics` with 2 (the next cycle groups normally) |
| `TestSilentPartnerDoesNotHoldData` | `deliveries: 2`, `max_wait: 50ms`; five single calls 120 ms apart | 5 `Metrics` in the sink, none lost, none merged |
| `TestGroupsDoNotMix` | `deliveries: 2`, `max_wait: time.Hour`; four calls | 2 `Metrics`, 2 `ResourceMetrics` each |
| `TestDefaultPassesEachDeliveryThrough` | `createDefaultConfig()` unchanged; two calls | default is `Deliveries == 1`, `MaxWait == 10*time.Second`; sink holds 2 `Metrics` |
| `TestShutdownHandsOverPendingGroup` | `deliveries: 2`, `max_wait: time.Hour`; one call, then `Shutdown` | when `Shutdown` returns the sink holds the delivery; 100 ms later still exactly 1 `Metrics` |
| `TestNothingSentWhenIdle` | `deliveries: 2`, `max_wait: 20ms`; no calls for 100 ms | sink empty |
| `TestDownstreamErrorIsLoggedNotReturned` | next is `consumertest.NewErr(errors.New("boom"))`, `deliveries: 1`, logger from `zaptest/observer` | `ConsumeMetrics` returns nil; exactly 1 log entry at Warn |
| `TestConcurrentDeliveriesLoseNothing` | `deliveries: 2`, `max_wait: 5ms`; 8 goroutines × 100 calls, then `Shutdown` | the sink's `ResourceMetrics` total is 800; passes under `-race` |
| `TestConfigValidate` | `Deliveries: 0`; `MaxWait: 0`; `Deliveries: -1` | each returns an error; `{2, 10s}` returns nil |
| `TestCapabilitiesMutatesData` | — | `Capabilities().MutatesData == true` |

- [ ] **Step 2: Run** `go test ./internal/processor/cyclebatch/` — expected: FAIL (package does not compile).

- [ ] **Step 3: Implement** `config.go`, `factory.go`, `processor.go`. The processor moves each delivery's `ResourceMetrics` into a pending `pmetric.Metrics` (`MoveAndAppendTo`), counts deliveries, arms one `max_wait` timer on the first delivery of a group, and sends the group to `next` when the count is reached or the timer fires. After a send it holds nothing and no timer runs. `ConsumeMetrics` always returns nil. A send error is logged once at Warn with the group's datapoint count. Sends use `context.Background()` (the delivering receiver's context may be cancelled by then). A timer that fires for a group already sent on the count must not send the next group early.

- [ ] **Step 4: Register** in `newFactories()`: `component.MustNewType("cycle_batch"): cyclebatch.NewFactory()`, with a comment naming the spec. Add `"cycle_batch"` to the processor list in `TestFactoriesRegisterShippedComponents`.

- [ ] **Step 5: Run** `go test -race ./internal/processor/cyclebatch/ ./cmd/monitorable-agent/ -run 'Test.*'` — expected: PASS. Run `go vet ./...`.

- [ ] **Step 6: Commit** `feat(processor): cycle_batch sends a collection cycle when it is complete`.

---

### Task 2: The ports sampler counts every socket state

**Files:**
- Modify: `internal/collectors/ports_parse.go` (`summarizeSockets`, the state constants' comment), `internal/collectors/ports.go` (`PortsResult`, `portsWindow`, `Sample`, `Snapshot`)
- Test: `internal/collectors/ports_parse_test.go`, `internal/collectors/ports_test.go`

**Interfaces:**
- Produces:

```go
// TCPStateCounts is the number of socket-table rows per kernel state code
// (index = code, 0x01..0x0B; index 0 unused).
type TCPStateCounts [12]int

// TCPStateLabels are the twelve state labels of hostmetrics v0.162.0, in its order.
var TCPStateLabels = [12]string{"CLOSE_WAIT", "CLOSE", "CLOSING", "DELETE", "ESTABLISHED",
	"FIN_WAIT_1", "FIN_WAIT_2", "LAST_ACK", "LISTEN", "SYN_SENT", "SYN_RECV", "TIME_WAIT"}

// ByLabel returns one entry per label in TCPStateLabels, zero included. DELETE is always 0.
func (c TCPStateCounts) ByLabel() map[string]int64

// TCPStates is the per-state count of one sample.
type TCPStates struct {
	At     time.Time
	Counts TCPStateCounts
}

type PortsResult struct {
	Status   PortsStatus
	Err      error
	Snapshot *PortsSnapshot
	States   *TCPStates // newest sample of the window; nil unless Status == PortsOK
}

func summarizeSockets(v4, v6 []tcpRow) (listeners map[uint16]*listenerFacts, established map[uint16]int, states TCPStateCounts)
```

`Sample` and `Snapshot` keep their signatures.

- [ ] **Step 1: Write the failing tests.**

In `ports_parse_test.go`:

```go
func TestSummarizeSockets_CountsEveryState(t *testing.T) {
	v4 := []tcpRow{
		row("10.0.0.1", 22, 0x0A), row("10.0.0.1", 22, 0x01), row("10.0.0.1", 22, 0x01),
		row("10.0.0.1", 443, 0x06), row("10.0.0.1", 443, 0x06), row("10.0.0.1", 443, 0x08),
		row("10.0.0.1", 5000, 0x04), row("10.0.0.1", 5001, 0x0C), // 0x0C: not a state we know
	}
	v6 := []tcpRow{row("::", 22, 0x0A), row("::1", 9000, 0x01)}
	_, _, states := summarizeSockets(v4, v6)
	got := states.ByLabel()
	want := map[string]int64{"ESTABLISHED": 3, "LISTEN": 2, "TIME_WAIT": 2, "CLOSE_WAIT": 1, "FIN_WAIT_1": 1}
	if len(got) != 12 {
		t.Fatalf("ByLabel has %d labels, want 12", len(got))
	}
	for _, label := range TCPStateLabels {
		if got[label] != want[label] {
			t.Errorf("%s = %d, want %d", label, got[label], want[label])
		}
	}
}
```

- `TestSummarizeSockets_SharedPortListenersCountEach`: four LISTEN rows with the same address and port → `ByLabel()["LISTEN"] == 4` and `len(listeners) == 1`.
- Update the three existing `summarizeSockets` call sites for the third return value.

In `ports_test.go` (existing helpers `newTestPorts`, `writeProcNet`, `sampleAt`, `listenRow`, `estRows`):

- `TestPortsCollector_StatesAreFromTheNewestSample`: sample 1 with 1 LISTEN + 2 ESTABLISHED, sample 2 with 1 LISTEN + 5 ESTABLISHED → `res.States.Counts.ByLabel()["ESTABLISHED"] == 5`, `["LISTEN"] == 1`, `res.States.At` equals the second sample's time.
- `TestPortsCollector_NoStatesWithoutASample`: `Snapshot` on a new collector → `Status == PortsNoSamples`, `States == nil`; with `net/tcp` missing after one failed `Sample` → `Status == PortsUnreadable`, `States == nil`.
- `TestPortsCollector_StatesWithoutTCP6`: only `net/tcp` present → `States != nil`, counts from the v4 table, `len(ByLabel()) == 12`.
- `TestPortsCollector_StatesWithNoListener`: a table with ESTABLISHED rows and no LISTEN row → `Status == PortsOK`, `ByLabel()["ESTABLISHED"]` is the row count (a host with no listener still has connections).
- `TestPortsSnapshot_JSONKeysUnchanged`: marshal a `PortsSnapshot` from a sample → the top-level JSON keys are exactly `collected_at_unix_ms`, `all_ports`, `all_ports_truncated`, `ports`.

- [ ] **Step 2: Run** `go test ./internal/collectors/ -run 'Summarize|PortsCollector|PortsSnapshot'` — expected: FAIL (does not compile).

- [ ] **Step 3: Implement.** `summarizeSockets` counts every row whose state code is 0x01..0x0B. `portsWindow` keeps the counts of its newest sample next to `lastAt`; `Snapshot` returns them in `PortsResult.States` when the window has a sample. Update the comment on the state constants in `ports_parse.go`, which says the other states are deliberately ignored.

- [ ] **Step 4: Run** `go test -race ./internal/collectors/` — expected: PASS.

- [ ] **Step 5: Commit** `feat(ports): count every TCP state in the sampler`.

---

### Task 3: The receiver emits `system.network.connections`

**Files:**
- Create: `internal/receiver/monitorable/tcp_states_emit.go`, `tcp_states_emit_test.go`, `config_tcp_states_test.go`
- Modify: `internal/receiver/monitorable/config.go` (`Config`, `Validate`, constants), `factory.go` (`createDefaultConfig`), `receiver.go` (construction and `Start`: when the collector exists and when the sampler runs), `ports_emit.go` (`collectListeningPorts`)

**Interfaces:**
- Consumes: Task 2's `collectors.TCPStates`, `TCPStateLabels`, `ByLabel`, `PortsResult.States`.
- Produces:

```go
const (
	TCPStatesModeAuto = "auto" // send; quiet when /proc/net/tcp is unreadable
	TCPStatesModeOn   = "on"   // send; WARN when /proc/net/tcp is unreadable
	TCPStatesModeOff  = "off"  // never send
)

type TCPStatesConfig struct {
	Mode string `mapstructure:"mode"`
}

// in Config:
TCPStates TCPStatesConfig `mapstructure:"tcp_states"`

// tcp_states_emit.go
func buildTCPStateMetric(ms pmetric.MetricSlice, st *collectors.TCPStates)

// receiver.go: Start launches runPortsSampler only when this is true (ports.mode != off).
func (r *metricsReceiver) runsPortsSampler() bool
```

`Validate`: `""` becomes `off`; any other value than the three is `monitorable: invalid tcp_states.mode %q (want auto|on|off)`. `createDefaultConfig` sets `off`.

- [ ] **Step 1: Write the failing tests.**

`config_tcp_states_test.go`: `TestTCPStatesModeDefaultsToOff` (`createDefaultConfig()` and an empty `Config` after `Validate` both have `TCPStates.Mode == "off"`); `TestTCPStatesModeAccepted` (`auto`, `on`, `off` validate); `TestTCPStatesModeInvalid` (`"yes"` → the error text above).

`tcp_states_emit_test.go`, on the helpers of `ports_emit_test.go` (`writeProcTCP`, `newPortsTestReceiver`, `mustSample`, `portsAttrs`); extend `newPortsTestReceiver` or add a sibling so a test can set `ports.mode` and `tcp_states.mode`:

```go
func TestBuildTCPStateMetricShape(t *testing.T) {
	at := time.Unix(1795527600, 0)
	var counts collectors.TCPStateCounts
	counts[0x01], counts[0x0A], counts[0x06] = 19, 64, 7
	ms := pmetric.NewMetricSlice()
	buildTCPStateMetric(ms, &collectors.TCPStates{At: at, Counts: counts})

	if ms.Len() != 1 {
		t.Fatalf("metrics = %d, want 1", ms.Len())
	}
	m := ms.At(0)
	if m.Name() != "system.network.connections" || m.Unit() != "{connections}" || m.Description() != "The number of connections." {
		t.Errorf("name/unit/description = %q/%q/%q", m.Name(), m.Unit(), m.Description())
	}
	if m.Type() != pmetric.MetricTypeSum || m.Sum().IsMonotonic() || m.Sum().AggregationTemporality() != pmetric.AggregationTemporalityCumulative {
		t.Errorf("want a non-monotonic cumulative Sum, got %v", m.Type())
	}
	dps := m.Sum().DataPoints()
	if dps.Len() != 12 {
		t.Fatalf("datapoints = %d, want 12", dps.Len())
	}
	got := map[string]int64{}
	for i := 0; i < dps.Len(); i++ {
		dp := dps.At(i)
		if dp.Attributes().Len() != 2 {
			t.Errorf("datapoint %d has %d attributes, want protocol and state only", i, dp.Attributes().Len())
		}
		if p, _ := dp.Attributes().Get("protocol"); p.Str() != "tcp" {
			t.Errorf("protocol = %q", p.Str())
		}
		if dp.Timestamp().AsTime().Unix() != at.Unix() || dp.ValueType() != pmetric.NumberDataPointValueTypeInt {
			t.Errorf("datapoint %d: timestamp %v, value type %v", i, dp.Timestamp().AsTime(), dp.ValueType())
		}
		s, _ := dp.Attributes().Get("state")
		got[s.Str()] = dp.IntValue()
	}
	if got["ESTABLISHED"] != 19 || got["LISTEN"] != 64 || got["TIME_WAIT"] != 7 || got["DELETE"] != 0 || len(got) != 12 {
		t.Errorf("states = %v", got)
	}
}
```

| Test | Arrange | Assert on what `collectListeningPorts` produced |
|---|---|---|
| `TestTCPStatesRideTheCycle` | ports `auto`, states `auto`; a sample exists | `listening.ports` attribute, both per-port gauges, and `system.network.connections` with 12 datapoints stamped with the sample time |
| `TestTCPStatesOff` | ports `auto`, states `off` | attribute and gauges present; no `system.network.connections` |
| `TestTCPStatesWithPortsOff` | ports `off`, states `auto`; no sample taken by the test | `system.network.connections` with 12 datapoints; no `listening.ports`; no per-port gauges; `r.runsPortsSampler() == false`. Rewrite the table with a different ESTABLISHED count and run a second cycle with no explicit sample: the datapoint follows the new table (the cycle takes its own sample every time) |
| `TestBothOffBuildsNoCollector` | ports `off`, states `off` | `portsCollector == nil`; nothing emitted |
| `TestFirstCycleTakesItsOwnSample` | ports `auto`, states `auto`; no sample exists | the attribute, the gauges and the 12 datapoints are all present |
| `TestTCPStatesUnreadable` | no `net/tcp` under the proc root; states `on`, then `auto` | no metric; with `on` exactly one Warn over three cycles; with `auto` none |

- [ ] **Step 2: Run** `go test ./internal/receiver/monitorable/ -run 'TCPState|BothOff|FirstCycle'` — expected: FAIL (does not compile).

- [ ] **Step 3: Implement.**
  - `receiver.go`: build the `PortsCollector` when `ports.mode != off` **or** `tcp_states.mode != off`; start `runPortsSampler` only when `ports.mode != off`.
  - `ports_emit.go`, `collectListeningPorts`: with ports on, take the window's snapshot; if its status is `PortsNoSamples`, take one sample in the cycle (bounded by `portsSampleTimeout`) and snapshot again. With ports off and states on, take one sample in the cycle and snapshot. Emit the attribute and gauges only when ports is on; call `buildTCPStateMetric` only when states is not off and `res.States != nil`.
  - Logging: `logPortsTransition` stays tied to `ports.mode`. Add `logTCPStatesTransition` in the same style: one line per change; unreadable is Warn under `on`, Debug under `auto`.
  - `buildTCPStateMetric`: one datapoint per entry of `collectors.TCPStateLabels`, in that order.

- [ ] **Step 4: Run** `go test -race ./internal/receiver/monitorable/` — expected: PASS (the existing ports tests included).

- [ ] **Step 5: Commit** `feat(receiver): TCP state counts from the ports sampler`.

---

### Task 4: Shipped config, pipeline tests, repo docs

**Files:**
- Modify: `distribution/configs/linux/collector-config.yaml`, `cmd/monitorable-agent/config_test.go` (`TestShippedConfigsLoad`), `cmd/monitorable-agent/hostmetrics_shipped_test.go`, `CLAUDE.md`, `distribution/README.md`
- Create: `cmd/monitorable-agent/pipeline_test.go`, `cmd/monitorable-agent/tcp_states_parity_test.go`, `cmd/monitorable-agent/testdata/collector-config.v1.8.0.yaml` (the output of `git show v1.8.0:distribution/configs/linux/collector-config.yaml`, byte for byte)

**Interfaces:**
- Consumes: `cyclebatch.Config` (Task 1), `monitorable.Config.TCPStates` and `monitorable.TCPStatesMode*` (Task 3).

- [ ] **Step 1: Write the failing tests.**

In `TestShippedConfigsLoad`, add:

```go
	pipe := cfg.Service.Pipelines[pipeline.NewID(pipeline.SignalMetrics)]
	wantProcs := []component.ID{component.MustNewID("cycle_batch"), component.MustNewID("batch"), component.MustNewID("resource")}
	if !slices.Equal(pipe.Processors, wantProcs) {
		t.Errorf("metrics pipeline processors = %v, want %v", pipe.Processors, wantProcs)
	}
	cb, ok := cfg.Processors[component.MustNewID("cycle_batch")].(*cyclebatch.Config)
	if !ok {
		t.Fatal("cycle_batch config missing or of the wrong type")
	}
	// One delivery per receiver per cycle: a receiver added without raising the
	// count would make every cycle leave before it is complete.
	if cb.Deliveries != len(pipe.Receivers) || cb.MaxWait != 10*time.Second {
		t.Errorf("cycle_batch = %+v, want deliveries %d (the pipeline's receivers) and max_wait 10s", cb, len(pipe.Receivers))
	}
	bc, ok := cfg.Processors[component.MustNewID("batch")].(*batchprocessor.Config)
	if !ok {
		t.Fatal("batch config missing or of the wrong type")
	}
	// Size limit only: no timer, and a cycle above 5,000 datapoints is split
	// (the backend refuses a request above 20,000).
	if bc.Timeout != 0 || bc.SendBatchSize != 5000 || bc.SendBatchMaxSize != 5000 {
		t.Errorf("batch = timeout %v size %d max %d, want 0 / 5000 / 5000", bc.Timeout, bc.SendBatchSize, bc.SendBatchMaxSize)
	}
	if rc.TCPStates.Mode != monitorable.TCPStatesModeAuto {
		t.Errorf("tcp_states.mode = %q, want auto", rc.TCPStates.Mode)
	}
```

In `hostmetrics_shipped_test.go`, inside `TestShippedHostMetricsEmitOnlyStoredSeries` where it walks the emitted names: fail when the name is `system.network.connections` ("the monitorable receiver sends it since v1.9.0; hostmetrics would scan /proc/<pid>/fd for it"). The name stays in `stored`.

`pipeline_test.go`:

- `TestShippedProcessorsSplitOversizedCycle`: load the shipped config, build `batch` then `cycle_batch` from `newFactories().Processors` with the shipped configs (`processortest.NewNopSettings`), chained `cycle_batch → batch → consumertest.MetricsSink`, start both. Deliver one `Metrics` of 12,000 gauge datapoints and one of 10. After shutting both down in pipeline order: every `Metrics` in the sink has `DataPointCount() <= 5000`, and the total is 12,010.
- `TestShippedProcessorsSendACycleAtOnce`: same chain; two deliveries of 100 datapoints → within 1 s the sink holds exactly one `Metrics` with 200 datapoints.
- `TestV180ConfigLoadsAndKeepsTCPStatesOff`: `otelcoltest.LoadConfigAndValidate("testdata/collector-config.v1.8.0.yaml", newFactories())` succeeds; the receiver config has `TCPStates.Mode == "off"`; the pipeline's processors are `[batch, resource]`.

`tcp_states_parity_test.go`, `TestTCPStateMetricMatchesHostmetrics` (Linux, real `/proc`): run the `host_metrics` factory with a config that enables only the `network` scraper (`collection_interval: 1s`) and capture one scrape with a sink, as `TestShippedHostMetricsEmitOnlyStoredSeries` does. Run the `monitorable` factory with `ports.mode: off`, `tcp_states.mode: on`, `docker`, `systemd` and `smart` modes `off`, no `address_probe`, and capture its first delivery. Find `system.network.connections` in both. Assert equal: metric type, monotonicity, temporality, unit, description, and the set of attribute keys on every datapoint. Assert our state set equals the twelve labels, and that each of the twelve appears in hostmetrics' output (hostmetrics may add `FIN_WAIT1` or `FIN_WAIT2`; that is the known difference of spec 5.4). Do not compare values: the two reads happen at different moments.

- [ ] **Step 2: Run** `go test ./cmd/monitorable-agent/` — expected: FAIL on the new assertions.

- [ ] **Step 3: Edit the shipped config** to the YAML of spec sections 4.2 and 5.3:
  - `processors`: add `cycle_batch` (`deliveries: 2`, `max_wait: 10s`) with a comment saying what it does, that `deliveries` is the number of receivers in the pipeline, and that `TestShippedConfigsLoad` pins it; change `batch` to `timeout: 0s`, `send_batch_size: 5000`, `send_batch_max_size: 5000` with the "size limit only" comment.
  - `service::pipelines::metrics::processors: [cycle_batch, batch, resource]`.
  - `host_metrics::scrapers::network`: add `metrics: {system.network.connections: {enabled: false}}`; change the trailing comment to `# system.network.{io,packets,dropped,errors}`.
  - `monitorable`: add `tcp_states: {mode: auto}` with a comment: the counts come from the ports sampler's reads of `/proc/net/tcp{,6}`, and `ports.mode: off` does not stop them.
  - Correct the comments that are now wrong: the header's "Collection cadence is controlled by the batch processor", the receiver's "Collection frequency is controlled by the batch processor", and the queue comment's "A busy host (send_batch_size: 3000) can flush more than one batch a minute" (now: a cycle above 5,000 datapoints is sent as more than one request).

- [ ] **Step 4: Run** `go test -race ./...` and `go vet ./...` — expected: PASS.

- [ ] **Step 5: Smoke run.** Build, then run for 70 s against the Part 1 sink (`python3 -I sink.py <port> <log.jsonl> <dump-dir>` from the platform checkout, `docs/superpowers/notes/2026-11-24-agent-resource-analysis/`) with `MONITORABLE_STATE_DIR` in a scratch directory. Expected: a `/v1/metrics` line within 5 s of start and a second one about 60 s after it; no `error` line in the agent's log.

- [ ] **Step 6: Repo docs.**
  - `CLAUDE.md`: add `processor/cyclebatch/` to the file structure; add two Key Concepts entries, "Send when collected" (what `cycle_batch` does, `deliveries` must equal the number of receivers, the stock `batch` is a size limit only, spec path) and "TCP state counts" (from the ports sampler, `tcp_states.mode`, default `off` in the binary and `auto` in the shipped config and why, the shared-port and `FIN_WAIT` differences); in "Listening ports", replace "connection counts = ESTABLISHED `01` only" context so it does not read as if other states were ignored by the sampler.
  - `distribution/README.md`, the queue section near "The queue holds 1440 batches": replace the `send_batch_size: 3000` sentence with the 5,000-datapoint rule, and add one sentence that a request is sent about a second after its cycle is collected.

- [ ] **Step 7: Commit** `feat: send each cycle when collected and take TCP state counts from the sampler (v1.9.0)`.

---

### Task 5: Acceptance run, PR, review

Spec 7.1. Needs a Linux host with Docker running and `strace` (the dev server qualifies). `$W` is a scratch directory outside the repo. Tools: `sink.py` and `experiment.patch` from the platform checkout, `docs/superpowers/notes/2026-11-24-agent-resource-analysis/`. Never use `pkill -f`; stop every process by PID.

- [ ] **Step 1: Tools.** Copy `sink.py` to `$W` and make it keep every `/v1/metrics` body (it keeps the first six) and, when the environment variable `SINK_503_UNTIL` holds a Unix time, answer 503 until then. In a scratch copy of the repo without `.git`, `git apply --include='cmd/zz-pbdump/*' <experiment.patch>` and add a `series` mode to it that prints one line per datapoint, `name{sorted labels}` and the datapoint time. Build it to `$W/pbdump`. None of this is committed to the agent repo; put the final scripts in the platform notes directory next to the Part 1 ones.
- [ ] **Step 2: Build both.** Branch: `CGO_ENABLED=0 go build -ldflags "-s -w -X github.com/monitorable-io/monitorable-agent/internal/version.Version=v1.9.0" -o "$W/new" cmd/monitorable-agent/main.go`. Old: `git worktree add "$W/v180" v1.8.0`, then the same build there with `Version=v1.8.0` to `$W/old`. Also cross-build the branch for arm64 with the release flags.
- [ ] **Step 3: Run both for 10 minutes**, each with its own sink port, its own `MONITORABLE_STATE_DIR`, `MONITORABLE_API_KEY=acceptance-not-a-key` and its own tree's shipped config. Record each process's start time. Every 10 s record `Pss:` from `/proc/$PID/smaps_rollup` and fields 14+15 of `/proc/$PID/stat`. Stop both with SIGTERM.
- [ ] **Step 4: Check 1 to 5, 7 and 8** of spec 7.1 from the two sink logs and dumps: first request ≤ 5 s after start (old: about 60 s); for every later request, arrival minus the newest collection-timed datapoint ≤ 5 s, and ≤ 15 s for `monitorable.port.connections*` and `system.network.connections`; one request per cycle; the sorted unique `name{labels}` lines of both runs are equal once `state="FIN_WAIT1"` and `state="FIN_WAIT2"` are removed from the old one; mean `wire` after the first request, new ≤ 1.02 × old; CPU ticks new ≤ old; PSS median new ≤ old + 1 MB; `$W/new` ≤ 33,000,000 B and the arm64 build ≤ 31,000,000 B.
- [ ] **Step 5: Check 6.** `strace -f -e trace=openat -o "$W/strace-new.txt" "$W/new" --config …` for 3 minutes, then the same for `$W/old` for 70 s. `grep -cE '"/proc/[0-9]+/fd"'`: new is 0; old is about the number of processes per scrape (321 with 322 processes on 2026-11-24).
- [ ] **Step 6: Check 9.** Ten paired reads one second apart, in one process: rows of `/proc/net/tcp` and `/proc/net/tcp6` per state against `net.ConnectionsWithContext(ctx, "tcp")` per status (a scratch program; `gopsutil` is already in the module graph). LISTEN equal in all ten; every other state equal in at least eight.
- [ ] **Step 7: Check 10.** Start `$W/new`; 0.6 s after the first line appears in the agent's log that the receiver started, send SIGTERM. The process exits within 45 s. Either the sink has a `/v1/metrics` request, or a second start with the same `MONITORABLE_STATE_DIR` delivers it. Repeat five times; no run may lose the first cycle.
- [ ] **Step 8: Check 11.** Run `$W/new` with `SINK_503_UNTIL` five minutes ahead, for eight minutes. After the sink accepts: the queued requests arrive, each holds one cycle (its datapoint times span less than 15 s), and every minute of the run has its datapoints.
- [ ] **Step 9: Checks 13 and 14.** `$W/new` with `$W/v180`'s config for 150 s: starts, no `error` line, first request about 60 s after start, each `system.network.connections` state once per request. `$W/new` with the shipped config plus `monitorable::ports::mode: off` for 70 s: no `listening.ports` attribute, no `monitorable.port.connections*`, twelve state datapoints per request.
- [ ] **Step 10:** Both 10-minute logs: no line at `error` level; the only `warn` allowed is the SMART permission line (the run is not root).
- [ ] **Step 11:** Remove the worktree (`git worktree remove "$W/v180"`). Push `feat/lean-agent-part2`, open the PR: title `feat: send when collected and TCP state counts from the sampler (v1.9.0)`; body: spec link, the table of spec 7.1 with the measured numbers, the five Review Focus points, and the three differences of spec 5.4. Request the Opus review of the whole branch; answer every finding on the branch.

### Task 6: Release gate, prod, public statements, handoff

- [ ] **Step 1 (founder):** merge; tag `v1.9.0` (`release.yml` publishes to staging; `publish-dist.sh` sets the installer's `MIN_VERSION` to the tag, which is what keeps an older binary away from this config).
- [ ] **Step 2: Staging acceptance (spec 7.2).** On staging 19, record the series of the last 20 minutes for the server (`/api/v1/series`, names and label sets), then update to v1.9.0 from `install-mon.ok9k.com`. After 30 minutes: the series of the last 20 minutes are the same set apart from `state="FIN_WAIT1"` and `"FIN_WAIT2"`; every panel of the server page that had data has data with no gap (CPU, memory, swap, load, disk I/O, disk usage, network throughput/packets/errors, TCP connections, temperature, ports, containers, services, SMART); `journalctl -u monitorable-agent --since "-30min" -p warning` shows nothing new; `NRestarts=0`. Then `systemctl restart monitorable-agent`, note the time from a GitHub `Date:` header, and read the raw samples of `system.uptime` for the server from VictoriaMetrics (`/api/v1/export`): one stamped after the restart is stored within 15 s. Soaks are timed with foreground `sleep` ≤ 10 min, not with background-task notices. Put the results in the PR.
- [ ] **Step 3 (founder):** `publish-prod` dispatch for `v1.9.0`. Then the fleet update (dev 1/7, staging 19/20/24, prod 2, demo web and db).
- [ ] **Step 4: Changelog** (landing repo, merge = prod), `src/content/changelog/<prod date>-agent-sends-when-collected.md`, label `Improved`. Two user-visible facts, in plain words and without the mechanism: metrics appear a few seconds after the agent starts and are about a minute fresher from then on, so alerts can fire sooner; on servers where several processes listen on one port, the Connections panel now counts each listener, so its Listen line may step up once at the update. Needs agent v1.9.0 or later; link `/docs/agent/updating-uninstalling/`. `npm test` passes.
- [ ] **Step 5: Docs wording** (ruled by the founder on 2026-11-24, spec section 8): "The first batch arrives a few seconds after the agent service starts. CPU and network charts need two samples, so they fill about a minute later." in the eight places of `src/content/docs/docs/faq.md`, `getting-started.md`, `agent/install.md` and `troubleshooting/no-data.md`; the no-data page's "Give a fresh install 60 seconds" heading and advice change with it. Same PR as Step 4. The dashboard string "within a minute" stays. Platform repo, separate PR: `agent_docs/procedures/collector-distribution.md` (the pipeline and where the TCP state counts come from).
- [ ] **Step 6: Hand off:** `/handoff` with the acceptance and staging numbers. Open afterwards: the benchmark re-measurement decision (Part 1, Q5) and Part 3 (container inspection), each its own session.
