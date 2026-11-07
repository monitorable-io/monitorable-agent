// Package writebehindstorage provides the write_behind_storage extension: a
// storage.Extension that wraps another storage extension (file_storage in the
// shipped distribution) and keeps the persistent export queue's writes in
// memory until they are older than flush_after, the pending bytes pass
// max_buffer_bytes, or the client closes. A batch that is sent and deleted
// inside the window never reaches the disk.
//
// Design: docs/superpowers/specs/2026-11-07-write-behind-queue-storage-design.md.
package writebehindstorage // import "github.com/monitorable-io/monitorable-agent/internal/extension/writebehindstorage"

import (
	"errors"
	"time"

	"go.opentelemetry.io/collector/component"
)

// Config is the write_behind_storage configuration.
type Config struct {
	// Storage is the component ID of the wrapped storage extension.
	Storage component.ID `mapstructure:"storage"`
	// FlushAfter is the durability window: a pending write (or delete) older
	// than this is written to the wrapped storage.
	FlushAfter time.Duration `mapstructure:"flush_after"`
	// MaxBufferBytes caps the bytes of pending values held in memory; above it
	// the client flushes at once instead of waiting out the window.
	MaxBufferBytes int64 `mapstructure:"max_buffer_bytes"`
}

// Validate checks the configuration.
func (c *Config) Validate() error {
	var errs []error
	if c.Storage == (component.ID{}) {
		errs = append(errs, errors.New("storage must name the wrapped storage extension"))
	}
	if c.FlushAfter <= 0 {
		errs = append(errs, errors.New("flush_after must be greater than 0"))
	}
	if c.MaxBufferBytes <= 0 {
		errs = append(errs, errors.New("max_buffer_bytes must be greater than 0"))
	}
	return errors.Join(errs...)
}
