package main

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
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

	// Write-behind queue storage. Both extensions must be listed; the start/stop
	// order comes from write_behind_storage's Dependencies() (the collector
	// topo-sorts extensions, it does not follow the list order).
	wantExt := []component.ID{component.MustNewID("file_storage"), component.MustNewID("write_behind_storage")}
	for _, id := range wantExt {
		if !slices.Contains(cfg.Service.Extensions, id) {
			t.Errorf("service.extensions = %v, missing %v", cfg.Service.Extensions, id)
		}
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

	// A graceful stop must reach the write-behind flush. The queue's client is
	// closed only after in-flight sends return, and a send to a blackholed
	// backend returns at the HTTP client timeout; systemd SIGKILLs at
	// TimeoutStopSec. So the stop timeout must exceed the client timeout with
	// room for the flush and the rest of shutdown.
	stop := unitTimeoutStopSec(t, filepath.Join("..", "..", "distribution", "configs", "linux", "monitorable-agent.service"))
	if margin := 10 * time.Second; stop < ec.ClientConfig.Timeout+margin {
		t.Errorf("unit TimeoutStopSec = %v, want >= otlp_http timeout %v + %v so a stop with a hung send still flushes the queue",
			stop, ec.ClientConfig.Timeout, margin)
	}
}

func unitTimeoutStopSec(t *testing.T, path string) time.Duration {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "TimeoutStopSec="); ok {
			n, err := strconv.Atoi(v)
			if err != nil {
				t.Fatalf("TimeoutStopSec=%q is not plain seconds: %v", v, err)
			}
			return time.Duration(n) * time.Second
		}
	}
	t.Fatalf("%s has no TimeoutStopSec", path)
	return 0
}
