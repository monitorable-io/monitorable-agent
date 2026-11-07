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
