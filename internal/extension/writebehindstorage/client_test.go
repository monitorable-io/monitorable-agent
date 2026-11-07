package writebehindstorage

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/extension/xextension/storage"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// fakeClock is a hand-advanced clock for the client's now func.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func testConfig() Config {
	return Config{FlushAfter: 15 * time.Second, MaxBufferBytes: 4 << 20}
}

// newTestClient returns a client with no background ticker (tests drive tick
// by hand), its fake wrapped client, and its fake clock. disk holds key/value
// pairs present in the wrapped storage before the client starts; the client
// lists them at start (loadKeys) as it does against file_storage.
func newTestClient(t *testing.T, cfg Config, disk ...string) (*client, *fakeClient, *fakeClock) {
	t.Helper()
	fake := newFakeClient()
	for i := 0; i+1 < len(disk); i += 2 {
		fake.disk[disk[i]] = []byte(disk[i+1])
	}
	clk := &fakeClock{t: time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)}
	c := newClientWithTicker(fake, cfg, zap.NewNop(), clk.now, 0)
	c.loadKeys(ctx)
	fake.resetCalls()
	return c, fake, clk
}

var ctx = context.Background()

func mustBatch(t *testing.T, c *client, ops ...*storage.Operation) {
	t.Helper()
	if err := c.Batch(ctx, ops...); err != nil {
		t.Fatalf("Batch: %v", err)
	}
}

func assertNoCalls(t *testing.T, f *fakeClient) {
	t.Helper()
	if calls := f.callLog(); len(calls) != 0 {
		t.Fatalf("wrapped client calls = %v, want none", calls)
	}
}

func TestSteadyStateTouchesDiskNever(t *testing.T) {
	c, fake, _ := newTestClient(t, testConfig())
	item := []byte("batch-1")

	mustBatch(t, c, storage.SetOperation(metadataKey, []byte("m1")), storage.SetOperation("1", item))
	get := storage.GetOperation("1")
	mustBatch(t, c, storage.SetOperation(metadataKey, []byte("m2")), get)
	if !bytes.Equal(get.Value, item) {
		t.Fatalf("read Batch Get = %q, want %q", get.Value, item)
	}
	mustBatch(t, c, storage.SetOperation(metadataKey, []byte("m3")), storage.DeleteOperation("1"))

	assertNoCalls(t, fake)
	if c.pendingBytes != int64(len("m3")) {
		t.Fatalf("pendingBytes = %d, want %d (metadata only)", c.pendingBytes, len("m3"))
	}
	if _, ok := c.entries["1"]; ok {
		t.Fatal("item 1 still in memory after a delete of a never-persisted entry")
	}
}

func TestGetMissReadsThroughAndCaches(t *testing.T) {
	c, fake, _ := newTestClient(t, testConfig(), "k", "v")

	for i := range 2 {
		v, err := c.Get(ctx, "k")
		if err != nil || string(v) != "v" {
			t.Fatalf("Get #%d = %q, %v", i+1, v, err)
		}
	}
	if calls := fake.callLog(); len(calls) != 1 || calls[0] != "Get k" {
		t.Fatalf("wrapped calls = %v, want exactly [Get k]", calls)
	}
	e := c.entries["k"]
	if e == nil || !e.persisted || e.dirty {
		t.Fatalf("entry = %+v, want persisted and clean", e)
	}
}

func TestGetMissOfAbsentKeyReturnsNil(t *testing.T) {
	c, _, _ := newTestClient(t, testConfig())
	v, err := c.Get(ctx, "nope")
	if err != nil || v != nil {
		t.Fatalf("Get absent = %q, %v; want nil, nil", v, err)
	}
}

func TestDeleteOfUnseenKeyIsTombstone(t *testing.T) {
	c, fake, _ := newTestClient(t, testConfig(), "ri", "3")
	if err := c.Delete(ctx, "ri"); err != nil {
		t.Fatal(err)
	}
	e := c.entries["ri"]
	if e == nil || !e.tombstone || !e.dirty {
		t.Fatalf("entry = %+v, want dirty tombstone", e)
	}
	assertNoCalls(t, fake)
}

func TestBatchSetThenGetSameKey(t *testing.T) {
	c, fake, _ := newTestClient(t, testConfig())
	get := storage.GetOperation("k")
	mustBatch(t, c, storage.SetOperation("k", []byte("v")), get)
	if string(get.Value) != "v" {
		t.Fatalf("Get after Set in one Batch = %q, want \"v\"", get.Value)
	}
	assertNoCalls(t, fake)
}

func TestGetAfterDeleteIsNil(t *testing.T) {
	c, fake, _ := newTestClient(t, testConfig())
	if err := c.Set(ctx, "k", []byte("v")); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	v, err := c.Get(ctx, "k")
	if err != nil || v != nil {
		t.Fatalf("Get after Delete = %q, %v; want nil, nil", v, err)
	}
	// The deleted entry never reached the disk, so it is gone from memory
	// (no per-key "known absent" marker: that set would grow without bound);
	// the Get may read through, but nothing may be written.
	assertNoWrites(t, fake)
}

func assertNoWrites(t *testing.T, f *fakeClient) {
	t.Helper()
	for _, call := range f.callLog() {
		if !strings.HasPrefix(call, "Get ") {
			t.Fatalf("wrapped client calls = %v, want reads only", f.callLog())
		}
	}
}

func TestGetOfTombstoneIsNilWithoutReadThrough(t *testing.T) {
	c, fake, _ := newTestClient(t, testConfig(), "k", "old")
	if err := c.Delete(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	v, err := c.Get(ctx, "k")
	if err != nil || v != nil {
		t.Fatalf("Get of tombstone = %q, %v; want nil, nil (disk still holds the old value)", v, err)
	}
	assertNoCalls(t, fake)
}

func TestSetCopiesCallerBuffer(t *testing.T) {
	c, _, _ := newTestClient(t, testConfig())
	buf := []byte("abc")
	if err := c.Set(ctx, "k", buf); err != nil {
		t.Fatal(err)
	}
	buf[0] = 'X'
	v, _ := c.Get(ctx, "k")
	if string(v) != "abc" {
		t.Fatalf("Get = %q after caller reused its buffer, want \"abc\"", v)
	}
}

func TestSinceSetOnlyWhenEntryBecomesDirty(t *testing.T) {
	c, _, clk := newTestClient(t, testConfig())
	t0 := clk.now()
	if err := c.Set(ctx, "1", []byte("a")); err != nil {
		t.Fatal(err)
	}
	clk.advance(5 * time.Second)
	if err := c.Set(ctx, "1", []byte("b")); err != nil {
		t.Fatal(err)
	}
	if got := c.entries["1"].since; !got.Equal(t0) {
		t.Fatalf("since = %v after a rewrite of a dirty entry, want first-dirty time %v", got, t0)
	}
	if c.pendingBytes != 1 {
		t.Fatalf("pendingBytes = %d after rewrite, want 1", c.pendingBytes)
	}
}

// --- Task 3: flush ---

const window = 15 * time.Second

func assertCalls(t *testing.T, f *fakeClient, want ...string) {
	t.Helper()
	got := f.callLog()
	if strings.Join(got, " | ") != strings.Join(want, " | ") {
		t.Fatalf("wrapped calls = %v, want %v", got, want)
	}
}

func TestItemOlderThanWindowFlushesWithMetadata(t *testing.T) {
	c, fake, clk := newTestClient(t, testConfig())
	mustBatch(t, c, storage.SetOperation(metadataKey, []byte("meta")), storage.SetOperation("7", []byte("item")))

	clk.advance(window - time.Millisecond)
	c.tick(ctx)
	assertNoCalls(t, fake)

	clk.advance(2 * time.Millisecond)
	c.tick(ctx)
	assertCalls(t, fake, "Batch[Set qmv0,Set 7]")

	if _, ok := c.entries["7"]; ok {
		t.Fatal("persisted item still held in memory, want evicted")
	}
	m := c.entries[metadataKey]
	if m == nil || string(m.value) != "meta" || m.dirty || !m.persisted {
		t.Fatalf("metadata entry = %+v, want cached clean persisted value", m)
	}
	if c.pendingBytes != 0 {
		t.Fatalf("pendingBytes = %d, want 0", c.pendingBytes)
	}
	if string(fake.disk["7"]) != "item" || string(fake.disk[metadataKey]) != "meta" {
		t.Fatalf("disk = %v", fake.disk)
	}

	// The evicted item reads back through the wrapped client.
	fake.resetCalls()
	v, err := c.Get(ctx, "7")
	if err != nil || string(v) != "item" {
		t.Fatalf("Get evicted item = %q, %v", v, err)
	}
	assertCalls(t, fake, "Get 7")
}

func TestMetadataAloneNeverFlushes(t *testing.T) {
	c, fake, clk := newTestClient(t, testConfig())
	if err := c.Set(ctx, metadataKey, []byte("meta")); err != nil {
		t.Fatal(err)
	}
	clk.advance(time.Hour)
	c.tick(ctx)
	assertNoCalls(t, fake)
}

func TestCapFlushesAtOnce(t *testing.T) {
	cfg := testConfig()
	cfg.MaxBufferBytes = 100
	c, fake, _ := newTestClient(t, cfg)
	if err := c.Set(ctx, "1", bytes.Repeat([]byte("a"), 60)); err != nil {
		t.Fatal(err)
	}
	assertNoCalls(t, fake)
	if err := c.Set(ctx, "2", bytes.Repeat([]byte("b"), 60)); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, fake, "Batch[Set 1,Set 2]")
	if c.pendingBytes != 0 {
		t.Fatalf("pendingBytes = %d, want 0", c.pendingBytes)
	}
}

// The cap is checked once per call, after the whole Batch: a flush between
// the metadata Set and the item Set of one offer would put metadata on disk
// that points at an item that is not there.
func TestCapCheckedAtBatchBoundary(t *testing.T) {
	cfg := testConfig()
	cfg.MaxBufferBytes = 10
	c, fake, _ := newTestClient(t, cfg)
	mustBatch(t, c,
		storage.SetOperation(metadataKey, bytes.Repeat([]byte("m"), 20)),
		storage.SetOperation("1", bytes.Repeat([]byte("i"), 20)))
	assertCalls(t, fake, "Batch[Set qmv0,Set 1]")
}

func TestTombstoneAgesOut(t *testing.T) {
	c, fake, clk := newTestClient(t, testConfig(), "3", "delivered")
	mustBatch(t, c, storage.SetOperation(metadataKey, []byte("meta")), storage.DeleteOperation("3"))

	clk.advance(window)
	c.tick(ctx)
	assertCalls(t, fake, "Batch[Set qmv0,Delete 3]")
	if _, ok := c.entries["3"]; ok {
		t.Fatal("tombstone still in memory after flush")
	}
	if _, ok := fake.disk["3"]; ok {
		t.Fatal("item 3 still on disk after its tombstone flushed")
	}
}

func TestReviveAfterTombstone(t *testing.T) {
	c, fake, clk := newTestClient(t, testConfig(), "k", "old")
	if err := c.Delete(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	clk.advance(10 * time.Second)
	if err := c.Set(ctx, "k", []byte("new")); err != nil {
		t.Fatal(err)
	}
	// The revive resets since: 10 s after the delete is only 5 s after the
	// revive, so 10 s more is not yet a full window for the new value.
	clk.advance(6 * time.Second)
	c.tick(ctx)
	assertNoCalls(t, fake)

	clk.advance(10 * time.Second)
	c.tick(ctx)
	assertCalls(t, fake, "Batch[Set k]")
	if string(fake.disk["k"]) != "new" {
		t.Fatalf("disk k = %q, want \"new\"", fake.disk["k"])
	}
	// A revived key is still on disk: deleting it again must tombstone it.
	if err := c.Delete(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if e := c.entries["k"]; e == nil || !e.tombstone {
		t.Fatalf("entry after re-delete = %+v, want tombstone", e)
	}
}

func TestPendingBytesTracksDeletesAndEviction(t *testing.T) {
	c, _, clk := newTestClient(t, testConfig())
	for _, k := range []string{"1", "2", "3"} {
		if err := c.Set(ctx, k, bytes.Repeat([]byte("x"), 100)); err != nil {
			t.Fatal(err)
		}
	}
	if c.pendingBytes != 300 {
		t.Fatalf("pendingBytes = %d, want 300", c.pendingBytes)
	}
	if err := c.Delete(ctx, "2"); err != nil {
		t.Fatal(err)
	}
	if c.pendingBytes != 200 {
		t.Fatalf("pendingBytes after delete = %d, want 200", c.pendingBytes)
	}
	clk.advance(window)
	c.tick(ctx)
	if c.pendingBytes != 0 {
		t.Fatalf("pendingBytes after flush = %d, want 0", c.pendingBytes)
	}
	if err := c.Set(ctx, "4", bytes.Repeat([]byte("y"), 50)); err != nil {
		t.Fatal(err)
	}
	if c.pendingBytes != 50 {
		t.Fatalf("pendingBytes = %d, want 50", c.pendingBytes)
	}
	// Deleting a persisted, dirty entry (rewritten after its flush) releases its bytes too.
	if err := c.Set(ctx, "1", bytes.Repeat([]byte("z"), 10)); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, "1"); err != nil {
		t.Fatal(err)
	}
	if c.pendingBytes != 50 {
		t.Fatalf("pendingBytes = %d after deleting a rewritten persisted key, want 50", c.pendingBytes)
	}
}

func TestGetOfUnseenThenDeleteReachesDisk(t *testing.T) {
	c, fake, clk := newTestClient(t, testConfig(), "wi", "5")
	v, err := c.Get(ctx, "wi")
	if err != nil || string(v) != "5" {
		t.Fatalf("Get wi = %q, %v", v, err)
	}
	if err := c.Delete(ctx, "wi"); err != nil {
		t.Fatal(err)
	}
	fake.resetCalls()
	clk.advance(window)
	c.tick(ctx)
	assertCalls(t, fake, "Batch[Delete wi]")
	if _, ok := fake.disk["wi"]; ok {
		t.Fatal("wi still on disk")
	}
}

// A cached read-through value of an item key is dropped at the next flush,
// so memory stays flat while an outage backlog drains.
func TestReadThroughItemEvictedAtFlush(t *testing.T) {
	c, _, clk := newTestClient(t, testConfig(), "9", "backlog")
	if _, err := c.Get(ctx, "9"); err != nil {
		t.Fatal(err)
	}
	if err := c.Set(ctx, "10", []byte("new")); err != nil {
		t.Fatal(err)
	}
	clk.advance(window)
	c.tick(ctx)
	if _, ok := c.entries["9"]; ok {
		t.Fatal("read-through item 9 still cached after a flush")
	}
}

// --- Task 4: close and failure semantics ---

var errDisk = errors.New("disk on fire")

func TestCloseFlushesEverything(t *testing.T) {
	c, fake, _ := newTestClient(t, testConfig(), "1", "delivered")
	mustBatch(t, c,
		storage.SetOperation(metadataKey, []byte("meta")),
		storage.SetOperation("2", []byte("pending")),
		storage.DeleteOperation("1"))
	if err := c.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	assertCalls(t, fake, "Batch[Set qmv0,Delete 1,Set 2]", "Close")
	if string(fake.disk["2"]) != "pending" || fake.disk["1"] != nil {
		t.Fatalf("disk after Close = %v", fake.disk)
	}
}

func TestCloseTwiceIsSafe(t *testing.T) {
	c, fake, _ := newTestClient(t, testConfig())
	if err := c.Set(ctx, "1", []byte("x")); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		if err := c.Close(ctx); err != nil {
			t.Fatalf("Close #%d: %v", i+1, err)
		}
	}
	assertCalls(t, fake, "Batch[Set 1]", "Close")
}

func TestOperationsAfterCloseFail(t *testing.T) {
	c, _, _ := newTestClient(t, testConfig())
	if err := c.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.Set(ctx, "1", []byte("x")); !errors.Is(err, errClosed) {
		t.Fatalf("Set after Close = %v, want errClosed", err)
	}
}

func TestCloseReportsFailedFlushAndStillClosesWrapped(t *testing.T) {
	c, fake, _ := newTestClient(t, testConfig())
	if err := c.Set(ctx, "1", []byte("x")); err != nil {
		t.Fatal(err)
	}
	fake.setErr(errDisk)
	if err := c.Close(ctx); !errors.Is(err, errDisk) {
		t.Fatalf("Close with failing flush = %v, want errDisk", err)
	}
	if fake.closed != 1 {
		t.Fatalf("wrapped closed %d times, want 1", fake.closed)
	}
}

// ageAndFailFlush sets an item, ages it past the window and runs one tick
// against a failing disk.
func ageAndFailFlush(t *testing.T, c *client, fake *fakeClient, clk *fakeClock) {
	t.Helper()
	mustBatch(t, c, storage.SetOperation(metadataKey, []byte("m1")), storage.SetOperation("1", []byte("item1")))
	fake.setErr(errDisk)
	clk.advance(window)
	c.tick(ctx)
	if len(fake.callLog()) != 1 {
		t.Fatalf("expected one failed flush attempt, calls = %v", fake.callLog())
	}
}

func TestFlushErrorSurfacesOnOfferOnly(t *testing.T) {
	c, fake, clk := newTestClient(t, testConfig())
	ageAndFailFlush(t, c, fake, clk)

	// Read path: Batch(Set metadata, Get item) must not see the error, or the
	// queue would finish (delete) a good batch.
	get := storage.GetOperation("1")
	if err := c.Batch(ctx, storage.SetOperation(metadataKey, []byte("m2")), get); err != nil {
		t.Fatalf("read Batch after failed flush = %v, want nil", err)
	}
	if string(get.Value) != "item1" {
		t.Fatalf("read Batch Get = %q, want item1", get.Value)
	}
	if _, err := c.Get(ctx, metadataKey); err != nil {
		t.Fatalf("Get after failed flush = %v, want nil", err)
	}
	// Delete path: only logged by the queue; must not see it either.
	if err := c.Delete(ctx, "1"); err != nil {
		t.Fatalf("Delete after failed flush = %v, want nil", err)
	}
	if err := c.Batch(ctx, storage.SetOperation(metadataKey, []byte("m3")), storage.DeleteOperation("1")); err != nil {
		t.Fatalf("finish Batch after failed flush = %v, want nil", err)
	}

	// Offer path: Batch(Set metadata, Set item) gets the error and stores nothing.
	if err := c.Batch(ctx, storage.SetOperation(metadataKey, []byte("m4")), storage.SetOperation("2", []byte("item2"))); !errors.Is(err, errDisk) {
		t.Fatalf("offer Batch after failed flush = %v, want errDisk", err)
	}
	if _, ok := c.entries["2"]; ok {
		t.Fatal("rejected offer was buffered anyway; memory would grow while the disk fails")
	}
	if got := string(c.entries[metadataKey].value); got != "m3" {
		t.Fatalf("metadata = %q after a rejected offer, want m3 (untouched)", got)
	}
	if err := c.Set(ctx, "2", []byte("item2")); !errors.Is(err, errDisk) {
		t.Fatalf("single Set of an item after failed flush = %v, want errDisk", err)
	}
	if err := c.Set(ctx, metadataKey, []byte("m5")); err != nil {
		t.Fatalf("Set of metadata alone after failed flush = %v, want nil", err)
	}

	// Disk recovers: the next due tick flushes and the error clears.
	fake.setErr(nil)
	fake.resetCalls()
	c.tick(ctx)
	if len(fake.callLog()) != 1 {
		t.Fatalf("recovery tick calls = %v, want one Batch", fake.callLog())
	}
	if err := c.Batch(ctx, storage.SetOperation(metadataKey, []byte("m6")), storage.SetOperation("3", []byte("item3"))); err != nil {
		t.Fatalf("offer after successful flush = %v, want nil", err)
	}
}

func TestFlushErrorKeepsStateForRetry(t *testing.T) {
	c, fake, clk := newTestClient(t, testConfig())
	ageAndFailFlush(t, c, fake, clk)
	before := c.pendingBytes
	if before != int64(len("m1")+len("item1")) {
		t.Fatalf("pendingBytes = %d", before)
	}
	for _, k := range []string{metadataKey, "1"} {
		if e := c.entries[k]; e == nil || !e.dirty {
			t.Fatalf("entry %s = %+v after failed flush, want still dirty", k, e)
		}
	}
	if len(fake.disk) != 0 {
		t.Fatalf("disk = %v after failed flush, want empty", fake.disk)
	}
	// It stays due, so every tick retries.
	c.tick(ctx)
	if n := len(fake.callLog()); n != 2 {
		t.Fatalf("calls after second tick = %d, want 2 (retry)", n)
	}
}

func TestFlushErrorLoggedOncePerStreak(t *testing.T) {
	core, logs := observer.New(zap.ErrorLevel)
	fake := newFakeClient()
	clk := &fakeClock{t: time.Unix(0, 0)}
	c := newClientWithTicker(fake, testConfig(), zap.New(core), clk.now, 0)

	ageAndFailFlush(t, c, fake, clk)
	c.tick(ctx)
	c.tick(ctx)
	if n := logs.Len(); n != 1 {
		t.Fatalf("error logs after a 3-attempt failure streak = %d, want 1", n)
	}
	fake.setErr(nil)
	c.tick(ctx)
	mustBatch(t, c, storage.SetOperation("2", []byte("x")))
	fake.setErr(errDisk)
	clk.advance(window)
	c.tick(ctx)
	if n := logs.Len(); n != 2 {
		t.Fatalf("error logs after a second streak = %d, want 2", n)
	}
}

// A cap-triggered flush that fails leaves the offer that triggered it
// buffered (it was applied) and returns nil for it; the next offer fails.
func TestCapFlushFailureFailsNextOffer(t *testing.T) {
	cfg := testConfig()
	cfg.MaxBufferBytes = 10
	c, fake, _ := newTestClient(t, cfg)
	fake.setErr(errDisk)
	if err := c.Set(ctx, "1", bytes.Repeat([]byte("a"), 20)); err != nil {
		t.Fatalf("offer that tripped the cap = %v, want nil (it is buffered)", err)
	}
	if _, ok := c.entries["1"]; !ok {
		t.Fatal("offer that tripped the cap is not buffered")
	}
	if err := c.Set(ctx, "2", []byte("b")); !errors.Is(err, errDisk) {
		t.Fatalf("next offer = %v, want errDisk", err)
	}
}

func TestShutdownAfterQueueCloseClosesWrappedOnce(t *testing.T) {
	fs := &fakeStorageExt{}
	ext := newExt(t, NewFactory().CreateDefaultConfig().(*Config))
	if err := ext.Start(ctx, hostWith{component.MustNewID("file_storage"): fs}); err != nil {
		t.Fatal(err)
	}
	cl, err := ext.GetClient(ctx, component.KindExporter, component.MustNewID("otlp_http"), "metrics")
	if err != nil {
		t.Fatal(err)
	}
	if err := cl.Set(ctx, "1", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := cl.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := ext.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	fake := fs.client.(*fakeClient)
	assertCalls(t, fake, "Walk", "Batch[Set 1]", "Close")
}

func TestShutdownClosesUnclosedClients(t *testing.T) {
	fs := &fakeStorageExt{}
	ext := newExt(t, NewFactory().CreateDefaultConfig().(*Config))
	if err := ext.Start(ctx, hostWith{component.MustNewID("file_storage"): fs}); err != nil {
		t.Fatal(err)
	}
	cl, err := ext.GetClient(ctx, component.KindExporter, component.MustNewID("otlp_http"), "metrics")
	if err != nil {
		t.Fatal(err)
	}
	if err := cl.Set(ctx, "1", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := ext.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, fs.client.(*fakeClient), "Walk", "Batch[Set 1]", "Close")
}

// A failure streak whose backlog drained (only metadata left dirty) must
// still retry and clear, or offers would be refused forever.
func TestFailureStreakRetriesWithOnlyMetadataDirty(t *testing.T) {
	c, fake, clk := newTestClient(t, testConfig())
	ageAndFailFlush(t, c, fake, clk)
	mustBatch(t, c, storage.SetOperation(metadataKey, []byte("m2")), storage.DeleteOperation("1"))
	fake.setErr(nil)
	fake.resetCalls()
	c.tick(ctx)
	assertCalls(t, fake, "Batch[Set qmv0]")
	if err := c.Set(ctx, "2", []byte("x")); err != nil {
		t.Fatalf("offer after recovery = %v, want nil", err)
	}
}

// With nothing dirty at all, the next tick clears the error.
func TestFailureStreakClearsWhenNothingPending(t *testing.T) {
	c, fake, clk := newTestClient(t, testConfig())
	ageAndFailFlush(t, c, fake, clk)
	c.flushErr = errDisk
	c.entries = map[string]*entry{}
	c.pendingBytes = 0
	c.tick(ctx)
	if c.flushErr != nil {
		t.Fatalf("flushErr = %v with nothing pending, want cleared", c.flushErr)
	}
}

// --- exact on-disk key knowledge (Task 5 model finding) ---

func TestDeleteOfKeyAbsentFromDiskLeavesNothing(t *testing.T) {
	c, fake, clk := newTestClient(t, testConfig())
	if err := c.Delete(ctx, "ri"); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.entries["ri"]; ok {
		t.Fatal("delete of a key that is on no disk left an entry")
	}
	clk.advance(window)
	c.tick(ctx)
	assertNoCalls(t, fake)
}

func TestGetOfKeyAbsentFromDiskDoesNotReadThrough(t *testing.T) {
	c, fake, _ := newTestClient(t, testConfig())
	v, err := c.Get(ctx, "5")
	if err != nil || v != nil {
		t.Fatalf("Get = %q, %v; want nil, nil", v, err)
	}
	// Every file_storage Get is a bbolt Update transaction (a meta page write
	// and an fsync), so a miss must not reach it.
	assertNoCalls(t, fake)
}

// A key that is on disk but was never seen in this process, Set and then
// deleted: the delete must reach the disk, or the old value resurfaces.
func TestSetOfUnseenDiskKeyThenDeleteReachesDisk(t *testing.T) {
	c, fake, clk := newTestClient(t, testConfig(), "4", "old")
	if err := c.Set(ctx, "4", []byte("new")); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, "4"); err != nil {
		t.Fatal(err)
	}
	if v, _ := c.Get(ctx, "4"); v != nil {
		t.Fatalf("Get after delete = %q, want nil", v)
	}
	clk.advance(window)
	c.tick(ctx)
	if _, ok := fake.disk["4"]; ok {
		t.Fatal("old value of 4 still on disk")
	}
}

// A key flushed (and evicted) earlier in this process, Set again and then
// deleted: same rule.
func TestFlushedKeyRewrittenThenDeletedReachesDisk(t *testing.T) {
	c, fake, clk := newTestClient(t, testConfig())
	if err := c.Set(ctx, "4", []byte("v1")); err != nil {
		t.Fatal(err)
	}
	clk.advance(window)
	c.tick(ctx)
	if err := c.Set(ctx, "4", []byte("v2")); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, "4"); err != nil {
		t.Fatal(err)
	}
	if v, _ := c.Get(ctx, "4"); v != nil {
		t.Fatalf("Get after delete = %q, want nil", v)
	}
	clk.advance(window)
	c.tick(ctx)
	if _, ok := fake.disk["4"]; ok {
		t.Fatal("flushed value of 4 still on disk")
	}
}

// noWalker hides the fake's Walk: storage that cannot list its keys.
type noWalker struct{ storage.Client }

// Without a Walker the client cannot know what is on disk: a Get of an
// unseen key reads through and a Delete of an unseen key is a tombstone.
func TestWithoutWalkerUnseenKeysMayBeOnDisk(t *testing.T) {
	fake := newFakeClient()
	fake.disk["k"] = []byte("v")
	clk := &fakeClock{t: time.Unix(0, 0)}
	c := newClientWithTicker(noWalker{fake}, testConfig(), zap.NewNop(), clk.now, 0)
	c.loadKeys(ctx)
	if v, err := c.Get(ctx, "k"); err != nil || string(v) != "v" {
		t.Fatalf("Get = %q, %v", v, err)
	}
	if err := c.Delete(ctx, "other"); err != nil {
		t.Fatal(err)
	}
	if e := c.entries["other"]; e == nil || !e.tombstone {
		t.Fatalf("entry = %+v, want tombstone", e)
	}
}
