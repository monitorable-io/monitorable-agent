package main

import (
	"path/filepath"
	"testing"

	"go.opentelemetry.io/collector/otelcol/otelcoltest"
)

// TestShippedConfigsLoad loads the published distribution configs through the
// real factories and validates them. It fails when a config references a
// component main.go does not register (e.g. a storage extension) or sets an
// invalid value — the same failure CI's smoke test would surface only after a
// full build and an 8 s wait.
func TestShippedConfigsLoad(t *testing.T) {
	t.Setenv("MONITORABLE_ENDPOINT", "http://127.0.0.1:4318")
	t.Setenv("MONITORABLE_API_KEY", "test-key")
	t.Setenv("MONITORABLE_STATE_DIR", t.TempDir())

	path := filepath.Join("..", "..", "distribution", "configs", "linux", "collector-config.yaml")
	if _, err := otelcoltest.LoadConfigAndValidate(path, newFactories()); err != nil {
		t.Fatalf("linux config failed to load: %v", err)
	}
}
