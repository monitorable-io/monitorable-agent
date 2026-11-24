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
	"go.opentelemetry.io/collector/config/configtelemetry"
	"go.opentelemetry.io/collector/exporter/otlphttpexporter"
	"go.opentelemetry.io/collector/otelcol/otelcoltest"

	"github.com/monitorable-io/monitorable-agent/internal/extension/writebehindstorage"
	"github.com/monitorable-io/monitorable-agent/internal/receiver/monitorable"
	"github.com/monitorable-io/monitorable-agent/internal/selftelemetry"
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

	// Our own telemetry factory reads the shipped section without leaving
	// anything aside (the v1.7.0 binary needs the same metrics::level: none).
	tc, ok := cfg.Service.Telemetry.(*selftelemetry.Config)
	if !ok {
		t.Fatalf("service.telemetry config is %T, want *selftelemetry.Config", cfg.Service.Telemetry)
	}
	if tc.Metrics.Level != configtelemetry.LevelNone || len(tc.IgnoredKeys()) != 0 {
		t.Errorf("shipped telemetry = %+v, ignored %v; want metrics level none and nothing ignored", tc, tc.IgnoredKeys())
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

// TestStockTelemetryKeysDoNotStopStart: a config written for the stock telemetry
// factory (sampling, readers, traces, resource) must still load — an agent that
// refuses to start after an update is worse than one that ignores a setting.
func TestStockTelemetryKeysDoNotStopStart(t *testing.T) {
	t.Setenv("MONITORABLE_ENDPOINT", "http://127.0.0.1:4318")
	t.Setenv("MONITORABLE_API_KEY", "test-key")
	t.Setenv("MONITORABLE_STATE_DIR", t.TempDir())

	shipped, err := os.ReadFile(filepath.Join("..", "..", "distribution", "configs", "linux", "collector-config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	const block = "  telemetry:\n    metrics:\n      level: none\n"
	stock := "  telemetry:\n" +
		"    logs: {level: debug, encoding: json, sampling: {enabled: false}}\n" +
		"    metrics: {level: detailed, readers: []}\n" +
		"    traces: {level: none}\n" +
		"    resource: {service.name: x}\n"
	if !strings.Contains(string(shipped), block) {
		t.Fatalf("shipped config has no %q block to replace", block)
	}
	path := filepath.Join(t.TempDir(), "collector-config.yaml")
	if err := os.WriteFile(path, []byte(strings.Replace(string(shipped), block, stock, 1)), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := otelcoltest.LoadConfigAndValidate(path, newFactories())
	if err != nil {
		t.Fatalf("config with stock telemetry keys failed to load: %v", err)
	}
	tc, ok := cfg.Service.Telemetry.(*selftelemetry.Config)
	if !ok {
		t.Fatalf("service.telemetry config is %T, want *selftelemetry.Config", cfg.Service.Telemetry)
	}
	want := []string{"logs::sampling", "metrics::readers", "resource", "traces"}
	if got := tc.IgnoredKeys(); !slices.Equal(got, want) {
		t.Errorf("IgnoredKeys = %v, want %v", got, want)
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
