package writebehindstorage

import (
	"context"
	"strings"
	"sync"

	"go.opentelemetry.io/collector/extension/xextension/storage"
)

// fakeClient is a recording in-memory storage.Client standing in for
// file_storage: disk is the "file", calls records every call in order, and
// err (when set) fails every Batch/Set/Delete without touching disk, like a
// bbolt transaction that does not commit.
type fakeClient struct {
	mu     sync.Mutex
	disk   map[string][]byte
	calls  []string
	err     error
	closed  int
	commits int // successful Batches that changed disk
}

func newFakeClient() *fakeClient { return &fakeClient{disk: map[string][]byte{}} }

func opString(op *storage.Operation) string {
	switch op.Type {
	case storage.Get:
		return "Get " + op.Key
	case storage.Set:
		return "Set " + op.Key
	default:
		return "Delete " + op.Key
	}
}

func (f *fakeClient) Get(_ context.Context, key string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "Get "+key)
	return clone(f.disk[key]), nil
}

func (f *fakeClient) Set(ctx context.Context, key string, value []byte) error {
	return f.Batch(ctx, storage.SetOperation(key, value))
}

func (f *fakeClient) Delete(ctx context.Context, key string) error {
	return f.Batch(ctx, storage.DeleteOperation(key))
}

func (f *fakeClient) Batch(_ context.Context, ops ...*storage.Operation) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	names := make([]string, len(ops))
	for i, op := range ops {
		names[i] = opString(op)
	}
	f.calls = append(f.calls, "Batch["+strings.Join(names, ",")+"]")
	if f.err != nil {
		return f.err
	}
	for _, op := range ops {
		if op.Type != storage.Get {
			f.commits++
			break
		}
	}
	for _, op := range ops {
		switch op.Type {
		case storage.Get:
			op.Value = clone(f.disk[op.Key])
		case storage.Set:
			f.disk[op.Key] = clone(op.Value)
		case storage.Delete:
			delete(f.disk, op.Key)
		}
	}
	return nil
}

// Walk implements storage.Walker like file_storage: fn sees every key and
// value; the ops it returns are applied in the same transaction.
func (f *fakeClient) Walk(_ context.Context, fn storage.WalkFunc) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "Walk")
	if f.err != nil {
		return f.err
	}
	for k, v := range f.disk {
		ops, err := fn(k, v)
		if err != nil {
			return err
		}
		if len(ops) > 0 {
			panic("fake Walk: write ops not supported")
		}
	}
	return nil
}

func (f *fakeClient) Close(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "Close")
	f.closed++
	return nil
}

func (f *fakeClient) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeClient) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeClient) commitCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.commits
}

func (f *fakeClient) resetCalls() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

func (f *fakeClient) snapshot() map[string][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string][]byte, len(f.disk))
	for k, v := range f.disk {
		out[k] = clone(v)
	}
	return out
}
