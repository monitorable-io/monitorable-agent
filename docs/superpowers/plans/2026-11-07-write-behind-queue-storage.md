# Write-behind queue storage (agent v1.7.0) — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans (Native) to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Execution mode: Native** (one session implements every task, one fresh Opus review of the whole branch at the end). Reason: one package with one shared data structure; tasks 2–5 all shape the same `client` type, nothing compiles usefully between them for a second agent, and the whole change is ~500 lines.

**Goal:** The agent's persistent export queue writes nothing to disk while exports succeed, persists only batches still waiting after 15 s (or when 4 MiB is pending, or at a graceful stop), and keeps the existing bbolt file and format.

**Architecture:** A new extension `write_behind_storage` implements `storage.Extension`, resolves the configured `file_storage` extension at start and wraps the client it hands out. The wrapper keeps pending writes and tombstones in memory and flushes them to the wrapped client in one `Batch` (one bbolt transaction) when an entry ages past the window, when pending bytes pass the cap, or at close. Reads are served from memory first, then read through.

**Tech Stack:** Go 1.27; collector core v1.68.0 (`extension`, `component`, `extension/xextension/storage` v0.162.0); contrib `filestorage` v0.162.0; `go.uber.org/zap`; tests with `testing` + a recording fake `storage.Client`.

**Spec:** `docs/superpowers/specs/2026-11-07-write-behind-queue-storage-design.md` (read it first; every number below comes from it).

## Global Constraints

- Component type `write_behind_storage`; package `internal/extension/writebehindstorage`.
- Defaults: `flush_after: 15s`, `max_buffer_bytes: 4194304` (4 MiB; a plain byte count in YAML).
- Wrapped `GetClient(ctx, kind, id, name)` is called with the arguments **unchanged** (keeps the file name `exporter_otlp_http__metrics`).
- One wrapped `Batch` per flush, metadata key included whenever dirty. Nothing is ever dropped by the wrapper.
- Tombstones age like items. Metadata alone never triggers a flush.
- A failed flush surfaces only on a `Batch` that contains a `Set` of a non-metadata key (an offer); never on `Get`, `Delete`, or read-shaped `Batch`es; clears after a successful flush.
- The mutex is held for the whole flush (snapshot-outside-lock only if Task 6 shows stalls).
- `service.extensions` lists `file_storage` before `write_behind_storage`.
- Commits follow Conventional Commits; no Claude attribution lines. Branch `feat/write-behind-queue`, PR to `main`, founder merges and tags.

## Review Focus

Inputs the spec implies and no obvious test covers; each has its test pinned to the owning task:

1. A `Batch` that sets a key and gets the same key in one call must return the value just set (Task 2).
2. `Set` on a tombstoned key revives it with a fresh `since`; the old delete must not be flushed over the new value (Task 3).
3. `pendingBytes` must fall when a dirty entry is deleted before its flush and after eviction, so the cap never trips on bytes that are gone (Task 3).
4. `Close` twice (the queue's `unrefClient` then the extension's `Shutdown`) must flush once and not panic or double-close the wrapped client (Task 4).
5. A `Get` for a key that exists on disk but was never seen in this process (legacy `ri`/`wi`/`di`, or a crash-recovered item) reads through; a following `Delete` of it becomes a tombstone that reaches disk (Task 2 + Task 3).

---

### Task 1: Extension skeleton — config, factory, start-time lookup, client hand-out

**Files:**
- Create: `internal/extension/writebehindstorage/config.go`, `factory.go`, `extension.go`, `extension_test.go`
- Modify: `cmd/monitorable-agent/main.go` (register in `factories.Extensions`), `cmd/monitorable-agent/main_test.go:33-36` (assert registration)

**Interfaces:**
- Produces:
  ```go
  type Config struct {
      Storage        component.ID  `mapstructure:"storage"`
      FlushAfter     time.Duration `mapstructure:"flush_after"`
      MaxBufferBytes int64         `mapstructure:"max_buffer_bytes"`
  }
  func (c *Config) Validate() error   // Storage set, FlushAfter > 0, MaxBufferBytes > 0
  func NewFactory() extension.Factory // type "write_behind_storage", stability Beta, defaults 15s / 4194304
  type writeBehind struct{ ... }      // implements storage.Extension
  func (w *writeBehind) Start(ctx context.Context, host component.Host) error
  func (w *writeBehind) Shutdown(ctx context.Context) error
  func (w *writeBehind) GetClient(ctx context.Context, kind component.Kind, id component.ID, name string) (storage.Client, error)
  ```
- Consumes from Task 2: `newClient(wrapped storage.Client, cfg Config, logger *zap.Logger, now func() time.Time) *client` (until Task 2 lands, `GetClient` returns the wrapped client directly).

- [ ] **Step 1: Write the failing tests** in `extension_test.go`: `TestConfigValidate` (zero `Storage` → error; `FlushAfter` 0 → error; `MaxBufferBytes` 0 → error; defaults from `createDefaultConfig` pass); `TestStartFailsWithoutWrappedStorage` (a `component.Host` whose `GetExtensions()` is empty → `Start` error contains `"write_behind_storage: storage extension \"file_storage\" not found"`; a host whose entry is a non-storage `component.Component` → error contains `"is not a storage extension"`); `TestGetClientForwardsArgumentsUnchanged` (fake `storage.Extension` records `(kind, id, name)`; call with `component.KindExporter`, `component.MustNewID("otlp_http")`, `"metrics"`; assert equality).
- [ ] **Step 2: Run** `go test ./internal/extension/writebehindstorage/ -run 'TestConfigValidate|TestStart|TestGetClient' -v` → FAIL (package does not exist).
- [ ] **Step 3: Implement** `config.go`, `factory.go` (`extension.NewFactory(component.MustNewType("write_behind_storage"), createDefaultConfig, create, component.StabilityLevelBeta)`), `extension.go` (`Start` resolves `host.GetExtensions()[cfg.Storage]`, type-asserts `storage.Extension`, stores it; `GetClient` forwards; `Shutdown` closes clients handed out, tracked in a slice under a mutex).
- [ ] **Step 4: Run** the three tests → PASS.
- [ ] **Step 5: Register** in `main.go` `factories.Extensions` with a one-line comment; extend `TestFactoriesRegisterShippedComponents` to assert `"write_behind_storage"` is registered. Run `go test ./cmd/... ./internal/extension/...` → PASS.
- [ ] **Step 6: Commit** `feat(queue): write_behind_storage extension skeleton wrapping file_storage`.

### Task 2: In-memory client — Set, Get, Delete, Batch

**Files:**
- Create: `internal/extension/writebehindstorage/client.go`, `client_test.go`, `fake_test.go` (recording fake `storage.Client`: in-memory map, `calls []string` like `"Batch[Set qmv0,Set 00000000000000000007]"`, injectable error)
- Modify: `extension.go` (`GetClient` wraps with `newClient`)

**Interfaces:**
- Produces:
  ```go
  type entry struct{ value []byte; dirty, tombstone, persisted bool; since time.Time }
  type client struct { mu sync.Mutex; wrapped storage.Client; cfg Config; log *zap.Logger; now func() time.Time
                       entries map[string]*entry; pendingBytes int64; flushErr error; closed bool; stop chan struct{} }
  func newClient(wrapped storage.Client, cfg Config, logger *zap.Logger, now func() time.Time) *client
  // storage.Client methods; const metadataKey = "qmv0"
  func (c *client) applyLocked(op *storage.Operation) error   // one op under c.mu; Batch and the single-op methods call it
  ```
- Consumes: Task 1 types.

- [ ] **Step 1: Write the failing tests**: `TestSteadyStateTouchesDiskNever` (`Batch(Set qmv0, Set item1)`, `Batch(Set qmv0, Get item1)` returns the bytes, `Batch(Set qmv0, Delete item1)`; fake records **zero** calls; `pendingBytes == 0` after the delete); `TestGetMissReadsThroughAndCaches` (fake holds `k→v`; first `Get` records one fake `Get`, second records none; entry `persisted == true`); `TestDeleteOfUnseenKeyIsTombstone` (`Delete("ri")` on an empty client → entry tombstone+dirty, no fake call); `TestBatchSetThenGetSameKey` (Review Focus 1: `Batch(Set k v, Get k)` → the Get op's `Value == v`); `TestGetAfterDeleteIsNil` (set, delete, `Get` → `nil, nil`, no fake call).
- [ ] **Step 2: Run** `go test ./internal/extension/writebehindstorage/ -run 'TestSteady|TestGetMiss|TestDeleteOfUnseen|TestBatchSetThenGet|TestGetAfterDelete' -v` → FAIL.
- [ ] **Step 3: Implement** `client.go` per spec 5.3 (no flushing yet; `since` set only when an entry becomes dirty; `Delete` of a non-persisted entry removes it and subtracts its bytes; of a persisted or unknown key makes a tombstone). `GetClient` now returns `newClient(...)`.
- [ ] **Step 4: Run** → PASS. `go vet ./...` clean.
- [ ] **Step 5: Commit** `feat(queue): write-behind client — memory-first Set/Get/Delete/Batch`.

### Task 3: Flush — age and cap triggers, one transaction, eviction

**Files:**
- Modify: `client.go`, `client_test.go`

**Interfaces:**
- Produces:
  ```go
  func (c *client) flushLocked(ctx context.Context, reason string) error  // builds the wrapped Batch, applies success bookkeeping
  func (c *client) dueLocked() bool                                        // oldest dirty item/tombstone older than cfg.FlushAfter, or pendingBytes > cfg.MaxBufferBytes
  func (c *client) tick(ctx context.Context)                              // called every second by the goroutine started in newClient; flushes when due
  ```
- Consumes: Task 2 `client`, `entry`.

- [ ] **Step 1: Write the failing tests** (fake clock `now` advanced by hand; call `tick` directly rather than waiting): `TestItemOlderThanWindowFlushesWithMetadata` (set qmv0+item at t0, advance 15 s + 1 ms, `tick` → exactly one fake `Batch` containing `Set qmv0` and `Set item`; afterwards the item entry has `persisted == true` and `value == nil` (evicted), qmv0 keeps its value, `pendingBytes == 0`); `TestMetadataAloneNeverFlushes` (only qmv0 dirty, advance 1 h, `tick` → no fake call); `TestCapFlushesAtOnce` (`MaxBufferBytes = 100`, set two 60-byte items → the second `Set` returns after one fake `Batch`; `pendingBytes == 0`); `TestTombstoneAgesOut` (fake holds item on disk; `Delete` it; advance 15 s; `tick` → one `Batch` with `Delete item` (+ `Set qmv0` if dirty); entry gone); `TestReviveAfterTombstone` (Review Focus 2: delete persisted key, then `Set` it, advance, flush → the `Batch` carries `Set k`, not `Delete k`); `TestPendingBytesTracksDeletesAndEviction` (Review Focus 3: set 3 items (300 B), delete one (200 B), flush → 0; set one more → its size only); `TestGetOfUnseenThenDeleteReachesDisk` (Review Focus 5: fake has `wi`; `Get("wi")` reads through; `Delete("wi")`; advance; flush → `Delete wi` in the batch).
- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Implement** `dueLocked`, `flushLocked` (ops = `SetOperation` per dirty non-tombstone incl. metadata, `DeleteOperation` per tombstone; on success mark persisted, clear dirty, drop tombstones, evict item values, recompute `pendingBytes`; debug log `keys`, `bytes`, `tombstones`, `reason`), `tick`, and the goroutine (`time.NewTicker(time.Second)`, stopped by `stop`). Wire the cap check into `applyLocked` for `Set`.
- [ ] **Step 4: Run** → PASS; run the whole package with `-race`.
- [ ] **Step 5: Commit** `feat(queue): flush on age, cap and metadata ride-along; evict persisted items`.

### Task 4: Close, failure semantics, extension shutdown

**Files:**
- Modify: `client.go`, `extension.go`, `client_test.go`, `extension_test.go`

**Interfaces:**
- Produces: `func (c *client) Close(ctx context.Context) error` (idempotent: stop ticker, flush with reason `"close"`, `wrapped.Close`, info log with totals; second call returns nil and does nothing). Extension `Shutdown` calls `Close` on every client it handed out.

- [ ] **Step 1: Write the failing tests**: `TestCloseFlushesEverything` (dirty item + tombstone + qmv0 → one `Batch` with all three, then one fake `Close`); `TestCloseTwiceIsSafe` (Review Focus 4: two `Close` calls → one `Batch`, one `Close`, nil errors; `Shutdown` after `Close` likewise); `TestFlushErrorSurfacesOnOfferOnly` (fake `Batch` returns `errDisk`; age an item, `tick` → logged, entries still dirty; then `Batch(Set qmv0, Get item)` → **nil** error; `Delete(item)` → nil; `Batch(Set qmv0, Set item2)` → `errDisk`; fake error cleared; `tick` flushes; `Batch(Set qmv0, Set item3)` → nil); `TestFlushErrorKeepsStateForRetry` (after the failed flush the entries keep `dirty == true` and `pendingBytes` unchanged).
- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Implement** `Close`, the `flushErr` field (set on failure, cleared on success, returned by `applyLocked` only for a `Set` op whose key != `metadataKey`; error log once per failure streak), extension `Shutdown`.
- [ ] **Step 4: Run** package with `-race` → PASS.
- [ ] **Step 5: Commit** `feat(queue): close flushes; a failed flush fails the next offer, never a read`.

### Task 5: Model test — random operations against a reference, crash snapshots

**Files:**
- Create: `internal/extension/writebehindstorage/model_test.go`

- [ ] **Step 1: Write** `TestRandomOpsMatchReferenceModel`: seeded `math/rand/v2`, 20 keys (`qmv0` + items), 5 000 operations drawn from Set/Get/Delete/Batch(2–4 ops), clock advanced 0–20 s between ops, `tick` after each; every `Get` and every `Get` inside a `Batch` must equal a plain `map[string][]byte` reference. Then `TestCrashLeavesConsistentSnapshot`: same driver, but after every flush record the fake's disk map; at the end drop the client (no `Close`) and assert the fake's disk equals the last recorded snapshot exactly (nothing partial ever reaches it).
- [ ] **Step 2: Run** with `-race -count=3` → PASS (fix anything it finds in Tasks 2–4 code; commit the fix with the test).
- [ ] **Step 3: Commit** `test(queue): randomised model and crash-snapshot tests`.

### Task 6: Integration with the real queue and `file_storage`

**Files:**
- Create: `internal/extension/writebehindstorage/integration_test.go`

**Interfaces:**
- Consumes: contrib `filestorage.NewFactory()`; `exporterhelper.NewMetrics(ctx, exporter.Settings, cfg, push, exporterhelper.WithQueue(q))` with `q := exporterhelper.NewDefaultQueueConfig()` and `q.StorageID = &wbID` (field and constructor names as in exporterhelper v0.162.0 — verify in `~/go/pkg/mod/go.opentelemetry.io/collector/exporter/exporterhelper@v0.162.0/`); a test `component.Host` whose `GetExtensions()` returns `{file_storage: fs, write_behind_storage: wb}`; `go.etcd.io/bbolt` (already an indirect dependency) to count keys in the file read-only.

- [ ] **Step 1: Write** `TestQueueThroughWriteBehindLeavesNoItemsOnDisk`: temp dir; start `file_storage` (fsync on) then `write_behind_storage` (window 15 s, fake-free: real clock); exporter with a push function that succeeds; `ConsumeMetrics` 20 small metric requests; wait until the push count is 20; `Shutdown` exporter, then extensions in reverse order; open `exporter_otlp_http__metrics` with bbolt read-only and assert no item keys remain (only `qmv0`). Then `TestQueueRecoversItemsOlderThanWindow`: push function returns a retryable error; set the window to 200 ms for the test; consume 5 requests; wait 1 s; **do not shut down** (simulate a kill: close the wrapped bbolt by shutting down `file_storage` only, then drop everything); start a fresh pair on the same dir with a succeeding push → the 5 requests arrive.
- [ ] **Step 2: Run** `go test ./internal/extension/writebehindstorage/ -run Integration -v` → PASS. Note the wall time of the flush under `-v` logs; if a flush exceeds 50 ms on the dev box, record it in the PR description (spec 5.3 fallback is the reviewer's call, not this task's).
- [ ] **Step 3: Commit** `test(queue): integration with exporterhelper persistent queue and file_storage`.

### Task 7: Shipped config, tests, docs

**Files:**
- Modify: `distribution/configs/linux/collector-config.yaml:76-131` (extension block, `sending_queue.storage`, `service.extensions`, rewrite the queue comment: window, cap, what a hard kill loses, that the file is unchanged); `cmd/monitorable-agent/config_test.go` (`TestShippedConfigsLoad`: assert `cfg.Service.Extensions == [file_storage, write_behind_storage]` in that order and the exporter's `sending_queue.storage` is `write_behind_storage`); `CLAUDE.md` "Persistent export queue (Linux)" bullet; `distribution/README.md` "Retry and offline buffering" section (one paragraph).

- [ ] **Step 1: Extend** `TestShippedConfigsLoad` with the two assertions; run → FAIL (config not yet changed).
- [ ] **Step 2: Edit** the YAML per spec 5.1 (`write_behind_storage: {storage: file_storage, flush_after: 15s, max_buffer_bytes: 4194304}`); run `go test ./cmd/...` → PASS.
- [ ] **Step 3: Docs**: the two paragraphs. Build once: `go build ./cmd/monitorable-agent && MONITORABLE_ENDPOINT=http://127.0.0.1:1 MONITORABLE_API_KEY=x MONITORABLE_STATE_DIR=/tmp/wb ./monitorable-agent --config distribution/configs/linux/collector-config.yaml` for 20 s → starts, creates `/tmp/wb/exporter_otlp_http__metrics`, logs no error about the extension.
- [ ] **Step 4: Commit** `feat(dist): ship the write-behind queue (15 s window, 4 MiB cap)`.

### Task 8: PR, review, release gate, prod, changelog

- [ ] **Step 1:** Push `feat/write-behind-queue`, open the PR (title `feat(queue): write-behind persistent queue storage (v1.7.0)`; body: spec link, the five Review Focus points, the integration-test flush timing). Request the Opus review of the whole branch; answer every finding on the branch.
- [ ] **Step 2 (founder):** merge; tag `v1.7.0` (`release.yml` publishes to staging).
- [ ] **Step 3: Staging acceptance (release gate, spec 6):** on a staging-enrolled box (dev 1 or a Hetzner spare), install `v1.7.0` from `install-mon.ok9k.com`; after 5 min record `write_bytes` from `/proc/$(systemctl show -p MainPID --value monitorable-agent)/io`; after a further 2 h record again → growth < 2 MB (< 1 MB/h). Then block `ingest-mon.ok9k.com` with a ufw rule for 3 min, `systemctl restart monitorable-agent`, unblock → the gap's batches arrive (dashboard shows no hole longer than 15 s + one interval). Record both results in the PR.
- [ ] **Step 4 (founder):** `publish-prod` dispatch for `v1.7.0`.
- [ ] **Step 5:** Landing changelog entry `src/content/changelog/<prod date>-agent-queue-disk-writes.md`, label `Improved`, title ≤70 chars, body: the agent no longer writes every batch to disk; unsent data still survives restarts and outages; a power cut can lose up to 15 seconds; needs agent v1.7.0 or later. PR in the landing repo (merge = prod).
- [ ] **Step 6:** Hand off: `/handoff` with the measured numbers; the benchmark re-run (Part B plan) may start once v1.7.0 is on prod.
