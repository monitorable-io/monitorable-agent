package main

import (
	"path/filepath"
	"slices"
	"testing"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/exporter/otlphttpexporter"
	"go.opentelemetry.io/collector/otelcol/otelcoltest"

	"github.com/monitorable-io/monitorable-agent/internal/extension/writebehindstorage"
	"github.com/monitorable-io/monitorable-agent/internal/receiver/monitorable"
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
	cfg, err := otelcoltest.LoadConfigAndValidate(path, newFactories())
	if err != nil {
		t.Fatalf("linux config failed to load: %v", err)
	}

	rc, ok := cfg.Receivers[component.MustNewID("monitorable")].(*monitorable.Config)
	if !ok {
		t.Fatal("monitorable receiver config missing or of the wrong type")
	}
	if rc.AddressProbe.Endpoint != "http://127.0.0.1:4318" {
		t.Errorf("address_probe.endpoint = %q, want the exporter endpoint", rc.AddressProbe.Endpoint)
	}
	if string(rc.AddressProbe.APIKey) != "test-key" {
		t.Errorf("address_probe.api_key does not carry the exporter key")
	}

	// Write-behind queue storage. The wrapper declares no dependency on the
	// storage it wraps, so the service.extensions order is what starts
	// file_storage first and stops it last (extensions stop in reverse).
	wantExt := []component.ID{component.MustNewID("file_storage"), component.MustNewID("write_behind_storage")}
	if !slices.Equal(cfg.Service.Extensions, wantExt) {
		t.Errorf("service.extensions = %v, want %v in that order", cfg.Service.Extensions, wantExt)
	}
	ec, ok := cfg.Exporters[component.MustNewID("otlp_http")].(*otlphttpexporter.Config)
	if !ok {
		t.Fatal("otlp_http exporter config missing or of the wrong type")
	}
	q := ec.QueueConfig.Get()
	if q == nil || q.StorageID == nil || *q.StorageID != component.MustNewID("write_behind_storage") {
		t.Errorf("otlp_http sending_queue.storage = %v, want write_behind_storage", q)
	}
	wb, ok := cfg.Extensions[component.MustNewID("write_behind_storage")].(*writebehindstorage.Config)
	if !ok {
		t.Fatal("write_behind_storage extension config missing or of the wrong type")
	}
	if wb.Storage != component.MustNewID("file_storage") || wb.FlushAfter != 15*time.Second || wb.MaxBufferBytes != 4194304 {
		t.Errorf("write_behind_storage = %+v, want storage file_storage, flush_after 15s, max_buffer_bytes 4194304", wb)
	}
}
