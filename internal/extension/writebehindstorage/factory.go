package writebehindstorage

import (
	"context"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/extension"
)

var componentType = component.MustNewType("write_behind_storage")

const (
	defaultFlushAfter     = 15 * time.Second
	defaultMaxBufferBytes = 4 << 20 // 4 MiB
)

// NewFactory returns the write_behind_storage extension factory.
func NewFactory() extension.Factory {
	return extension.NewFactory(componentType, createDefaultConfig, create, component.StabilityLevelBeta)
}

func createDefaultConfig() component.Config {
	return &Config{
		Storage:        component.MustNewID("file_storage"),
		FlushAfter:     defaultFlushAfter,
		MaxBufferBytes: defaultMaxBufferBytes,
	}
}

func create(_ context.Context, set extension.Settings, cfg component.Config) (extension.Extension, error) {
	return &writeBehind{cfg: *cfg.(*Config), logger: set.Logger}, nil
}
