//go:build linux

package main

import (
	"context"
	"slices"
	"testing"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/confmap"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/receiver/receivertest"

	"github.com/monitorable-io/monitorable-agent/internal/collectors"
	"github.com/monitorable-io/monitorable-agent/internal/receiver/monitorable"
)

// firstMetric runs a receiver until it emits name (up to 10 s) and returns it.
func firstMetric(t *testing.T, typ string, cfg component.Config, name string) pmetric.Metric {
	t.Helper()
	factory := newFactories().Receivers[component.MustNewType(typ)]
	ctx := context.Background()
	sink := new(consumertest.MetricsSink)
	rcv, err := factory.CreateMetrics(ctx, receivertest.NewNopSettings(factory.Type()), cfg, sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := rcv.Start(ctx, componenttest.NewNopHost()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := rcv.Shutdown(ctx); err != nil {
			t.Errorf("%s shutdown: %v", typ, err)
		}
	}()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		for _, md := range sink.AllMetrics() {
			rms := md.ResourceMetrics()
			for i := 0; i < rms.Len(); i++ {
				sms := rms.At(i).ScopeMetrics()
				for j := 0; j < sms.Len(); j++ {
					ms := sms.At(j).Metrics()
					for k := 0; k < ms.Len(); k++ {
						if ms.At(k).Name() == name {
							out := pmetric.NewMetric()
							ms.At(k).CopyTo(out)
							return out
						}
					}
				}
			}
		}
	}
	t.Fatalf("%s emitted no %s within 10 s", typ, name)
	return pmetric.Metric{}
}

func attrKeys(m pmetric.Metric) [][]string {
	var out [][]string
	dps := m.Sum().DataPoints()
	for i := 0; i < dps.Len(); i++ {
		var keys []string
		for k := range dps.At(i).Attributes().All() {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		out = append(out, keys)
	}
	return out
}

func stateSet(m pmetric.Metric) map[string]bool {
	out := map[string]bool{}
	dps := m.Sum().DataPoints()
	for i := 0; i < dps.Len(); i++ {
		if s, ok := dps.At(i).Attributes().Get("state"); ok {
			out[s.Str()] = true
		}
	}
	return out
}

// TestTCPStateMetricMatchesHostmetrics compares the shape of the monitorable
// receiver's system.network.connections with what the hostmetrics network
// scraper emits on this host, so the backend converts both to the same series.
// Values are not compared: the two reads happen at different moments.
func TestTCPStateMetricMatchesHostmetrics(t *testing.T) {
	const name = "system.network.connections"

	hf := newFactories().Receivers[component.MustNewType("host_metrics")]
	hcfg := hf.CreateDefaultConfig()
	if err := confmap.NewFromStringMap(map[string]any{
		"collection_interval": "1s",
		"scrapers":            map[string]any{"network": map[string]any{}},
	}).Unmarshal(hcfg); err != nil {
		t.Fatalf("host_metrics config: %v", err)
	}
	theirs := firstMetric(t, "host_metrics", hcfg, name)

	mcfg := newFactories().Receivers[component.MustNewType("monitorable")].CreateDefaultConfig().(*monitorable.Config)
	mcfg.Ports.Mode = monitorable.PortsModeOff
	mcfg.TCPStates.Mode = monitorable.TCPStatesModeOn
	mcfg.Docker.Mode = monitorable.DockerModeOff
	mcfg.Systemd.Mode = monitorable.SystemdModeOff
	mcfg.Smart.Mode = monitorable.SmartModeOff
	if err := mcfg.Validate(); err != nil {
		t.Fatal(err)
	}
	ours := firstMetric(t, "monitorable", mcfg, name)

	if ours.Type() != theirs.Type() || ours.Type() != pmetric.MetricTypeSum {
		t.Fatalf("type = %v, hostmetrics %v; want both Sum", ours.Type(), theirs.Type())
	}
	if ours.Sum().IsMonotonic() != theirs.Sum().IsMonotonic() {
		t.Errorf("monotonic = %v, hostmetrics %v", ours.Sum().IsMonotonic(), theirs.Sum().IsMonotonic())
	}
	if ours.Sum().AggregationTemporality() != theirs.Sum().AggregationTemporality() {
		t.Errorf("temporality = %v, hostmetrics %v", ours.Sum().AggregationTemporality(), theirs.Sum().AggregationTemporality())
	}
	if ours.Unit() != theirs.Unit() || ours.Description() != theirs.Description() {
		t.Errorf("unit/description = %q/%q, hostmetrics %q/%q", ours.Unit(), ours.Description(), theirs.Unit(), theirs.Description())
	}
	want := []string{"protocol", "state"}
	for i, keys := range attrKeys(theirs) {
		if !slices.Equal(keys, want) {
			t.Fatalf("hostmetrics datapoint %d attribute keys = %v, want %v (the reference shape changed)", i, keys, want)
		}
	}
	for i, keys := range attrKeys(ours) {
		if !slices.Equal(keys, want) {
			t.Errorf("our datapoint %d attribute keys = %v, want %v", i, keys, want)
		}
	}
	if dps := ours.Sum().DataPoints(); dps.Len() > 0 && dps.At(0).ValueType() != theirs.Sum().DataPoints().At(0).ValueType() {
		t.Errorf("value type = %v, hostmetrics %v", dps.At(0).ValueType(), theirs.Sum().DataPoints().At(0).ValueType())
	}

	ourStates, theirStates := stateSet(ours), stateSet(theirs)
	if len(ourStates) != len(collectors.TCPStateLabels) {
		t.Errorf("our states = %v, want the twelve labels", ourStates)
	}
	for _, label := range collectors.TCPStateLabels {
		if !ourStates[label] {
			t.Errorf("our states lack %s", label)
		}
		// hostmetrics may add FIN_WAIT1/FIN_WAIT2 (spec 5.4); each declared label is there.
		if !theirStates[label] {
			t.Errorf("hostmetrics no longer sends state %s: the label set changed", label)
		}
	}
}
