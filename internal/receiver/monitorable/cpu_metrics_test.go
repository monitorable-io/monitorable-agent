package monitorable

import (
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/pmetric"

	"github.com/monitorable-io/monitorable-agent/internal/collectors"
)

func TestBuildCPUTimeMetric_ShapeAndAttributes(t *testing.T) {
	times := &collectors.CPUTimes{
		BootTime: 1777625254,
		PerCPU: []collectors.CPUStateTime{
			{CPU: "cpu8", User: 0.10, Nice: 0, System: 0.20, Idle: 0.30, Iowait: 0.04, Irq: 0, Softirq: 0.01, Steal: 0},
			{CPU: "cpu44", User: 0.90, Nice: 0, System: 1.80, Idle: 2.70, Iowait: 0.36, Irq: 0, Softirq: 0.07, Steal: 0},
		},
	}
	ms := pmetric.NewMetricSlice()
	buildCPUTimeMetric(ms, times, time.Unix(1777700000, 0))

	if ms.Len() != 1 {
		t.Fatalf("want 1 metric, got %d", ms.Len())
	}
	m := ms.At(0)
	if m.Name() != "system.cpu.time" {
		t.Fatalf("name = %q", m.Name())
	}
	if m.Type() != pmetric.MetricTypeSum {
		t.Fatalf("type = %v, want Sum", m.Type())
	}
	if m.Unit() != "s" {
		t.Fatalf("unit = %q", m.Unit())
	}
	sum := m.Sum()
	if !sum.IsMonotonic() {
		t.Fatal("want monotonic Sum")
	}
	if sum.AggregationTemporality() != pmetric.AggregationTemporalityCumulative {
		t.Fatalf("temporality = %v, want cumulative", sum.AggregationTemporality())
	}
	// 2 cpus * 8 states = 16 datapoints.
	if sum.DataPoints().Len() != 16 {
		t.Fatalf("datapoints = %d, want 16", sum.DataPoints().Len())
	}
	// Spot-check cpu8/user.
	var found bool
	for i := 0; i < sum.DataPoints().Len(); i++ {
		dp := sum.DataPoints().At(i)
		cpu, _ := dp.Attributes().Get("cpu")
		state, _ := dp.Attributes().Get("state")
		if cpu.Str() == "cpu8" && state.Str() == "user" {
			found = true
			if dp.DoubleValue() != 0.10 {
				t.Fatalf("cpu8/user value = %v, want 0.10", dp.DoubleValue())
			}
			if dp.StartTimestamp() == 0 || dp.Timestamp() == 0 {
				t.Fatal("start and now timestamps must be set")
			}
		}
	}
	if !found {
		t.Fatal("cpu8/user datapoint not found")
	}
}
