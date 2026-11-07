package writebehindstorage

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/collector/extension/xextension/storage"
	"go.uber.org/zap"
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
// by hand), its fake wrapped client, and its fake clock.
func newTestClient(t *testing.T, cfg Config) (*client, *fakeClient, *fakeClock) {
	t.Helper()
	fake := newFakeClient()
	clk := &fakeClock{t: time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)}
	c := newClientWithTicker(fake, cfg, zap.NewNop(), clk.now, 0)
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
	c, fake, _ := newTestClient(t, testConfig())
	fake.disk["k"] = []byte("v")

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
	c, fake, _ := newTestClient(t, testConfig())
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
	c, fake, _ := newTestClient(t, testConfig())
	fake.disk["k"] = []byte("old")
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
	c, fake, clk := newTestClient(t, testConfig())
	fake.disk["3"] = []byte("delivered")
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
	c, fake, clk := newTestClient(t, testConfig())
	fake.disk["k"] = []byte("old")
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
	c, fake, clk := newTestClient(t, testConfig())
	fake.disk["wi"] = []byte("5")
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
	c, fake, clk := newTestClient(t, testConfig())
	fake.disk["9"] = []byte("backlog")
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
