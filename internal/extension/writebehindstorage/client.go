package writebehindstorage

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.opentelemetry.io/collector/extension/xextension/storage"
	"go.uber.org/zap"
)

// metadataKey is the persistent queue's metadata record (exporterhelper
// internal/queue: metadataKey). It is rewritten on every queue operation, so
// it never triggers a flush on its own; it rides along with the next one.
const metadataKey = "qmv0"

var errClosed = errors.New("write_behind_storage: client is closed")

// entry is the in-memory state of one key.
//
// A key absent from client.entries is "unknown": it may or may not exist in
// the wrapped storage, so a Get reads through and a Delete records a
// tombstone. Item values are dropped from memory (the entry removed) once they
// are persisted, which returns the key to that state.
type entry struct {
	value     []byte
	dirty     bool      // memory differs from the wrapped storage
	tombstone bool      // deleted in memory; the delete is not yet on disk
	persisted bool      // known to exist in the wrapped storage
	since     time.Time // when the entry last became dirty
}

// client is the write-behind storage.Client handed to the persistent queue.
// One mutex guards all state and is held across flushes: flushes are rare,
// and the queue serialises its own calls under its own mutex anyway.
type client struct {
	mu      sync.Mutex
	wrapped storage.Client
	cfg     Config
	log     *zap.Logger
	now     func() time.Time

	entries      map[string]*entry
	pendingBytes int64 // len(value) summed over dirty, non-tombstone entries
	flushErr     error // last flush failure; cleared by a successful flush
	closed       bool
	stop         chan struct{}
	tickEvery    time.Duration
}

var _ storage.Client = (*client)(nil)

func newClient(wrapped storage.Client, cfg Config, logger *zap.Logger, now func() time.Time) *client {
	return newClientWithTicker(wrapped, cfg, logger, now, time.Second)
}

// newClientWithTicker builds a client whose background flush check runs every
// tickEvery; 0 starts no goroutine (unit tests call tick by hand).
func newClientWithTicker(wrapped storage.Client, cfg Config, logger *zap.Logger, now func() time.Time, tickEvery time.Duration) *client {
	return &client{
		wrapped:   wrapped,
		cfg:       cfg,
		log:       logger,
		now:       now,
		entries:   map[string]*entry{},
		stop:      make(chan struct{}),
		tickEvery: tickEvery,
	}
}

// start launches the background flush check (Task 3); a no-op when
// tickEvery is 0.
func (c *client) start() {}

func (c *client) Get(ctx context.Context, key string) ([]byte, error) {
	op := storage.GetOperation(key)
	if err := c.Batch(ctx, op); err != nil {
		return nil, err
	}
	return op.Value, nil
}

func (c *client) Set(ctx context.Context, key string, value []byte) error {
	return c.Batch(ctx, storage.SetOperation(key, value))
}

func (c *client) Delete(ctx context.Context, key string) error {
	return c.Batch(ctx, storage.DeleteOperation(key))
}

// Batch applies ops in order against memory.
func (c *client) Batch(ctx context.Context, ops ...*storage.Operation) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errClosed
	}
	for _, op := range ops {
		if err := c.applyLocked(ctx, op); err != nil {
			return err
		}
	}
	return nil
}

// applyLocked applies one operation to memory; c.mu is held.
func (c *client) applyLocked(ctx context.Context, op *storage.Operation) error {
	switch op.Type {
	case storage.Get:
		return c.getLocked(ctx, op)
	case storage.Set:
		c.setLocked(op.Key, op.Value)
		return nil
	case storage.Delete:
		c.deleteLocked(op.Key)
		return nil
	default:
		return errors.New("write_behind_storage: unknown operation type")
	}
}

func (c *client) getLocked(ctx context.Context, op *storage.Operation) error {
	if e, ok := c.entries[op.Key]; ok {
		if e.tombstone {
			op.Value = nil
		} else {
			op.Value = clone(e.value)
		}
		return nil
	}
	// Unknown key: read through. Only start-up recovery and outage drains get
	// here. A hit is cached clean and persisted; a persisted item's value is
	// dropped again at the next flush (see flushLocked), so this cache never
	// outlives one window.
	v, err := c.wrapped.Get(ctx, op.Key)
	if err != nil {
		return err
	}
	if v != nil {
		c.entries[op.Key] = &entry{value: clone(v), persisted: true}
	}
	op.Value = v
	return nil
}

func (c *client) setLocked(key string, value []byte) {
	e, ok := c.entries[key]
	if !ok {
		e = &entry{}
		c.entries[key] = e
	}
	if e.dirty && !e.tombstone {
		c.pendingBytes -= int64(len(e.value))
	} else {
		// Becomes dirty now (clean, new, or a revived tombstone).
		e.since = c.now()
	}
	e.value = clone(value)
	e.dirty = true
	e.tombstone = false
	c.pendingBytes += int64(len(e.value))
}

func (c *client) deleteLocked(key string) {
	e, ok := c.entries[key]
	if ok && e.dirty && !e.tombstone {
		c.pendingBytes -= int64(len(e.value))
	}
	if ok && !e.persisted {
		// Never reached the disk: nothing to undo there. A Set of a key this
		// client has not seen creates a non-persisted entry: the queue only
		// sets fresh item indices and the metadata key, and every snapshot on
		// disk is consistent (metadata and items in one transaction), so a
		// fresh index is never already on disk.
		delete(c.entries, key)
		return
	}
	if !ok {
		e = &entry{persisted: true} // unknown: assume it may be on disk
		c.entries[key] = e
	}
	if !e.tombstone {
		e.since = c.now()
	}
	e.value = nil
	e.dirty = true
	e.tombstone = true
}

// Close is completed in a later task; for now it closes the wrapped client.
func (c *client) Close(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	return c.wrapped.Close(ctx)
}

func clone(b []byte) []byte {
	if b == nil {
		return nil
	}
	return append([]byte{}, b...)
}
