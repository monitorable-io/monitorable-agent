package main

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/otelcol/otelcoltest"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pipeline"
	"go.opentelemetry.io/collector/processor"
	"go.opentelemetry.io/collector/processor/processortest"

	"github.com/monitorable-io/monitorable-agent/internal/receiver/monitorable"
)

// shippedProcessorChain builds cycle_batch → batch → sink from the shipped
// config, as the metrics pipeline wires them, and starts both. stop shuts them
// down in pipeline order (the collector stops processors from the receiver side).
func shippedProcessorChain(t *testing.T) (sink *consumertest.MetricsSink, head processor.Metrics, stop func()) {
	t.Helper()
	t.Setenv("MONITORABLE_ENDPOINT", "http://127.0.0.1:4318")
	t.Setenv("MONITORABLE_API_KEY", "test-key")
	t.Setenv("MONITORABLE_STATE_DIR", t.TempDir())

	factories := newFactories()
	cfg, err := otelcoltest.LoadConfigAndValidate(shippedConfigPath(), factories)
	if err != nil {
		t.Fatalf("config failed to load: %v", err)
	}
	ctx := context.Background()
	host := componenttest.NewNopHost()
	sink = new(consumertest.MetricsSink)

	bf := factories.Processors[component.MustNewType("batch")]
	batch, err := bf.CreateMetrics(ctx, processortest.NewNopSettings(bf.Type()), cfg.Processors[component.MustNewID("batch")], sink)
	if err != nil {
		t.Fatal(err)
	}
	cf := factories.Processors[component.MustNewType("cycle_batch")]
	cycle, err := cf.CreateMetrics(ctx, processortest.NewNopSettings(cf.Type()), cfg.Processors[component.MustNewID("cycle_batch")], batch)
	if err != nil {
		t.Fatal(err)
	}
	if err := batch.Start(ctx, host); err != nil {
		t.Fatal(err)
	}
	if err := cycle.Start(ctx, host); err != nil {
		t.Fatal(err)
	}
	stopped := false
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		if err := cycle.Shutdown(ctx); err != nil {
			t.Errorf("cycle_batch shutdown: %v", err)
		}
		if err := batch.Shutdown(ctx); err != nil {
			t.Errorf("batch shutdown: %v", err)
		}
	}
	t.Cleanup(stop)
	return sink, cycle, stop
}

func gaugeDatapoints(name string, n int) pmetric.Metrics {
	md := pmetric.NewMetrics()
	m := md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
	m.SetName(name)
	dps := m.SetEmptyGauge().DataPoints()
	for i := 0; i < n; i++ {
		dps.AppendEmpty().SetIntValue(int64(i))
	}
	return md
}

func TestShippedProcessorsSplitOversizedCycle(t *testing.T) {
	sink, head, stop := shippedProcessorChain(t)
	ctx := context.Background()
	if err := head.ConsumeMetrics(ctx, gaugeDatapoints("big", 12000)); err != nil {
		t.Fatal(err)
	}
	if err := head.ConsumeMetrics(ctx, gaugeDatapoints("small", 10)); err != nil {
		t.Fatal(err)
	}
	stop()

	total := 0
	for i, md := range sink.AllMetrics() {
		n := md.DataPointCount()
		total += n
		if n > 5000 {
			t.Errorf("request %d holds %d datapoints, want at most 5000 (the backend refuses above 20,000)", i, n)
		}
	}
	if total != 12010 {
		t.Errorf("datapoints at the sink = %d, want 12010", total)
	}
	if len(sink.AllMetrics()) < 3 {
		t.Errorf("requests = %d, want at least 3 for 12,010 datapoints", len(sink.AllMetrics()))
	}
}

func TestShippedProcessorsSendACycleAtOnce(t *testing.T) {
	sink, head, _ := shippedProcessorChain(t)
	ctx := context.Background()
	if err := head.ConsumeMetrics(ctx, gaugeDatapoints("host_metrics", 100)); err != nil {
		t.Fatal(err)
	}
	if err := head.ConsumeMetrics(ctx, gaugeDatapoints("monitorable", 100)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for len(sink.AllMetrics()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	all := sink.AllMetrics()
	if len(all) != 1 || all[0].DataPointCount() != 200 {
		counts := []int{}
		for _, md := range all {
			counts = append(counts, md.DataPointCount())
		}
		t.Fatalf("within 1 s the sink holds requests of %v datapoints, want one of 200", counts)
	}
}

// TestV180ConfigLoadsAndKeepsTCPStatesOff: hosts that manage the config
// themselves may swap only the binary. The v1.9.0 binary must start on the
// v1.8.0 config and leave the TCP state counts to hostmetrics, so each state
// series is sent once.
func TestV180ConfigLoadsAndKeepsTCPStatesOff(t *testing.T) {
	t.Setenv("MONITORABLE_ENDPOINT", "http://127.0.0.1:4318")
	t.Setenv("MONITORABLE_API_KEY", "test-key")
	t.Setenv("MONITORABLE_STATE_DIR", t.TempDir())

	cfg, err := otelcoltest.LoadConfigAndValidate(filepath.Join("testdata", "collector-config.v1.8.0.yaml"), newFactories())
	if err != nil {
		t.Fatalf("v1.8.0 config failed to load: %v", err)
	}
	rc, ok := cfg.Receivers[component.MustNewID("monitorable")].(*monitorable.Config)
	if !ok {
		t.Fatal("monitorable receiver config missing or of the wrong type")
	}
	if rc.TCPStates.Mode != monitorable.TCPStatesModeOff {
		t.Errorf("tcp_states.mode = %q, want off", rc.TCPStates.Mode)
	}
	want := []component.ID{component.MustNewID("batch"), component.MustNewID("resource")}
	if got := cfg.Service.Pipelines[pipeline.NewID(pipeline.SignalMetrics)].Processors; !slices.Equal(got, want) {
		t.Errorf("v1.8.0 processors = %v, want %v", got, want)
	}
}
