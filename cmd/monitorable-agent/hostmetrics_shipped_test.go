package main

import (
	"context"
	"path/filepath"
	"reflect"
	"regexp"
	"testing"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/confmap/confmaptest"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/otelcol/otelcoltest"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/receiver/receivertest"
)

// stored is the set of hostmetrics names the backend's data catalog keeps; it
// drops every other name at ingest.
var stored = map[string]bool{
	"system.cpu.load_average.1m": true, "system.cpu.load_average.5m": true, "system.cpu.load_average.15m": true,
	"system.memory.usage": true, "system.memory.utilization": true, "system.memory.limit": true,
	"system.filesystem.usage": true,
	"system.disk.io":          true, "system.disk.operations": true, "system.disk.io_time": true,
	"system.network.io": true, "system.network.packets": true, "system.network.dropped": true,
	"system.network.errors": true, "system.network.connections": true,
	"system.paging.usage": true,
}

// mustHave is what every Linux host produces. A host with only `lo` or no swap
// emits fewer names than `stored`, so the test asserts a subset plus these.
var mustHave = []string{"system.cpu.load_average.1m", "system.memory.usage", "system.memory.limit", "system.filesystem.usage"}

// backendDeniesDevice is the backend's ingest deny pattern for the `device`
// attribute, copied verbatim.
var backendDeniesDevice = regexp.MustCompile(`^(lo|docker0|veth.+|br-.+|loop\d+|/dev/loop\d+)$`)

func shippedConfigPath() string {
	return filepath.Join("..", "..", "distribution", "configs", "linux", "collector-config.yaml")
}

// TestShippedHostMetricsEmitOnlyStoredSeries runs the shipped host_metrics
// receiver once on this host and fails on any metric name the backend does not
// store or any device the backend drops.
func TestShippedHostMetricsEmitOnlyStoredSeries(t *testing.T) {
	t.Setenv("MONITORABLE_ENDPOINT", "http://127.0.0.1:4318")
	t.Setenv("MONITORABLE_API_KEY", "test-key")
	t.Setenv("MONITORABLE_STATE_DIR", t.TempDir())

	factories := newFactories()
	cfg, err := otelcoltest.LoadConfigAndValidate(shippedConfigPath(), factories)
	if err != nil {
		t.Fatalf("config failed to load: %v", err)
	}
	hmCfg, ok := cfg.Receivers[component.MustNewID("host_metrics")]
	if !ok {
		t.Fatal("shipped config has no host_metrics receiver")
	}
	factory := factories.Receivers[component.MustNewType("host_metrics")]

	ctx := context.Background()
	sink := new(consumertest.MetricsSink)
	rcv, err := factory.CreateMetrics(ctx, receivertest.NewNopSettings(factory.Type()), hmCfg, sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := rcv.Start(ctx, componenttest.NewNopHost()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := rcv.Shutdown(ctx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	})

	seen := map[string]bool{}
	collect := func() {
		for _, md := range sink.AllMetrics() {
			rms := md.ResourceMetrics()
			for i := 0; i < rms.Len(); i++ {
				sms := rms.At(i).ScopeMetrics()
				for j := 0; j < sms.Len(); j++ {
					ms := sms.At(j).Metrics()
					for k := 0; k < ms.Len(); k++ {
						seen[ms.At(k).Name()] = true
					}
				}
			}
		}
	}
	hasAll := func() bool {
		for _, n := range mustHave {
			if !seen[n] {
				return false
			}
		}
		return true
	}
	for deadline := time.Now().Add(10 * time.Second); ; {
		collect()
		if hasAll() || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	for _, md := range sink.AllMetrics() {
		rms := md.ResourceMetrics()
		for i := 0; i < rms.Len(); i++ {
			sms := rms.At(i).ScopeMetrics()
			for j := 0; j < sms.Len(); j++ {
				ms := sms.At(j).Metrics()
				for k := 0; k < ms.Len(); k++ {
					checkStoredMetric(t, ms.At(k))
				}
			}
		}
	}
	for _, n := range mustHave {
		if !seen[n] {
			t.Errorf("%s was not collected within 10 s", n)
		}
	}
}

func checkStoredMetric(t *testing.T, m pmetric.Metric) {
	t.Helper()
	name := m.Name()
	if !stored[name] {
		t.Errorf("%s is collected but the backend does not store it", name)
	}
	var dps pmetric.NumberDataPointSlice
	switch m.Type() {
	case pmetric.MetricTypeGauge:
		dps = m.Gauge().DataPoints()
	case pmetric.MetricTypeSum:
		dps = m.Sum().DataPoints()
	default:
		t.Errorf("%s has type %v, want gauge or sum", name, m.Type())
		return
	}
	for i := 0; i < dps.Len(); i++ {
		if v, ok := dps.At(i).Attributes().Get("device"); ok {
			if dev := v.AsString(); backendDeniesDevice.MatchString(dev) {
				t.Errorf("%s{device=%q} is collected but the backend drops that device", name, dev)
			}
		}
	}
}

// TestShippedConfigExcludesLoopFilesystems pins the filesystem device rule on
// the file: a host with no loop-mounted filesystem never exercises it, and
// the fs-type rule does not catch a loop-mounted ext4 image.
func TestShippedConfigExcludesLoopFilesystems(t *testing.T) {
	conf, err := confmaptest.LoadConf(shippedConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	got := conf.Get("receivers::host_metrics::scrapers::filesystem::exclude_devices::devices")
	if want := []any{`^/dev/loop\d+$`}; !reflect.DeepEqual(got, want) {
		t.Errorf("filesystem exclude_devices.devices = %v, want %v", got, want)
	}
	if mt := conf.Get("receivers::host_metrics::scrapers::filesystem::exclude_devices::match_type"); mt != "regexp" {
		t.Errorf("filesystem exclude_devices.match_type = %v, want regexp", mt)
	}
}
