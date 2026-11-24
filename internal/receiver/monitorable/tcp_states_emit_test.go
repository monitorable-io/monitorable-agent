package monitorable

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/receiver/receivertest"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/monitorable-io/monitorable-agent/internal/collectors"
)

// newModesTestReceiver is newPortsTestReceiver with both modes set, built the
// way newMetricsReceiver builds it (a collector exists when either mode is on).
func newModesTestReceiver(root, portsMode, statesMode string, logger *zap.Logger) *metricsReceiver {
	r := &metricsReceiver{
		portsMode:     portsMode,
		portsPrev:     collectors.PortsStatus(-1),
		tcpStatesMode: statesMode,
		tcpStatesPrev: collectors.PortsStatus(-1),
		cfg: &Config{
			Ports:     PortsConfig{Mode: portsMode, SampleInterval: 10 * time.Second, MaxPorts: 30},
			TCPStates: TCPStatesConfig{Mode: statesMode},
		},
		logger: logger,
	}
	if portsMode != PortsModeOff || statesMode != TCPStatesModeOff {
		r.portsCollector = collectors.NewPortsCollector(root, filepath.Join(root, "no-services"), discardSlogLogger())
	}
	return r
}

// runCycle calls collectListeningPorts as one collection cycle does and
// returns the resource attributes and the metrics it produced.
func runCycle(r *metricsReceiver) (pcommon.Map, pmetric.MetricSlice) {
	md := pmetric.NewMetrics()
	attrs := md.ResourceMetrics().AppendEmpty().Resource().Attributes()
	ms := pmetric.NewMetricSlice()
	r.collectListeningPorts(context.Background(), attrs, ms)
	return attrs, ms
}

func findMetric(ms pmetric.MetricSlice, name string) (pmetric.Metric, bool) {
	for i := 0; i < ms.Len(); i++ {
		if ms.At(i).Name() == name {
			return ms.At(i), true
		}
	}
	return pmetric.Metric{}, false
}

func stateValues(t *testing.T, m pmetric.Metric) map[string]int64 {
	t.Helper()
	dps := m.Sum().DataPoints()
	out := map[string]int64{}
	for i := 0; i < dps.Len(); i++ {
		s, _ := dps.At(i).Attributes().Get("state")
		out[s.Str()] = dps.At(i).IntValue()
	}
	return out
}

func TestBuildTCPStateMetricShape(t *testing.T) {
	at := time.Unix(1795527600, 0)
	var counts collectors.TCPStateCounts
	counts[0x01], counts[0x0A], counts[0x06] = 19, 64, 7
	ms := pmetric.NewMetricSlice()
	buildTCPStateMetric(ms, &collectors.TCPStates{At: at, Counts: counts})

	if ms.Len() != 1 {
		t.Fatalf("metrics = %d, want 1", ms.Len())
	}
	m := ms.At(0)
	if m.Name() != "system.network.connections" || m.Unit() != "{connections}" || m.Description() != "The number of connections." {
		t.Errorf("name/unit/description = %q/%q/%q", m.Name(), m.Unit(), m.Description())
	}
	if m.Type() != pmetric.MetricTypeSum || m.Sum().IsMonotonic() || m.Sum().AggregationTemporality() != pmetric.AggregationTemporalityCumulative {
		t.Errorf("want a non-monotonic cumulative Sum, got %v", m.Type())
	}
	dps := m.Sum().DataPoints()
	if dps.Len() != 12 {
		t.Fatalf("datapoints = %d, want 12", dps.Len())
	}
	got := map[string]int64{}
	for i := 0; i < dps.Len(); i++ {
		dp := dps.At(i)
		if dp.Attributes().Len() != 2 {
			t.Errorf("datapoint %d has %d attributes, want protocol and state only", i, dp.Attributes().Len())
		}
		if p, _ := dp.Attributes().Get("protocol"); p.Str() != "tcp" {
			t.Errorf("protocol = %q", p.Str())
		}
		if dp.Timestamp().AsTime().Unix() != at.Unix() || dp.ValueType() != pmetric.NumberDataPointValueTypeInt {
			t.Errorf("datapoint %d: timestamp %v, value type %v", i, dp.Timestamp().AsTime(), dp.ValueType())
		}
		s, _ := dp.Attributes().Get("state")
		if s.Str() != collectors.TCPStateLabels[i] {
			t.Errorf("datapoint %d state = %q, want %q (TCPStateLabels order)", i, s.Str(), collectors.TCPStateLabels[i])
		}
		got[s.Str()] = dp.IntValue()
	}
	if got["ESTABLISHED"] != 19 || got["LISTEN"] != 64 || got["TIME_WAIT"] != 7 || got["DELETE"] != 0 || len(got) != 12 {
		t.Errorf("states = %v", got)
	}
}

func TestTCPStatesRideTheCycle(t *testing.T) {
	root := t.TempDir()
	r := newModesTestReceiver(root, PortsModeAuto, TCPStatesModeAuto, zap.NewNop())
	writeProcTCP(t, root, sockSpec{22, 3})
	at := time.Now().Add(-5 * time.Second).Truncate(time.Millisecond)
	mustSample(t, r, at)

	attrs, ms := runCycle(r)
	if _, ok := attrs.Get("listening.ports"); !ok {
		t.Error("listening.ports attribute missing")
	}
	for _, name := range []string{"monitorable.port.connections", "monitorable.port.connections.peak"} {
		if _, ok := findMetric(ms, name); !ok {
			t.Errorf("%s missing", name)
		}
	}
	m, ok := findMetric(ms, "system.network.connections")
	if !ok {
		t.Fatal("system.network.connections missing")
	}
	dps := m.Sum().DataPoints()
	if dps.Len() != 12 {
		t.Fatalf("datapoints = %d, want 12", dps.Len())
	}
	if got := dps.At(0).Timestamp().AsTime(); !got.Equal(at) {
		t.Errorf("timestamp = %v, want the sample time %v", got, at)
	}
	if v := stateValues(t, m); v["ESTABLISHED"] != 3 || v["LISTEN"] != 1 {
		t.Errorf("states = %v", v)
	}
}

func TestTCPStatesOff(t *testing.T) {
	root := t.TempDir()
	r := newModesTestReceiver(root, PortsModeAuto, TCPStatesModeOff, zap.NewNop())
	writeProcTCP(t, root, sockSpec{22, 1})
	mustSample(t, r, time.Now())

	attrs, ms := runCycle(r)
	if _, ok := attrs.Get("listening.ports"); !ok {
		t.Error("listening.ports attribute missing")
	}
	if _, ok := findMetric(ms, "monitorable.port.connections"); !ok {
		t.Error("per-port gauge missing")
	}
	if _, ok := findMetric(ms, "system.network.connections"); ok {
		t.Error("system.network.connections emitted with tcp_states.mode off")
	}
}

func TestTCPStatesWithPortsOff(t *testing.T) {
	root := t.TempDir()
	r := newModesTestReceiver(root, PortsModeOff, TCPStatesModeAuto, zap.NewNop())
	if r.runsPortsSampler() {
		t.Error("runsPortsSampler() = true with ports.mode off")
	}
	writeProcTCP(t, root, sockSpec{22, 2})

	attrs, ms := runCycle(r)
	if _, ok := attrs.Get("listening.ports"); ok {
		t.Error("listening.ports attribute set with ports.mode off")
	}
	if _, ok := findMetric(ms, "monitorable.port.connections"); ok {
		t.Error("per-port gauge emitted with ports.mode off")
	}
	m, ok := findMetric(ms, "system.network.connections")
	if !ok {
		t.Fatal("system.network.connections missing")
	}
	if n := m.Sum().DataPoints().Len(); n != 12 {
		t.Fatalf("datapoints = %d, want 12", n)
	}
	if v := stateValues(t, m)["ESTABLISHED"]; v != 2 {
		t.Errorf("ESTABLISHED = %d, want 2", v)
	}

	// The next cycle takes its own sample again: the count follows the table.
	writeProcTCP(t, root, sockSpec{22, 6})
	_, ms = runCycle(r)
	m, ok = findMetric(ms, "system.network.connections")
	if !ok {
		t.Fatal("second cycle: system.network.connections missing")
	}
	if v := stateValues(t, m)["ESTABLISHED"]; v != 6 {
		t.Errorf("second cycle ESTABLISHED = %d, want 6", v)
	}
}

func TestBothOffBuildsNoCollector(t *testing.T) {
	build := func(portsMode, statesMode string) *metricsReceiver {
		t.Helper()
		cfg := createDefaultConfig().(*Config)
		cfg.Ports.Mode, cfg.TCPStates.Mode = portsMode, statesMode
		cfg.Docker.Mode, cfg.Systemd.Mode, cfg.Smart.Mode = DockerModeOff, SystemdModeOff, SmartModeOff
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
		rcv, err := newMetricsReceiver(receivertest.NewNopSettings(component.MustNewType(typeStr)), cfg, consumertest.NewNop())
		if err != nil {
			t.Fatal(err)
		}
		return rcv.(*metricsReceiver)
	}

	r := build(PortsModeOff, TCPStatesModeOff)
	if r.portsCollector != nil {
		t.Error("portsCollector built with both modes off")
	}
	if r.runsPortsSampler() {
		t.Error("runsPortsSampler() = true with both modes off")
	}
	attrs, ms := runCycle(r)
	if attrs.Len() != 0 || ms.Len() != 0 {
		t.Errorf("emitted %d attributes and %d metrics, want none", attrs.Len(), ms.Len())
	}

	// Ports off, states on: the collector exists for the in-cycle read, no sampler.
	r = build(PortsModeOff, TCPStatesModeAuto)
	if r.portsCollector == nil || r.runsPortsSampler() {
		t.Errorf("ports off + states auto: collector %v, sampler %v; want a collector and no sampler",
			r.portsCollector != nil, r.runsPortsSampler())
	}
	r = build(PortsModeAuto, TCPStatesModeOff)
	if r.portsCollector == nil || !r.runsPortsSampler() {
		t.Error("ports auto: want a collector and the sampler")
	}
}

func TestFirstCycleTakesItsOwnSample(t *testing.T) {
	root := t.TempDir()
	r := newModesTestReceiver(root, PortsModeAuto, TCPStatesModeAuto, zap.NewNop())
	writeProcTCP(t, root, sockSpec{22, 1}, sockSpec{80, 2})
	// No Sample call: the sampler goroutine has not run yet.

	attrs, ms := runCycle(r)
	if _, ok := attrs.Get("listening.ports"); !ok {
		t.Error("listening.ports attribute missing on the first cycle")
	}
	for _, name := range []string{"monitorable.port.connections", "monitorable.port.connections.peak"} {
		if _, ok := findMetric(ms, name); !ok {
			t.Errorf("%s missing on the first cycle", name)
		}
	}
	m, ok := findMetric(ms, "system.network.connections")
	if !ok {
		t.Fatal("system.network.connections missing on the first cycle")
	}
	if n := m.Sum().DataPoints().Len(); n != 12 {
		t.Errorf("datapoints = %d, want 12", n)
	}
}

func TestTCPStatesUnreadable(t *testing.T) {
	for _, tc := range []struct {
		mode  string
		warns int
	}{{TCPStatesModeOn, 1}, {TCPStatesModeAuto, 0}} {
		t.Run(tc.mode, func(t *testing.T) {
			core, logs := observer.New(zap.DebugLevel)
			r := newModesTestReceiver(t.TempDir(), PortsModeOff, tc.mode, zap.New(core)) // no net/tcp
			for range 3 {
				if _, ms := runCycle(r); ms.Len() != 0 {
					t.Fatalf("emitted %d metrics with /proc/net/tcp missing", ms.Len())
				}
			}
			if n := logs.FilterLevelExact(zap.WarnLevel).Len(); n != tc.warns {
				t.Errorf("WARN count = %d, want %d; logs = %v", n, tc.warns, logs.All())
			}
			if tc.mode == TCPStatesModeAuto && logs.FilterLevelExact(zap.DebugLevel).Len() != 1 {
				t.Errorf("auto: want one DEBUG line, logs = %v", logs.All())
			}
		})
	}
}
