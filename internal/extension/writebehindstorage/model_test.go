package writebehindstorage

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strconv"
	"testing"
	"time"

	"go.opentelemetry.io/collector/extension/xextension/storage"
	"go.uber.org/zap"
)

// modelRun drives a client with random operations against a plain map
// reference. It checks every read against the reference, and after every
// flush that reached the fake disk it checks that the disk equals the
// reference exactly: a flush always writes a complete, consistent snapshot
// taken at a call boundary, never part of one. A hard kill at any moment
// therefore leaves the state as of the last flush.
type modelRun struct {
	t       *testing.T
	rng     *rand.Rand
	c       *client
	fake    *fakeClient
	clk     *fakeClock
	ref     map[string][]byte
	keys    []string
	commits int
	// lastFlushed is the reference as of the last flush that reached disk.
	lastFlushed map[string][]byte
}

func newModelRun(t *testing.T, seed uint64, cfg Config) *modelRun {
	t.Helper()
	m := &modelRun{
		t:    t,
		rng:  rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)),
		fake: newFakeClient(),
		clk:  &fakeClock{t: time.Unix(1_700_000_000, 0)},
		ref:  map[string][]byte{},
		keys: []string{metadataKey},
	}
	for i := range 19 {
		m.keys = append(m.keys, strconv.Itoa(i))
	}
	// Half the keys already exist on disk (a previous run, legacy keys):
	// exercises read-through and tombstones for keys never seen in-process.
	for _, k := range m.keys {
		if m.rng.IntN(2) == 0 {
			v := m.value()
			m.fake.disk[k] = v
			m.ref[k] = clone(v)
		}
	}
	m.lastFlushed = maps.Clone(m.ref)
	m.c = newClientWithTicker(m.fake, cfg, zap.NewNop(), m.clk.now, 0)
	m.c.loadKeys(ctx)
	return m
}

func (m *modelRun) value() []byte {
	b := make([]byte, 1+m.rng.IntN(300))
	for i := range b {
		b[i] = byte(m.rng.Uint32())
	}
	return b
}

func (m *modelRun) key() string { return m.keys[m.rng.IntN(len(m.keys))] }

func (m *modelRun) randomOp() *storage.Operation {
	k := m.key()
	switch m.rng.IntN(3) {
	case 0:
		return storage.GetOperation(k)
	case 1:
		return storage.SetOperation(k, m.value())
	default:
		return storage.DeleteOperation(k)
	}
}

// checkAndApply applies ops to ref in order, checking each Get against the
// state at its position in the batch.
func (m *modelRun) checkAndApply(step int, ops []*storage.Operation, ref map[string][]byte) {
	for _, op := range ops {
		switch op.Type {
		case storage.Get:
			want, ok := ref[op.Key]
			if ok != (op.Value != nil) || !bytes.Equal(want, op.Value) {
				m.t.Fatalf("step %d: Get %s = %d bytes (nil=%v), want %d bytes (present=%v)",
					step, op.Key, len(op.Value), op.Value == nil, len(want), ok)
			}
		case storage.Set:
			ref[op.Key] = clone(op.Value)
		case storage.Delete:
			delete(ref, op.Key)
		}
	}
}

func (m *modelRun) step(i int) {
	// Occasionally the disk fails for a while.
	if m.rng.IntN(100) < 2 {
		if m.fake.err == nil {
			m.fake.setErr(errDisk)
		} else {
			m.fake.setErr(nil)
		}
	}

	var ops []*storage.Operation
	if m.rng.IntN(3) == 0 {
		n := 2 + m.rng.IntN(3)
		for range n {
			ops = append(ops, m.randomOp())
		}
	} else {
		ops = []*storage.Operation{m.randomOp()}
	}
	// Values are copied by the client; keep the originals to compare against.
	saved := make([][]byte, len(ops))
	for j, op := range ops {
		saved[j] = op.Value
	}

	var err error
	if len(ops) == 1 {
		op := ops[0]
		switch op.Type {
		case storage.Get:
			op.Value, err = m.c.Get(ctx, op.Key)
		case storage.Set:
			err = m.c.Set(ctx, op.Key, op.Value)
		case storage.Delete:
			err = m.c.Delete(ctx, op.Key)
		}
	} else {
		err = m.c.Batch(ctx, ops...)
	}

	switch {
	case err == nil:
		m.checkAndApply(i, ops, m.ref)
	case errors.Is(err, errDisk):
		// Only offers are refused, and a refused offer changes nothing.
		if !isOffer(ops) {
			m.t.Fatalf("step %d: non-offer %v failed with %v", i, opsString(ops), err)
		}
		for j, op := range ops {
			if op.Type == storage.Set && !bytes.Equal(op.Value, saved[j]) {
				m.t.Fatalf("step %d: op value mutated", i)
			}
		}
	default:
		m.t.Fatalf("step %d: unexpected error %v", i, err)
	}
	m.checkFlush(i, "call")

	m.clk.advance(time.Duration(m.rng.IntN(20_001)) * time.Millisecond)
	m.c.tick(ctx)
	m.checkFlush(i, "tick")
}

// checkFlush asserts that, if a flush reached disk since the last check, the
// disk equals the reference exactly.
func (m *modelRun) checkFlush(step int, where string) {
	if n := m.fake.commitCount(); n != m.commits {
		m.commits = n
		if !mapsEqual(m.fake.snapshot(), m.ref) {
			m.t.Fatalf("step %d (%s): disk after flush differs from reference\n disk keys %v\n ref keys  %v",
				step, where, sortedKeys(m.fake.snapshot()), sortedKeys(m.ref))
		}
		m.lastFlushed = maps.Clone(m.ref)
	}
	// Between flushes the disk never changes.
	if !mapsEqual(m.fake.snapshot(), m.lastFlushed) {
		m.t.Fatalf("step %d (%s): disk changed without a flush", step, where)
	}
}

func mapsEqual(a, b map[string][]byte) bool {
	return maps.EqualFunc(a, b, func(x, y []byte) bool { return bytes.Equal(x, y) })
}

func sortedKeys(m map[string][]byte) []string { return slices.Sorted(maps.Keys(m)) }

func opsString(ops []*storage.Operation) string {
	s := ""
	for _, op := range ops {
		s += opString(op) + ","
	}
	return s
}

func modelConfig() Config {
	// A cap small enough that cap flushes happen alongside age flushes.
	return Config{FlushAfter: 15 * time.Second, MaxBufferBytes: 1500}
}

func TestRandomOpsMatchReferenceModel(t *testing.T) {
	for seed := range uint64(5) {
		t.Run(fmt.Sprint("seed=", seed), func(t *testing.T) {
			m := newModelRun(t, seed, modelConfig())
			for i := range 5000 {
				m.step(i)
			}
			if m.commits == 0 {
				t.Fatal("no flush ever reached disk; the model exercised nothing")
			}
			// A graceful stop writes everything: disk == reference.
			m.fake.setErr(nil)
			if err := m.c.Close(ctx); err != nil {
				t.Fatalf("Close: %v", err)
			}
			if !mapsEqual(m.fake.snapshot(), m.ref) {
				t.Fatalf("disk after Close differs from reference\n disk %v\n ref  %v",
					sortedKeys(m.fake.snapshot()), sortedKeys(m.ref))
			}
		})
	}
}

func TestCrashLeavesConsistentSnapshot(t *testing.T) {
	for seed := range uint64(5) {
		t.Run(fmt.Sprint("seed=", seed), func(t *testing.T) {
			m := newModelRun(t, 100+seed, modelConfig())
			steps := 1000 + m.rng.IntN(4000)
			for i := range steps {
				m.step(i)
			}
			// Hard kill: drop the client without Close. The disk holds exactly
			// the reference as of the last flush.
			disk := m.fake.snapshot()
			m.c = nil
			if !mapsEqual(disk, m.lastFlushed) {
				t.Fatalf("disk after crash is not the last flushed snapshot")
			}
		})
	}
}
