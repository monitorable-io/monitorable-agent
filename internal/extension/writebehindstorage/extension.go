package writebehindstorage

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/extension/xextension/storage"
	"go.uber.org/zap"
)

// writeBehind is the write_behind_storage extension. It resolves the wrapped
// storage extension at Start and hands out clients that wrap the wrapped
// extension's clients. It declares no dependency on the wrapped extension:
// service.extensions must list the wrapped one first so that it starts before
// and stops after this one.
type writeBehind struct {
	cfg    Config
	logger *zap.Logger

	mu      sync.Mutex
	wrapped storage.Extension
	clients []storage.Client
}

var _ storage.Extension = (*writeBehind)(nil)

func (w *writeBehind) Start(_ context.Context, host component.Host) error {
	ext, ok := host.GetExtensions()[w.cfg.Storage]
	if !ok {
		return fmt.Errorf("write_behind_storage: storage extension %q not found", w.cfg.Storage)
	}
	se, ok := ext.(storage.Extension)
	if !ok {
		return fmt.Errorf("write_behind_storage: extension %q is not a storage extension", w.cfg.Storage)
	}
	w.mu.Lock()
	w.wrapped = se
	w.mu.Unlock()
	return nil
}

// GetClient forwards (kind, id, name) unchanged to the wrapped extension, so
// file_storage derives the same file name it would without the wrapper.
func (w *writeBehind) GetClient(ctx context.Context, kind component.Kind, id component.ID, name string) (storage.Client, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.wrapped == nil {
		return nil, errors.New("write_behind_storage: GetClient called before Start")
	}
	inner, err := w.wrapped.GetClient(ctx, kind, id, name)
	if err != nil {
		return nil, err
	}
	c := newClient(inner, w.cfg, w.logger, time.Now)
	c.start()
	w.clients = append(w.clients, c)
	return c, nil
}

// Shutdown closes every client this extension handed out that its owner did
// not close. Client Close is idempotent, so a client the queue already closed
// is not closed twice.
func (w *writeBehind) Shutdown(ctx context.Context) error {
	w.mu.Lock()
	clients := w.clients
	w.clients = nil
	w.mu.Unlock()
	var errs []error
	for _, c := range clients {
		if err := c.Close(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
