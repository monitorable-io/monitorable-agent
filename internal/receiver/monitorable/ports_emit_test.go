package monitorable

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/monitorable-io/monitorable-agent/internal/collectors"
)

// sockRow renders one /proc/net/tcp row for 127.0.0.1:<port>. state is the
// kernel's hex state: "0A" LISTEN, "01" ESTABLISHED.
func sockRow(slot, port int, state string) string {
	return fmt.Sprintf("%4d: 0100007F:%04X 00000000:0000 %s 00000000:00000000 00:00000000 00000000     0        0 12345 1 0000000000000000 100 0 0 10 0\n",
		slot, port, state)
}

type sockSpec struct {
	port        int
	established int
}

// writeProcTCP writes <root>/net/tcp with one LISTEN row per spec plus its
// ESTABLISHED rows.
func writeProcTCP(t *testing.T, root string, specs ...sockSpec) {
	t.Helper()
	var b strings.Builder
	b.WriteString("  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n")
	slot := 0
	for _, s := range specs {
		b.WriteString(sockRow(slot, s.port, "0A"))
		slot++
		for range s.established {
			b.WriteString(sockRow(slot, s.port, "01"))
			slot++
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "net"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "net", "tcp"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newPortsTestReceiver(root string, maxPorts int) *metricsReceiver {
	return &metricsReceiver{
		portsCollector: collectors.NewPortsCollector(root, filepath.Join(root, "no-services"), discardSlogLogger()),
		portsMode:      PortsModeAuto,
		portsPrev:      collectors.PortsStatus(-1),
		cfg:            &Config{Ports: PortsConfig{Mode: PortsModeAuto, SampleInterval: 10 * time.Second, MaxPorts: maxPorts}},
		logger:         zap.NewNop(),
	}
}

func mustSample(t *testing.T, r *metricsReceiver, at time.Time) {
	t.Helper()
	if err := r.portsCollector.Sample(context.Background(), at); err != nil {
		t.Fatalf("Sample: %v", err)
	}
}

func portsAttrs() (pmetric.Metrics, func() (string, bool)) {
	md := pmetric.NewMetrics()
	attrs := md.ResourceMetrics().AppendEmpty().Resource().Attributes()
	return md, func() (string, bool) {
		v, ok := attrs.Get("listening.ports")
		if !ok {
			return "", false
		}
		return v.Str(), true
	}
}

func TestCollectPortsNoopWhenCollectorNil(t *testing.T) {
	r := &metricsReceiver{} // portsCollector nil (ports.mode=off)
	md, get := portsAttrs()
	ms := pmetric.NewMetricSlice()
	r.collectListeningPorts(context.Background(), md.ResourceMetrics().At(0).Resource().Attributes(), ms)
	if _, ok := get(); ok {
		t.Fatal("attribute set despite nil collector")
	}
	if ms.Len() != 0 {
		t.Fatalf("metrics = %d, want 0", ms.Len())
	}
}

func TestCollectPortsEmitsSnapshotAndGauges(t *testing.T) {
	root := t.TempDir()
	r := newPortsTestReceiver(root, 30)

	writeProcTCP(t, root, sockSpec{22, 1}, sockSpec{80, 0})
	t0 := time.Now().Add(-10 * time.Second).Truncate(time.Millisecond)
	mustSample(t, r, t0)
	writeProcTCP(t, root, sockSpec{22, 3}, sockSpec{80, 0})
	t1 := t0.Add(10 * time.Second)
	mustSample(t, r, t1)

	md, get := portsAttrs()
	ms := pmetric.NewMetricSlice()
	r.collectListeningPorts(context.Background(), md.ResourceMetrics().At(0).Resource().Attributes(), ms)

	raw, ok := get()
	if !ok {
		t.Fatal("listening.ports attribute missing")
	}
	var snap collectors.PortsSnapshot
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		t.Fatalf("attribute is not a PortsSnapshot: %v", err)
	}
	if len(snap.AllPorts) != 2 || snap.AllPorts[0] != 22 || snap.AllPorts[1] != 80 {
		t.Fatalf("AllPorts = %v, want [22 80]", snap.AllPorts)
	}
	if len(snap.Ports) != 2 || snap.Ports[0].Port != 22 {
		t.Fatalf("Ports = %+v", snap.Ports)
	}
	if snap.Ports[0].ConnAvg != 2 || snap.Ports[0].ConnPeak != 3 {
		t.Fatalf("port 22 avg/peak = %v/%v, want 2/3", snap.Ports[0].ConnAvg, snap.Ports[0].ConnPeak)
	}
	if snap.CollectedAtUnixMs != t1.UnixMilli() {
		t.Fatalf("CollectedAtUnixMs = %d, want %d", snap.CollectedAtUnixMs, t1.UnixMilli())
	}
	for _, banned := range []string{"127.0.0.1", "0100007F", "cmdline", "pid"} {
		if strings.Contains(raw, banned) {
			t.Errorf("payload leaks %q: %s", banned, raw)
		}
	}

	if ms.Len() != 2 {
		t.Fatalf("metrics = %d, want 2", ms.Len())
	}
	avg, peak := ms.At(0), ms.At(1)
	if avg.Name() != "monitorable.port.connections" || peak.Name() != "monitorable.port.connections.peak" {
		t.Fatalf("names = %q, %q", avg.Name(), peak.Name())
	}
	if avg.Unit() != "{connection}" || peak.Unit() != "{connection}" {
		t.Fatalf("units = %q, %q", avg.Unit(), peak.Unit())
	}
	if avg.Type() != pmetric.MetricTypeGauge || peak.Type() != pmetric.MetricTypeGauge {
		t.Fatal("both metrics must be gauges")
	}
	adps, pdps := avg.Gauge().DataPoints(), peak.Gauge().DataPoints()
	if adps.Len() != 2 || pdps.Len() != 2 {
		t.Fatalf("datapoints = %d/%d, want 2/2", adps.Len(), pdps.Len())
	}
	a0, p0 := adps.At(0), pdps.At(0)
	if v, _ := a0.Attributes().Get("port"); v.Str() != "22" {
		t.Fatalf("avg dp port attr = %q, want 22", v.Str())
	}
	if v, _ := p0.Attributes().Get("port"); v.Str() != "22" {
		t.Fatalf("peak dp port attr = %q, want 22", v.Str())
	}
	if a0.ValueType() != pmetric.NumberDataPointValueTypeDouble || a0.DoubleValue() != 2 {
		t.Fatalf("avg dp = %v (%v), want double 2", a0.DoubleValue(), a0.ValueType())
	}
	if p0.ValueType() != pmetric.NumberDataPointValueTypeInt || p0.IntValue() != 3 {
		t.Fatalf("peak dp = %v (%v), want int 3", p0.IntValue(), p0.ValueType())
	}
	if a0.Timestamp().AsTime().UnixMilli() != t1.UnixMilli() || p0.Timestamp().AsTime().UnixMilli() != t1.UnixMilli() {
		t.Fatalf("datapoint timestamps = %v / %v, want %v", a0.Timestamp().AsTime(), p0.Timestamp().AsTime(), t1)
	}
}

func TestCollectPortsNothingWithoutSamples(t *testing.T) {
	r := newPortsTestReceiver(t.TempDir(), 30)
	md, get := portsAttrs()
	ms := pmetric.NewMetricSlice()
	r.collectListeningPorts(context.Background(), md.ResourceMetrics().At(0).Resource().Attributes(), ms)
	if _, ok := get(); ok {
		t.Fatal("attribute set without samples")
	}
	if ms.Len() != 0 {
		t.Fatalf("metrics = %d, want 0", ms.Len())
	}
}

func TestCollectPortsNoListenerKeepsAttributeNoGauges(t *testing.T) {
	root := t.TempDir()
	r := newPortsTestReceiver(root, 30)
	writeProcTCP(t, root) // header only: no listener at all
	mustSample(t, r, time.Now())

	md, get := portsAttrs()
	ms := pmetric.NewMetricSlice()
	r.collectListeningPorts(context.Background(), md.ResourceMetrics().At(0).Resource().Attributes(), ms)
	if _, ok := get(); !ok {
		t.Fatal("attribute must still be set for a host with no listener")
	}
	if ms.Len() != 0 {
		t.Fatalf("metrics = %d, want 0 (no empty gauges)", ms.Len())
	}
}

func TestCollectPortsRespectsMaxPorts(t *testing.T) {
	root := t.TempDir()
	r := newPortsTestReceiver(root, 2)
	writeProcTCP(t, root, sockSpec{22, 1}, sockSpec{80, 1}, sockSpec{443, 1})
	mustSample(t, r, time.Now())

	md, get := portsAttrs()
	ms := pmetric.NewMetricSlice()
	r.collectListeningPorts(context.Background(), md.ResourceMetrics().At(0).Resource().Attributes(), ms)

	raw, _ := get()
	var snap collectors.PortsSnapshot
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Ports) != 2 || len(snap.AllPorts) != 3 {
		t.Fatalf("ports=%d all_ports=%d, want 2 and 3", len(snap.Ports), len(snap.AllPorts))
	}
	if ms.Len() != 2 {
		t.Fatalf("metrics = %d, want 2", ms.Len())
	}
	for i := 0; i < ms.Len(); i++ {
		if n := ms.At(i).Gauge().DataPoints().Len(); n != 2 {
			t.Errorf("metric %s datapoints = %d, want 2", ms.At(i).Name(), n)
		}
	}
}

func TestPortsSamplerStopsOnShutdown(t *testing.T) {
	root := t.TempDir()
	r := newPortsTestReceiver(root, 30)
	writeProcTCP(t, root, sockSpec{22, 0})

	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.wg.Add(1)
	go r.runPortsSampler(ctx, 10*time.Millisecond)

	// The sampler samples immediately and on each tick.
	deadline := time.Now().Add(5 * time.Second)
	for {
		res := r.portsCollector.Snapshot(30)
		if res.Status == collectors.PortsOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sampler produced no sample, last status %v", res.Status)
		}
		time.Sleep(5 * time.Millisecond)
	}

	done := make(chan struct{})
	go func() {
		_ = r.Shutdown(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return: sampler goroutine still running")
	}

	// No further samples after Shutdown.
	r.portsCollector.Snapshot(30) // drain
	time.Sleep(50 * time.Millisecond)
	if res := r.portsCollector.Snapshot(30); res.Status == collectors.PortsOK {
		t.Fatal("sampler kept sampling after Shutdown")
	}
}

func TestLogPortsTransition(t *testing.T) {
	boom := errors.New("open /proc/net/tcp: boom")

	t.Run("unreadable warns once under mode on", func(t *testing.T) {
		core, logs := observer.New(zap.DebugLevel)
		r := &metricsReceiver{logger: zap.New(core), portsMode: PortsModeOn, portsPrev: collectors.PortsStatus(-1)}
		r.logPortsTransition(collectors.PortsUnreadable, boom)
		r.logPortsTransition(collectors.PortsUnreadable, boom)
		if n := logs.FilterLevelExact(zap.WarnLevel).Len(); n != 1 {
			t.Fatalf("WARN count = %d, want 1; logs=%v", n, logs.All())
		}
	})
	t.Run("unreadable is DEBUG under mode auto", func(t *testing.T) {
		core, logs := observer.New(zap.DebugLevel)
		r := &metricsReceiver{logger: zap.New(core), portsMode: PortsModeAuto, portsPrev: collectors.PortsStatus(-1)}
		r.logPortsTransition(collectors.PortsUnreadable, boom)
		if logs.FilterLevelExact(zap.WarnLevel).Len() != 0 || logs.FilterLevelExact(zap.DebugLevel).Len() != 1 {
			t.Fatalf("logs = %v", logs.All())
		}
		if logs.All()[0].ContextMap()["error"] == nil {
			t.Error("DEBUG line should carry the error")
		}
	})
	t.Run("ok logs INFO once per transition", func(t *testing.T) {
		core, logs := observer.New(zap.DebugLevel)
		r := &metricsReceiver{logger: zap.New(core), portsMode: PortsModeAuto, portsPrev: collectors.PortsStatus(-1)}
		r.logPortsTransition(collectors.PortsOK, nil)
		r.logPortsTransition(collectors.PortsOK, nil)
		if n := logs.FilterLevelExact(zap.InfoLevel).Len(); n != 1 {
			t.Fatalf("INFO count = %d, want 1", n)
		}
		// recovery after an unreadable spell logs INFO again
		r.logPortsTransition(collectors.PortsUnreadable, boom)
		r.logPortsTransition(collectors.PortsOK, nil)
		if n := logs.FilterLevelExact(zap.InfoLevel).Len(); n != 2 {
			t.Fatalf("INFO count after recovery = %d, want 2", n)
		}
	})
	t.Run("no samples is quiet", func(t *testing.T) {
		core, logs := observer.New(zap.InfoLevel)
		r := &metricsReceiver{logger: zap.New(core), portsMode: PortsModeOn, portsPrev: collectors.PortsStatus(-1)}
		r.logPortsTransition(collectors.PortsNoSamples, nil)
		if logs.Len() != 0 {
			t.Fatalf("logs = %v", logs.All())
		}
	})
}
