# Lean agent, Part 1 (agent v1.8.0) — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans (Native) to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Execution mode: Native** (one session implements every task, one fresh Opus review of the whole branch at the end). Reason: three small changes in one repo, about 300 lines with tests, that share `cmd/monitorable-agent/config_test.go`, `go.mod` and one CI file; splitting them across agents buys nothing and the acceptance run needs all three together.

**Goal:** Ship agent v1.8.0 with a binary of about 29 MB instead of 57 MB, about 9 MB less resident memory, and a config that collects only the series the backend stores, with no change in what the backend receives and keeps.

**Architecture:** A new package `internal/selftelemetry` replaces the collector's stock telemetry factory with one that builds the same console logger and leaves meters and tracers as no-ops, which removes the AWS, Kubernetes, GCP and OAuth dependency trees from the binary. The release script strips debug info. The shipped hostmetrics config excludes the devices and switches off the metrics that the backend drops at ingest.

**Tech Stack:** Go 1.27; collector core v1.68.0 / v0.162.0 (`service/telemetry`, `otelcol/otelcoltest`, `consumer/consumertest`, `receiver/receivertest`, `component/componenttest`, `config/configtelemetry`, `confmap`); contrib `hostmetricsreceiver` v0.162.0; `go.uber.org/zap` v1.28.0 with `zaptest/observer`; `go.opentelemetry.io/otel/metric/noop` and `trace/noop`.

**Spec:** `docs/superpowers/specs/2026-11-24-lean-agent-build-design.md` (read it first; every number below comes from it).

## Global Constraints

- Package `internal/selftelemetry`. It must not be a second file in `cmd/monitorable-agent`: `scripts/publish-dist.sh` compiles `cmd/monitorable-agent/main.go` alone.
- `service::telemetry` keys read: `logs::level` (default `info`), `logs::encoding` (`console` default, or `json`), `metrics::level` (default `none`). Every other key loads without error and is ignored.
- Logger: zap production encoder config with `EncodeTime = zapcore.ISO8601TimeEncoder`; `OutputPaths` and `ErrorOutputPaths` `["stderr"]`; sampler tick 10 s, first 10, thereafter 100; no `resource` field.
- Size budgets on the release artifacts: `monitorable-agent-linux-amd64` ≤ 33,000,000 bytes, `monitorable-agent-linux-arm64` ≤ 31,000,000 bytes.
- Release link flags: `-s -w -X github.com/monitorable-io/monitorable-agent/internal/version.Version=${VERSION}`.
- The shipped `service::telemetry::metrics::level: none` line stays (the v1.7.0 binary needs it; the config object on R2 is unversioned).
- Backend device deny pattern, copied verbatim: `^(lo|docker0|veth.+|br-.+|loop\d+|/dev/loop\d+)$`.
- Nothing in `monitorable-backend` or `monitorable-app` changes.
- Commits follow Conventional Commits; no Claude attribution lines. Branch `feat/lean-agent`, PR to `main`. The founder merges, tags and dispatches prod; this session never runs `/ship` on its own.

## Review Focus

Inputs the spec implies and no obvious test covers; each has its test pinned to the owning task:

1. A config that still carries stock-only telemetry keys (`logs::sampling`, `metrics::readers`, `traces`, `resource`) must start the agent, not stop it (Task 1, `TestStockTelemetryKeysDoNotStopStart`).
2. `metrics::level: detailed` used to open port 8888; now it must do nothing and say so once in the log, never fail (Task 1, `TestWarnsOnceForIgnoredSettings`).
3. A host whose only network interface is `lo`, or that has no swap, emits fewer metric names; the collection test must assert a subset plus a small must-have set, not equality (Task 2).
4. A loop-mounted filesystem (`/dev/loop3` as the `device` of `system.filesystem.usage`) is dropped by the backend but not by an fs-type rule; the device exclude must catch it (Task 2).
5. The v1.7.0 binary must start with the v1.8.0 config and the v1.8.0 binary with the v1.7.0 config, because a rollback changes one without the other (Task 4, steps 5–6).

---

### Task 1: `internal/selftelemetry` replaces the stock telemetry factory

**Files:**
- Create: `internal/selftelemetry/factory.go`, `internal/selftelemetry/factory_test.go`, `cmd/monitorable-agent/deps_test.go`
- Modify: `cmd/monitorable-agent/main.go` (the `otelconftelemetry` import and `factories.Telemetry = …` in `newFactories`), `cmd/monitorable-agent/config_test.go` (`TestShippedConfigsLoad`, one new test), `go.mod`, `go.sum`

**Interfaces:**
- Produces:

```go
package selftelemetry

type Config struct {
	Logs    LogsConfig     `mapstructure:"logs"`
	Metrics MetricsConfig  `mapstructure:"metrics"`
	Ignored map[string]any `mapstructure:",remain"`
}
type LogsConfig struct {
	Level    zapcore.Level  `mapstructure:"level"`
	Encoding string         `mapstructure:"encoding"`
	Ignored  map[string]any `mapstructure:",remain"`
}
type MetricsConfig struct {
	Level   configtelemetry.Level `mapstructure:"level"`
	Ignored map[string]any        `mapstructure:",remain"`
}

func NewFactory() telemetry.Factory
func (c *Config) Validate() error      // error unless Logs.Encoding is "console" or "json"
func (c *Config) IgnoredKeys() []string // sorted; top-level keys bare, nested ones "logs::<key>" / "metrics::<key>"
```

- [ ] **Step 1: Write the failing tests** in `internal/selftelemetry/factory_test.go`:

```go
func TestDefaultConfig(t *testing.T) {
	cfg := NewFactory().CreateDefaultConfig().(*Config)
	if cfg.Logs.Level != zapcore.InfoLevel || cfg.Logs.Encoding != "console" || cfg.Metrics.Level != configtelemetry.LevelNone {
		t.Fatalf("defaults = %+v", cfg)
	}
}

func TestUnknownKeysAreKeptAside(t *testing.T) {
	cfg := NewFactory().CreateDefaultConfig().(*Config)
	conf := confmap.NewFromStringMap(map[string]any{
		"logs":     map[string]any{"level": "debug", "sampling": map[string]any{"enabled": false}},
		"metrics":  map[string]any{"level": "detailed", "readers": []any{}},
		"traces":   map[string]any{"level": "none"},
		"resource": map[string]any{"service.name": "x"},
	})
	if err := conf.Unmarshal(cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Logs.Level != zapcore.DebugLevel || cfg.Metrics.Level != configtelemetry.LevelDetailed {
		t.Fatalf("known keys not read: %+v", cfg)
	}
	want := []string{"logs::sampling", "metrics::readers", "resource", "traces"}
	if got := cfg.IgnoredKeys(); !slices.Equal(got, want) {
		t.Fatalf("IgnoredKeys = %v, want %v", got, want)
	}
}

func TestValidateRejectsUnknownEncoding(t *testing.T) {
	cfg := NewFactory().CreateDefaultConfig().(*Config)
	cfg.Logs.Encoding = "xml"
	if cfg.Validate() == nil {
		t.Fatal("encoding xml accepted")
	}
}

// observed builds the logger through the factory with a BuildZapLogger hook that
// records the zap.Config it is handed and returns a logger on an observer core.
func observed(t *testing.T, cfg *Config) (*zap.Logger, *observer.ObservedLogs, zap.Config) {
	t.Helper()
	core, logs := observer.New(zapcore.DebugLevel)
	var seen zap.Config
	res := pcommon.NewResource()
	res.Attributes().PutStr("service.name", "monitorable-agent")
	set := telemetry.LoggerSettings{
		Settings:       telemetry.Settings{Resource: &res},
		BuildZapLogger: func(zc zap.Config, _ ...zap.Option) (*zap.Logger, error) { seen = zc; return zap.New(core), nil },
	}
	logger, _, err := NewFactory().CreateLogger(context.Background(), set, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return logger, logs, seen
}

func TestLoggerSamplesAndCarriesNoResource(t *testing.T) {
	logger, logs, seen := observed(t, NewFactory().CreateDefaultConfig().(*Config))
	if seen.Encoding != "console" || !slices.Equal(seen.OutputPaths, []string{"stderr"}) ||
		!slices.Equal(seen.ErrorOutputPaths, []string{"stderr"}) || seen.Level.Level() != zapcore.InfoLevel {
		t.Fatalf("zap config = %+v", seen)
	}
	for i := 0; i < 200; i++ {
		logger.Info("same line")
	}
	if n := logs.Len(); n != 11 { // first 10, then the 110th
		t.Fatalf("200 identical lines produced %d entries, want 11", n)
	}
	for _, e := range logs.All() {
		if _, ok := e.ContextMap()["resource"]; ok {
			t.Fatal("log entry carries a resource field")
		}
	}
}

func TestWarnsOnceForIgnoredSettings(t *testing.T) {
	cfg := NewFactory().CreateDefaultConfig().(*Config)
	cfg.Ignored = map[string]any{"traces": map[string]any{}}
	cfg.Metrics.Level = configtelemetry.LevelDetailed
	_, logs, _ := observed(t, cfg)
	warns := logs.FilterLevelExact(zapcore.WarnLevel).All()
	if len(warns) != 2 {
		t.Fatalf("got %d warnings, want 2: %v", len(warns), warns)
	}
	if !strings.Contains(warns[0].Message+warns[1].Message, "internal metrics are not available in this build") {
		t.Fatalf("no metrics warning in %v", warns)
	}
	joined := fmt.Sprint(warns[0].ContextMap(), warns[1].ContextMap())
	if !strings.Contains(joined, "traces") {
		t.Fatalf("ignored key not named in %v", joined)
	}
}

func TestProvidersAreNoop(t *testing.T) {
	f, cfg := NewFactory(), NewFactory().CreateDefaultConfig()
	mp, err := f.CreateMeterProvider(context.Background(), telemetry.MeterSettings{}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := mp.Meter("x").(metricnoop.Meter); !ok {
		t.Fatalf("meter is %T, want the no-op meter", mp.Meter("x"))
	}
	tp, err := f.CreateTracerProvider(context.Background(), telemetry.TracerSettings{}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tp.Tracer("x").(tracenoop.Tracer); !ok {
		t.Fatalf("tracer is %T, want the no-op tracer", tp.Tracer("x"))
	}
}
```

- [ ] **Step 2: Run** `go test ./internal/selftelemetry/` → FAIL (package has no `NewFactory`).

- [ ] **Step 3: Implement** `internal/selftelemetry/factory.go`: the three config types above, `Validate`, `IgnoredKeys`, and `NewFactory` as `telemetry.NewFactory(createDefaultConfig, telemetry.WithCreateLogger(createLogger))`. `createLogger` follows the Global Constraints, calls `otel.SetErrorHandler` with a handler that logs `"OpenTelemetry internal telemetry error"` at error level, emits the two warnings **before** wrapping the core in the sampler (so they are never sampled away), and returns a nil `component.ShutdownFunc`. The ignored-keys warning carries the keys as one field, `zap.Strings("keys", cfg.IgnoredKeys())`. The package comment says why the stock factory is not used (spec 3.1, two sentences).

- [ ] **Step 4: Run** `go test ./internal/selftelemetry/` → PASS.

- [ ] **Step 5: Write the failing guard tests.** `cmd/monitorable-agent/deps_test.go`:

```go
// TestBinaryLinksNoStockTelemetry fails when the agent's import graph regains the
// collector's stock telemetry factory or anything only it pulled in (45 modules,
// 15 MB of binary in v1.7.0).
func TestBinaryLinksNoStockTelemetry(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	forbidden := []string{
		"go.opentelemetry.io/collector/service/telemetry/otelconftelemetry",
		"go.opentelemetry.io/contrib/otelconf/x",
		"github.com/aws/", "k8s.io/", "sigs.k8s.io/", "github.com/GoogleCloudPlatform/", "golang.org/x/oauth2",
	}
	for _, pkg := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		for _, f := range forbidden {
			if strings.HasPrefix(pkg, f) {
				t.Errorf("binary imports %s", pkg)
			}
		}
	}
}
```

In `config_test.go`, extend `TestShippedConfigsLoad`:

```go
	tc, ok := cfg.Service.Telemetry.(*selftelemetry.Config)
	if !ok {
		t.Fatalf("service.telemetry config is %T, want *selftelemetry.Config", cfg.Service.Telemetry)
	}
	if tc.Metrics.Level != configtelemetry.LevelNone || len(tc.IgnoredKeys()) != 0 {
		t.Errorf("shipped telemetry = %+v, ignored %v; want metrics level none and nothing ignored", tc, tc.IgnoredKeys())
	}
```

and add `TestStockTelemetryKeysDoNotStopStart`: read the shipped config, replace the exact text `"  telemetry:\n    metrics:\n      level: none\n"` with a block that sets `logs: {level: debug, encoding: json, sampling: {enabled: false}}`, `metrics: {level: detailed, readers: []}`, `traces: {level: none}`, `resource: {service.name: x}`; fail the test if the replacement matched nothing; write it to `t.TempDir()`; `otelcoltest.LoadConfigAndValidate` must return no error and `IgnoredKeys()` must equal `["logs::sampling", "metrics::readers", "resource", "traces"]`. Same three `t.Setenv` lines as `TestShippedConfigsLoad`.

- [ ] **Step 6: Run** `go test ./cmd/monitorable-agent/` → FAIL (`TestBinaryLinksNoStockTelemetry` lists `otelconftelemetry`, AWS and Kubernetes packages; the telemetry config is `*otelconftelemetry.Config`).

- [ ] **Step 7: Switch the factory.** In `main.go` replace the `otelconftelemetry` import with `github.com/monitorable-io/monitorable-agent/internal/selftelemetry` and set `factories.Telemetry = selftelemetry.NewFactory()`; rewrite the comment above it (the factory is ours; why). Run `go mod tidy`.

- [ ] **Step 8: Verify.** `go build ./... && go vet ./... && go test -race ./...` → PASS. `wc -l go.mod go.sum` → about 184 and 450 lines (were 229 and 584). `go build -o /tmp/agent-t1 ./cmd/monitorable-agent && ls -l /tmp/agent-t1` → about 42 MB (was 57.2 MB).

- [ ] **Step 9: Commit** `feat(telemetry): a telemetry factory of our own; drop the stock one and 45 modules`.

### Task 2: The shipped config collects only what the backend stores

**Files:**
- Create: `cmd/monitorable-agent/hostmetrics_shipped_test.go`
- Modify: `distribution/configs/linux/collector-config.yaml` (the `host_metrics.scrapers` block and the comment above it)

**Interfaces:**
- Consumes: `newFactories()` from `main.go`; the shipped config path used by `TestShippedConfigsLoad`.

- [ ] **Step 1: Write the failing test** `TestShippedHostMetricsEmitOnlyStoredSeries`: load the shipped config with `otelcoltest.LoadConfigAndValidate` (same three `t.Setenv` lines); take `cfg.Receivers[component.MustNewID("host_metrics")]` and the `host_metrics` factory from `newFactories().Receivers`; `CreateMetrics(ctx, receivertest.NewNopSettings(factory.Type()), hmCfg, sink)` with a `*consumertest.MetricsSink`; `Start(ctx, componenttest.NewNopHost())`; poll up to 10 s until the sink's metric names include all of the must-have set; `Shutdown`. Then assert, over every datapoint of every metric the sink holds:

```go
var stored = map[string]bool{
	"system.cpu.load_average.1m": true, "system.cpu.load_average.5m": true, "system.cpu.load_average.15m": true,
	"system.memory.usage": true, "system.memory.utilization": true, "system.memory.limit": true,
	"system.filesystem.usage": true,
	"system.disk.io": true, "system.disk.operations": true, "system.disk.io_time": true,
	"system.network.io": true, "system.network.packets": true, "system.network.dropped": true,
	"system.network.errors": true, "system.network.connections": true,
	"system.paging.usage": true,
}
var mustHave = []string{"system.cpu.load_average.1m", "system.memory.usage", "system.memory.limit", "system.filesystem.usage"}
var backendDeniesDevice = regexp.MustCompile(`^(lo|docker0|veth.+|br-.+|loop\d+|/dev/loop\d+)$`)
```

- every metric name is in `stored` (`t.Errorf("%s is collected but the backend does not store it", name)`);
- no datapoint has a `device` attribute matching `backendDeniesDevice` (`t.Errorf("%s{device=%q} is collected but the backend drops that device", name, dev)`);
- every `mustHave` name is present.

Handle both `pmetric.MetricTypeGauge` and `pmetric.MetricTypeSum`; fail on any other type.

A host with no loop-mounted filesystem never exercises the filesystem device rule, so pin it on the file as well. `TestShippedConfigExcludesLoopFilesystems`: `confmaptest.LoadConf(path)` (package `go.opentelemetry.io/collector/confmap/confmaptest`), then

```go
	got := conf.Get("receivers::host_metrics::scrapers::filesystem::exclude_devices::devices")
	if want := []any{`^/dev/loop\d+$`}; !reflect.DeepEqual(got, want) {
		t.Errorf("filesystem exclude_devices.devices = %v, want %v", got, want)
	}
	if mt := conf.Get("receivers::host_metrics::scrapers::filesystem::exclude_devices::match_type"); mt != "regexp" {
		t.Errorf("filesystem exclude_devices.match_type = %v, want regexp", mt)
	}
```

- [ ] **Step 2: Run** `go test ./cmd/monitorable-agent/ -run 'TestShippedHostMetricsEmitOnlyStoredSeries|TestShippedConfigExcludesLoopFilesystems' -v` → FAIL: the first names `system.disk.merged`, `system.paging.faults` and others, and `system.network.io{device="lo"}`; the second reports `<nil>`.

- [ ] **Step 3: Edit the config** to the YAML of spec 3.3 (four scraper blocks; `filesystem` keeps its existing `include_virtual_filesystems` and `exclude_fs_types`). Rewrite the comment above `scrapers:`: the selection is what the backend's data catalog stores; a device or metric the backend drops is not collected; a metric added to the catalog later must be switched on here in an agent release.

- [ ] **Step 4: Run** `go test -race ./cmd/monitorable-agent/` → PASS (this test and `TestShippedConfigsLoad`).

- [ ] **Step 5: Commit** `feat(dist): collect only the series the backend stores`.

### Task 3: Stripped release binaries, size budget, docs

**Files:**
- Modify: `scripts/publish-dist.sh` (`build()`, the `-ldflags` line), `.github/workflows/validate.yml` (after the "Signing dry-run (publish-dist.sh --render-only …)" step), `CLAUDE.md` ("Key Concepts"), `distribution/README.md` ("Publishing")

- [ ] **Step 1: Add the CI check** as a new step right after the signing dry-run step, which leaves the release artifacts in `distribution-build/binaries/otel/`:

```yaml
      - name: Release binary size budget (stripped, no stock telemetry)
        run: |
          grep -q -- '-ldflags "-s -w -X ' scripts/publish-dist.sh
          test "$(stat -c %s distribution-build/binaries/otel/monitorable-agent-linux-amd64)" -le 33000000
          test "$(stat -c %s distribution-build/binaries/otel/monitorable-agent-linux-arm64)" -le 31000000
```

- [ ] **Step 2: Run it locally** the way the dry-run step does (copy that step's commands, throwaway key included), then the three lines above → FAIL at the `grep`.

- [ ] **Step 3: Change** the `build()` flags to `-ldflags "-s -w -X github.com/monitorable-io/monitorable-agent/internal/version.Version=${VERSION}"` and extend the comment above `build()`: no debug info or symbol table; panics still print stack traces.

- [ ] **Step 4: Run** the same commands → PASS; sizes about 29.2 MB and 26.9 MB. `distribution-build/binaries/otel/monitorable-agent-linux-amd64 --version` prints the version. `shellcheck scripts/publish-dist.sh` → clean.

- [ ] **Step 5: Docs.** `CLAUDE.md` "Key Concepts": one bullet "Self-telemetry" (package, the three keys, ignored keys warn, why not the stock factory, the guard test and the size budget) and one bullet "Collected set" (the config is pruned to the backend's data catalog; adding a stored metric needs an agent release; `TestShippedHostMetricsEmitOnlyStoredSeries`). `distribution/README.md` "Publishing": one paragraph on stripped binaries and the budget.

- [ ] **Step 6: Commit** `build(dist): strip release binaries; size budget in CI`.

### Task 4: Acceptance run, PR, review

Spec 3.5. Needs a Linux host with Docker running (the dev server qualifies). `SINK` is `sink.py` from the platform checkout, `docs/superpowers/notes/2026-11-24-agent-resource-analysis/sink.py` (`python3 -I sink.py <port> <log.jsonl> <dump-dir>`); if it is missing, any HTTP server that answers 200 to POST and logs `Content-Length` will do.

- [ ] **Step 1: Build both.** Branch: `CGO_ENABLED=0 go build -ldflags "-s -w -X github.com/monitorable-io/monitorable-agent/internal/version.Version=v1.8.0" -o "$W/new" cmd/monitorable-agent/main.go`. Old: `git worktree add "$W/v170" v1.7.0` and `CGO_ENABLED=0 go build -C "$W/v170" -ldflags "-X github.com/monitorable-io/monitorable-agent/internal/version.Version=v1.7.0" -o "$W/old" cmd/monitorable-agent/main.go`. `$W` is a scratch directory outside the repo.
- [ ] **Step 2: Run both for 6 minutes**, each with its own sink port, its own `MONITORABLE_STATE_DIR`, `MONITORABLE_API_KEY=acceptance-not-a-key`, and its own tree's `distribution/configs/linux/collector-config.yaml`. Every 10 s record for each PID `Pss:` from `/proc/$PID/smaps_rollup` and fields 14+15 of `/proc/$PID/stat`. Never use `pkill -f`; stop by PID.
- [ ] **Step 3: Check** against spec 3.5: sizes (item 1); PSS median new ≤ old − 6 MB (item 2); CPU ticks delta new ≤ 1.05 × old (item 3); mean `wire` bytes of `/v1/metrics` requests after the first, new < old (item 4). Expected from the design run: 33.5 vs 42.2 MB, 141 vs 140 ms/min, 6.6 vs 9.1 KB.
- [ ] **Step 4:** Both logs: no line at `error` level; the only `warn` allowed is the SMART permission line (the run is not root).
- [ ] **Step 5: Rollback pair A.** `$W/old` with the branch's config for 90 s → "Everything is ready", one batch at the sink, no error.
- [ ] **Step 6: Rollback pair B.** `$W/new` with `$W/v170`'s config for 90 s → same.
- [ ] **Step 7:** Remove the worktree (`git worktree remove "$W/v170"`). Push `feat/lean-agent`, open the PR: title `feat: lean agent build and collection (v1.8.0)`; body: spec link, the table of Step 3 with the measured numbers, the five Review Focus points, the note that log lines lose the `resource` block. Request the Opus review of the whole branch; answer every finding on the branch.

### Task 5: Release gate, prod, public statements, handoff

- [ ] **Step 1 (founder):** merge; tag `v1.8.0` (`release.yml` publishes to staging).
- [ ] **Step 2: Staging acceptance (release gate).** On a staging-enrolled box, update to v1.8.0 from `install-mon.ok9k.com`. After 30 minutes: every panel of the server page that had data before the update has data after it with no gap (CPU, memory, swap, load, disk I/O, disk usage, network throughput/packets/errors, TCP connections, temperature, ports, containers, services, SMART); `journalctl -u monitorable-agent --since "-30min" -p warning` shows nothing new; `ls -l /opt/monitorable/monitorable-agent` shows about 29 MB; record `Pss:` from `smaps_rollup`. Put the results in the PR.
- [ ] **Step 3 (founder):** `publish-prod` dispatch for `v1.8.0`.
- [ ] **Step 4: Changelog** (landing repo, merge = prod), `src/content/changelog/<prod date>-agent-smaller-binary.md`, label `Improved`, title `Smaller agent download and memory use`; body: the binary is about 30 MB instead of about 60 MB and the agent uses less memory (state the staging figure, rounded toward the old value); it no longer reads counters Monitorable never stored (Docker's virtual network interfaces, loop devices, seven disk, filesystem and swap metrics), so nothing in the dashboard changes; needs agent v1.8.0 or later; link `/docs/agent/updating-uninstalling/`. `npm test` passes.
- [ ] **Step 5: Site wording, only on the founder's ruling** (spec 3.6). Default: "a binary of about 60 MB" becomes "about 30 MB (since v1.8.0)" in `src/data/ai-info.ts` (`footprint`), `src/content/docs/docs/faq.md`, `src/content/docs/docs/agent/requirements-permissions.md`, `src/components/Faq.astro`; "less than 50 MB of memory" stays; the benchmark page and its JSON are not touched. Same PR as Step 4 if ruled by then. Platform repo, separate PR: `agent_docs/procedures/agent-footprint-benchmark.md` (the founder's wording line) and `agent_docs/procedures/collector-distribution.md` ("Metrics collected": the pruned set and the catalog coupling; "Publishing": stripped binaries).
- [ ] **Step 6: Hand off:** `/handoff` with the acceptance and staging numbers; Part 2 (send delay, TCP state counts) is planned next in a fresh planning session.
