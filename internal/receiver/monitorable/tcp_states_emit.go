package monitorable

import (
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.uber.org/zap"

	"github.com/monitorable-io/monitorable-agent/internal/collectors"
)

// buildTCPStateMetric appends system.network.connections in the shape the
// hostmetrics network scraper (v0.162.0) emits it, so the backend stores the
// same series: a non-monotonic cumulative integer Sum with one datapoint per
// label of collectors.TCPStateLabels, zero included, attributes protocol="tcp"
// and state. The datapoints carry the time of the sample the counts came from.
func buildTCPStateMetric(ms pmetric.MetricSlice, st *collectors.TCPStates) {
	m := ms.AppendEmpty()
	m.SetName("system.network.connections")
	m.SetDescription("The number of connections.")
	m.SetUnit("{connections}")
	sum := m.SetEmptySum()
	sum.SetIsMonotonic(false)
	sum.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)

	ts := pcommon.NewTimestampFromTime(st.At)
	byLabel := st.Counts.ByLabel()
	dps := sum.DataPoints()
	dps.EnsureCapacity(len(collectors.TCPStateLabels))
	for _, label := range collectors.TCPStateLabels {
		dp := dps.AppendEmpty()
		dp.SetTimestamp(ts)
		dp.SetIntValue(byLabel[label])
		dp.Attributes().PutStr("protocol", "tcp")
		dp.Attributes().PutStr("state", label)
	}
}

// logTCPStatesTransition logs once whenever the TCP state count status changes,
// in the style of logPortsTransition: "unreadable" warns only under
// tcp_states.mode=on (auto stays at DEBUG); "no samples" never logs.
func (r *metricsReceiver) logTCPStatesTransition(status collectors.PortsStatus, err error) {
	if status == collectors.PortsNoSamples || status == r.tcpStatesPrev {
		return
	}
	switch status {
	case collectors.PortsOK:
		r.logger.Info("Collecting TCP state counts")
	case collectors.PortsUnreadable:
		if r.tcpStatesMode == TCPStatesModeOn {
			r.logger.Warn("TCP socket table unreadable but tcp_states.mode=on; no TCP state counts", zap.Error(err))
		} else {
			r.logger.Debug("TCP socket table unreadable; not collecting TCP state counts", zap.Error(err))
		}
	}
	r.tcpStatesPrev = status
}
