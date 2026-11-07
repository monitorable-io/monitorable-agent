# Write-behind storage for the persistent export queue — design

Date: 2026-11-07. Status: design, builder-checked, awaiting founder approval. Release target: **v1.7.0**.

## 1. Intent

The agent's `otlp_http` exporter keeps its unsent batches in a persistent queue
(`sending_queue.storage: file_storage`, bbolt file at
`/var/lib/monitorable/queue/exporter_otlp_http__metrics`, 1440 requests, fsync on).
The OpenTelemetry persistent queue writes every batch to that file *before* sending it
and deletes it after the send succeeds. On a quiet box that is one 50–100 KB write per
minute: the 2026 agent-footprint pilot measured **115 MB of disk writes per 24 h** for
our agent, fourth-highest of twelve rows, while every export succeeded. Static agents
with no queue write 0.

We keep the promise the queue exists for — unsent data survives restarts and
multi-hour backend outages — and stop paying for it on every successful minute. The
change ships as a normal release, before the benchmark re-run, so the re-run measures
the current release of every agent, ours included.

## 2. Goals, non-goals, anti-goals

Goals
- Steady state (every export succeeds within seconds): **no disk writes** from the queue.
- Durability window: a batch that still waits after **15 s** is on disk. A graceful stop
  (SIGTERM, `systemctl restart/stop`) writes everything out first. A hard kill or power
  cut loses at most the batches younger than 15 s (founder: acceptable).
- Outage behaviour unchanged: during a backend outage the queue fills the bbolt file
  exactly as today; **memory stays bounded** (persisted items are evicted from memory).
- Hard cap on the in-memory buffer: **4 MiB** of pending values by default; above it the
  wrapper flushes at once instead of waiting out the window. Nothing is ever dropped by
  the wrapper; dropping stays the queue's own `queue_size` overflow rule.
- On-disk format, file name and `file_storage` settings (fsync, compaction) unchanged:
  an upgrade from v1.6.0 reads its existing queue file; a downgrade reads what v1.7.0
  flushed.
- Configurable (`flush_after`, `max_buffer_bytes`), with the shipped defaults above.
  Setting `sending_queue.storage: file_storage` again is the rollback.

Non-goals
- Changing queue size, retry policy, batching, or the exporter.
- Contributing the mode upstream now (worth proposing once it runs in production).
- Anything outside the Linux distribution config.

Anti-goals
- A benchmark-only configuration. The shipped default is what is measured.
- Memory growth during an outage (the pilot measures memory too).
- Losing more than the stated window, or losing anything on a graceful stop.

## 3. Constraints and facts the design rests on

- Toolchain: Go 1.27, collector core v1.68.0 / contrib v0.162.0 (`exporterhelper`
  v0.162.0, `extension/xextension/storage` v0.162.0, contrib `filestorage` v0.162.0).
- The persistent queue (`exporterhelper/internal/queue/persistent_queue.go`) talks to
  storage only through `storage.Client` (`Get`, `Set`, `Delete`, `Batch`, `Close`) with
  two kinds of keys: the metadata record `qmv0` and one item per batch (`getItemKey(index)`).
  Writes are `Batch(Set metadata, Set item)` on offer, `Batch(Set metadata, Get item)` on
  read, `Batch(Set metadata, Delete item)` when a send finishes. Legacy keys `ri`/`wi`/`di`
  are read once at start.
- The exporter resolves `sending_queue.storage: <id>` via `host.GetExtensions()[id]`
  and requires the extension to implement `storage.Extension` (`GetClient(ctx, kind,
  id, storageName)`). `file_storage` derives the bbolt file name from exactly those
  three arguments (`exporter_otlp_http__metrics`), so a wrapper that forwards them
  unchanged keeps the same file.
- Shutdown order: the service stops pipelines (receivers → processors → exporters)
  before extensions; the queue's `Shutdown` calls `client.Close` once the last reference
  is released. Our `Close` therefore runs before our extension's `Shutdown`.
- The queue tolerates a missing item key after a restart: an item that was in flight at
  the last stop is reported at Warn ("Failed retrieving item"); an ordinary read of a
  missing item logs "Failed to dispatch item" at Debug and finishes it (metadata set,
  item deleted). Recovery is the same either way; at the default log level the gap a
  lost window leaves is mostly silent.
- `file_storage`'s `Batch` is one bbolt transaction (fsync as configured), so a flush
  that carries metadata and items in one `Batch` is crash-atomic.
- Extensions start in the order of `service.extensions` and stop in reverse (collector
  service v0.162.0). The wrapper looks `file_storage` up at runtime and declares no
  dependency, so `[file_storage, write_behind_storage]` is what guarantees the wrapped
  store is alive while the wrapper flushes at shutdown. A shipped-config test asserts
  that order (6).

## 4. Approaches considered

1. **Wrapper extension around `file_storage`** — a new extension in our distribution
   that looks up the configured `file_storage` instance at start and hands the exporter
   a client that buffers in memory and flushes to the wrapped client. Keeps bbolt,
   fsync, compaction and the file format; only our component changes. **Chosen** (founder,
   2026-11-07): smallest change that keeps every existing guarantee and format.
2. Fork of contrib `filestorage` with a lazy mode — full control of the bbolt
   transactions (one per window), but a fork to maintain against contrib releases.
3. Upstream a write-behind option into contrib `filestorage` — the right long-term home;
   weeks of review and not ours to decide. To be proposed after 1 runs in production.

Rejected without discussion: memory-only queue (loses the outage promise; memory grows
during an outage), and `fsync: false` (bbolt page writes still reach the disk at
writeback; the bytes do not change).

## 5. Design

### 5.1 Component

- Type `write_behind_storage`, package `internal/extension/writebehindstorage`,
  registered in `newFactories()` (`cmd/monitorable-agent/main.go`) next to `file_storage`.
- Implements `storage.Extension`. Config:

  ```yaml
  extensions:
    file_storage: { … unchanged … }
    write_behind_storage:
      storage: file_storage        # component ID of the wrapped storage extension
      flush_after: 15s             # a pending write older than this goes to disk
      max_buffer_bytes: 4194304    # 4 MiB; pending bytes above this flush at once
  exporters:
    otlp_http:
      sending_queue:
        storage: write_behind_storage
  service:
    extensions: [file_storage, write_behind_storage]
  ```
  `file_storage` stays listed so the service starts it. Validation: `storage` set,
  `flush_after` > 0, `max_buffer_bytes` > 0.
- `Start(ctx, host)`: resolve `host.GetExtensions()[cfg.Storage]`, require
  `storage.Extension`, else fail start with a clear error (the collector refuses to
  start, same as a missing `file_storage` today).
- `GetClient(ctx, kind, id, name)`: `wrapped.GetClient(ctx, kind, id, name)` with the
  arguments unchanged, wrapped in a write-behind client. The extension keeps a set of
  open clients so `Shutdown` can flush and close any the exporter did not close.

### 5.2 Client state (per client, one mutex)

- `entries map[string]*entry` with `entry{value []byte, dirty bool, tombstone bool,
  persisted bool, since time.Time}`.
  - `persisted` = known to exist on disk (flushed by us, or loaded by a `Get` miss).
  - `tombstone` = deleted in memory, delete not yet applied on disk.
- `pendingBytes` = sum of `len(value)` over dirty, non-tombstone entries.
- A background ticker (1 s) and the triggers below. All wrapped calls happen outside
  the entry mutex's critical path only where safe; simplest correct form is to hold the
  mutex for the whole flush (flushes are rare and the queue serialises on its own mutex).

### 5.3 Operations

- `Set(key, v)`: store `v` in memory, `dirty = true`, `tombstone = false`; `since` is set
  only if the entry was not already dirty (so a key rewritten every second still ages).
  A `Set` on an existing tombstone revives the entry (`since` reset).
  Update `pendingBytes`. If `pendingBytes > max_buffer_bytes` → flush now.
- `Delete(key)`: if the entry exists and `persisted == false` → drop it from memory
  (nothing to undo on disk). Otherwise (persisted, or unknown because never seen since
  start) → `tombstone = true, dirty = true, since = now`, value released. **Tombstones
  age exactly like items** (my call, from the builder check): a delete of a persisted
  item that is still unflushed after `flush_after` triggers a flush, so after an outage
  drains the disk forgets delivered batches within the window and a hard kill resends at
  most 15 s worth of already-delivered backlog, never the whole 1440. Without this rule a
  drained backlog could sit on disk for the life of the process. The steady-state path
  (set, send, delete within seconds, never persisted) therefore touches the disk zero times.
- `Get(key)`: entry present → its value (nil for a tombstone). Absent → `wrapped.Get`;
  on a hit cache it with `persisted = true, dirty = false` (metadata and in-flight items
  are read often; values of persisted *item* keys may be dropped again after the read to
  keep memory flat — builder's call, state it in code).
- `Batch(ops…)`: apply in order under the mutex using the three rules above; `Get` misses
  go to `wrapped.Get` one by one (they happen only at start and during an outage).
- Flush (triggers: oldest dirty **item or tombstone** entry older than `flush_after`;
  `pendingBytes` over the cap; `Close`; extension `Shutdown`): build one `wrapped.Batch` with a
  `SetOperation` per dirty non-tombstone entry and a `DeleteOperation` per tombstone,
  **always including the metadata key when it is dirty**, so disk state is a consistent
  snapshot. On success: `dirty = false`, `persisted = true`, tombstones removed, values of
  persisted item keys evicted (metadata stays cached), `pendingBytes` recomputed. On
  failure: keep everything dirty, log at error level once per failure streak, and
  surface the error **only on the offer path**: a `Batch` that contains a `Set` of an
  item key (my call, from the builder check). The queue's read path is
  `Batch(Set metadata, Get item)` and on any error it finishes the item, i.e. deletes it,
  and its delete path only logs — so a sticky error returned from reads or deletes would
  make the queue throw away good batches. Returned on offers it reaches the producer, as
  a failing disk does today. Reads and deletes never carry it. Once a flush succeeds the
  error clears.
  The metadata key alone never triggers a flush (it is rewritten on every queue
  operation); it rides along. An in-flight item (read, not yet deleted) that was never
  persisted is a dirty item entry and ages like any other, so the state "metadata dirty
  + one in-flight unpersisted item" flushes when that item passes the window.
- Locking (my call): hold the client mutex for the whole flush. Flushes are rare (steady
  state has none), carry at most ~4 MiB, and the queue serialises on its own mutex
  anyway; a flush blocks offers and reads for one bbolt transaction on local NVMe. If the
  integration test (6) shows stalls, snapshot under the lock, write outside it, and
  reconcile with a per-entry generation counter (a `Delete` during a flush becomes a
  tombstone); not before.
- `Close`: stop the ticker, flush, `wrapped.Close`. Extension `Shutdown`: `Close` any
  client still open. `Start` fails (and the collector refuses to start) when
  `cfg.Storage` names no extension or one that is not a `storage.Extension`.

### 5.4 Crash semantics

After a hard kill the file holds the last flushed snapshot: metadata and items written
in one transaction, so the read/write indices on disk never point past items that are
there. Items offered after the last flush (younger than 15 s, or fewer than the cap) are
gone; on the next start the queue finishes the missing items (Debug log) and continues. Compaction (`on_start`,
`on_rebound`) and `cleanup_on_start` keep working on the same file.

### 5.5 Observability

One debug log per flush (`keys`, `bytes`, `tombstones`, `reason`), one info log at
`Close` with totals, error log on flush failure. No new metrics (self-telemetry is off
in the distribution).

### 5.6 Config and docs changes

- `distribution/configs/linux/collector-config.yaml`: the block in 5.1, with the comment
  on the queue rewritten to describe the window and the cap (the existing "~two bbolt
  writes per minute" sentence is wrong for the pilot's finding and goes).
- `cmd/monitorable-agent/config_test.go` (`TestShippedConfigsLoad`) covers the new
  extension; `.github/workflows/validate.yml` step that checks the bbolt file still
  passes (the file is still created by `file_storage` at start).
- `CLAUDE.md` "Persistent export queue" bullet and `distribution/README.md` retry/offline
  section: one paragraph each.
- Changelog entry in `monitorable-landing/src/content/changelog/` the day v1.7.0 is on
  prod (user-visible: far fewer disk writes, same outage safety; state the 15 s window
  plainly).

## 6. Testing

- Unit tests with a recording fake `storage.Client`: set→delete within the window
  produces zero wrapped calls; an entry older than the window is flushed together with
  the metadata key in one `Batch`; cap exceeded flushes at once; tombstone for a
  persisted key becomes a `DeleteOperation` and ages out like an item; `Get` miss reads
  through and caches; a failed flush surfaces on the next offer-shaped `Batch` only —
  a read `Batch` and a `Delete` after a failed flush never return it — and clears after
  a successful flush; `Close` flushes everything.
- Shipped-config tests: `TestShippedConfigsLoad` loads the new extension;
  `service.extensions` lists `file_storage` before `write_behind_storage`; `Start`
  fails with the documented error when the wrapped extension is missing.
- A randomised sequence test (thousands of Set/Get/Delete/Batch against a reference map,
  with flushes at random points): every `Get` equals the reference; after a simulated
  crash (drop memory, keep the fake's disk) the disk equals some consistent snapshot
  taken at a flush.
- Integration: real `file_storage` + real `exporterhelper` persistent queue through the
  exporter (the collector test host), offer N requests, consume, assert the bbolt file's
  item count is 0 and the metadata is consistent; kill-and-restart variant asserts
  recovery of items older than the window.
- Acceptance on a real box (staging), a **release gate**: run v1.7.0 for 2 h against
  staging ingest; `/proc/<pid>/io` `write_bytes` growth measured from 5 min after start
  (so `on_start` compaction is excluded) is < 1 MB per hour (v1.6.0: ~4.8 MB per hour,
  the pilot's 115 MB per 24 h; the process writes nothing else — logs go to journald via
  stdout), and
  the queue survives `systemctl restart` with a backend outage simulated by a firewall
  rule (items older than 15 s present after restart).

## 7. Release and sequencing

PR with Opus review → merge → tag `v1.7.0` (release.yml publishes to staging) → staging
acceptance (6) → `publish-prod` dispatch → changelog → **then** the benchmark re-run
(harness part, separate plan). Rollback is the one-line config change to
`sending_queue.storage: file_storage`; the file format is unchanged in both directions.

## 8. Left to the builder

Package layout inside `internal/extension/writebehindstorage`, config struct naming,
whether `Get` of a persisted item key keeps the value cached until the next flush, log
wording, and the fake-client test helper. Everything above that names a number or a
rule is decided.
