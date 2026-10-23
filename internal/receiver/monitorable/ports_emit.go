package monitorable

import (
	"context"
	"strconv"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.uber.org/zap"

	"github.com/monitorable-io/monitorable-agent/internal/collectors"
)

// portsSampleTimeout bounds one /proc/net/tcp{,6} read in the sampler.
const portsSampleTimeout = 5 * time.Second

// runPortsSampler takes one listening-ports sample immediately, then one per
// interval, until ctx is cancelled. The caller must have done r.wg.Add(1). A failed
// sample is not logged here; the Snapshot status transition (logPortsTransition)
// reports it once.
func (r *metricsReceiver) runPortsSampler(ctx context.Context, interval time.Duration) {
	defer r.wg.Done()

	sample := func() {
		cctx, cancel := context.WithTimeout(ctx, portsSampleTimeout)
		defer cancel()
		_ = r.portsCollector.Sample(cctx, time.Now())
	}

	if interval <= 0 {
		interval = defaultPortsSampleInterval // NewTicker panics on <= 0; Validate normally guarantees this
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	sample()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sample()
		}
	}
}

// collectListeningPorts attaches the window's listening-ports snapshot as the
// `listening.ports` resource attribute and appends the per-port connection gauges
// to metrics, once per collection cycle. No-op when ports.mode=off or when the
// window holds no usable sample (agent just started, or /proc/net/tcp unreadable).
func (r *metricsReceiver) collectListeningPorts(_ context.Context, attrs pcommon.Map, metrics pmetric.MetricSlice) {
	if r.portsCollector == nil {
		return // ports.mode=off
	}
	res := r.portsCollector.Snapshot(r.cfg.Ports.MaxPorts)
	r.logPortsTransition(res.Status, res.Err)
	if res.Status != collectors.PortsOK {
		return
	}
	r.putBoundedJSON(attrs, "listening.ports", res.Snapshot, &r.portsJSONWarned)
	buildPortMetrics(metrics, res.Snapshot)
}

// buildPortMetrics appends monitorable.port.connections (mean ESTABLISHED count,
// double) and monitorable.port.connections.peak (max, int) gauges with one
// datapoint per detailed port. Nothing is appended when no port has detail.
func buildPortMetrics(ms pmetric.MetricSlice, snap *collectors.PortsSnapshot) {
	if len(snap.Ports) == 0 {
		return
	}
	ts := pcommon.NewTimestampFromTime(time.UnixMilli(snap.CollectedAtUnixMs))

	avg := ms.AppendEmpty()
	avg.SetName("monitorable.port.connections")
	avg.SetDescription("Mean ESTABLISHED TCP connections per listening port over the sampling window.")
	avg.SetUnit("{connection}")
	avgDPs := avg.SetEmptyGauge().DataPoints()

	peak := ms.AppendEmpty()
	peak.SetName("monitorable.port.connections.peak")
	peak.SetDescription("Peak ESTABLISHED TCP connections per listening port over the sampling window.")
	peak.SetUnit("{connection}")
	peakDPs := peak.SetEmptyGauge().DataPoints()

	for _, p := range snap.Ports {
		port := strconv.Itoa(p.Port)

		dp := avgDPs.AppendEmpty()
		dp.SetTimestamp(ts)
		dp.SetDoubleValue(p.ConnAvg)
		dp.Attributes().PutStr("port", port)

		dp = peakDPs.AppendEmpty()
		dp.SetTimestamp(ts)
		dp.SetIntValue(int64(p.ConnPeak))
		dp.Attributes().PutStr("port", port)
	}
}

// logPortsTransition logs once whenever the ports status changes, so steady state is
// silent. "unreadable" warns only under mode=on (auto stays at DEBUG); "no samples"
// (the first cycle races the first sample) is not a transition and never logs.
func (r *metricsReceiver) logPortsTransition(status collectors.PortsStatus, err error) {
	if status == collectors.PortsNoSamples || status == r.portsPrev {
		return
	}
	switch status {
	case collectors.PortsOK:
		r.logger.Info("Collecting listening-port snapshot")
	case collectors.PortsUnreadable:
		if r.portsMode == PortsModeOn {
			r.logger.Warn("Listening ports unreadable but ports.mode=on; no port snapshot", zap.Error(err))
		} else {
			r.logger.Debug("Listening ports unreadable; not collecting port snapshot", zap.Error(err))
		}
	}
	r.portsPrev = status
}
