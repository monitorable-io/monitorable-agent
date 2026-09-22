package monitorable

import (
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"

	"github.com/monitorable-io/monitorable-agent/internal/collectors"
)

// cpuStateValue pairs an OTel state attribute with its per-core seconds value.
type cpuStateValue struct {
	state string
	value float64
}

// buildCPUTimeMetric appends a single "system.cpu.time" metric (cumulative, monotonic
// Sum, unit "s") to ms, with one datapoint per (cpu, state). This reproduces exactly
// the series the backend already ingests (avg without(cpu)(rate(system.cpu.time_total
// {state=...}))*100); the cpu label uses the kernel's natural per-core id.
func buildCPUTimeMetric(ms pmetric.MetricSlice, times *collectors.CPUTimes, now time.Time) {
	m := ms.AppendEmpty()
	m.SetName("system.cpu.time")
	m.SetDescription("Total seconds each logical CPU spent on each mode.")
	m.SetUnit("s")
	sum := m.SetEmptySum()
	sum.SetIsMonotonic(true)
	sum.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)

	// StartTimestamp is the counter's origin (system boot). /proc/stat always carries
	// a "btime" line on supported kernels; if it were ever absent, BootTime is 0 and
	// start falls back to the Unix epoch — harmless, since the backend computes usage
	// via rate(), which does not depend on the start timestamp.
	start := pcommon.NewTimestampFromTime(time.Unix(times.BootTime, 0))
	ts := pcommon.NewTimestampFromTime(now)
	dps := sum.DataPoints()

	for i := range times.PerCPU {
		c := times.PerCPU[i]
		for _, sv := range []cpuStateValue{
			{"user", c.User},
			{"nice", c.Nice},
			{"system", c.System},
			{"idle", c.Idle},
			{"wait", c.Iowait},
			{"interrupt", c.Irq},
			{"softirq", c.Softirq},
			{"steal", c.Steal},
		} {
			dp := dps.AppendEmpty()
			dp.SetStartTimestamp(start)
			dp.SetTimestamp(ts)
			dp.SetDoubleValue(sv.value)
			dp.Attributes().PutStr("cpu", c.CPU)
			dp.Attributes().PutStr("state", sv.state)
		}
	}
}
