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

// runsPortsSampler reports whether the sampler goroutine runs: only for the
// ports list. With ports.mode off and tcp_states on, each collection cycle
// takes its own sample instead.
func (r *metricsReceiver) runsPortsSampler() bool {
	return r.portsCollector != nil && r.portsMode != PortsModeOff
}

// sampleInCycle takes one /proc/net/tcp{,6} sample inside the collection cycle.
// A failure is not logged here; the Snapshot status transition reports it.
func (r *metricsReceiver) sampleInCycle(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, portsSampleTimeout)
	defer cancel()
	_ = r.portsCollector.Sample(cctx, time.Now())
}

// collectListeningPorts runs once per collection cycle. With ports.mode on it
// attaches the window's listening-ports snapshot as the `listening.ports`
// resource attribute and appends the per-port connection gauges; with
// tcp_states.mode on it appends system.network.connections from the window's
// newest sample. The first cycle can run before the sampler's first sample, and
// with ports.mode off there is no sampler: in both cases the cycle samples
// itself. Nothing is emitted when /proc/net/tcp is unreadable.
func (r *metricsReceiver) collectListeningPorts(ctx context.Context, attrs pcommon.Map, metrics pmetric.MetricSlice) {
	if r.portsCollector == nil {
		return // ports.mode and tcp_states.mode both off
	}
	portsOn := r.portsMode != PortsModeOff
	statesOn := r.tcpStatesMode == TCPStatesModeAuto || r.tcpStatesMode == TCPStatesModeOn

	if !portsOn {
		r.sampleInCycle(ctx)
	}
	res := r.portsCollector.Snapshot(r.cfg.Ports.MaxPorts)
	if portsOn && res.Status == collectors.PortsNoSamples {
		r.sampleInCycle(ctx)
		res = r.portsCollector.Snapshot(r.cfg.Ports.MaxPorts)
	}

	if portsOn {
		r.logPortsTransition(res.Status, res.Err)
		if res.Status == collectors.PortsOK {
			r.putBoundedJSON(attrs, "listening.ports", res.Snapshot, &r.portsJSONWarned)
			buildPortMetrics(metrics, res.Snapshot)
		}
	}
	if statesOn {
		r.logTCPStatesTransition(res.Status, res.Err)
		if res.States != nil {
			buildTCPStateMetric(metrics, res.States)
		}
	}
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
