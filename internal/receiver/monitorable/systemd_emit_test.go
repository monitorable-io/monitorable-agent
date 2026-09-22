package monitorable

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/pmetric"

	"github.com/monitorable-io/monitorable-agent/internal/collectors"
)

func newAttrMap() (pmetric.Metrics, func() (string, bool)) {
	md := pmetric.NewMetrics()
	attrs := md.ResourceMetrics().AppendEmpty().Resource().Attributes()
	return md, func() (string, bool) {
		v, ok := attrs.Get("systemd.services")
		if !ok {
			return "", false
		}
		return v.Str(), true
	}
}

func TestCollectSystemdNoopWhenCollectorNil(t *testing.T) {
	r := &metricsReceiver{} // systemdCollector nil
	md, get := newAttrMap()
	r.collectSystemdServices(context.Background(), md.ResourceMetrics().At(0).Resource().Attributes())
	if _, ok := get(); ok {
		t.Fatal("attribute set despite nil collector")
	}
}

func TestCollectSystemdRespectsCadence(t *testing.T) {
	r := &metricsReceiver{
		systemdCollector: collectors.NewSystemdCollector(discardSlogLogger()),
		systemdInterval:  5 * time.Minute,
		lastSystemdAt:    time.Now(), // sampled "just now" → not due
		cfg:              &Config{Systemd: SystemdConfig{Timeout: 10 * time.Second}},
	}
	md, get := newAttrMap()
	r.collectSystemdServices(context.Background(), md.ResourceMetrics().At(0).Resource().Attributes())
	if _, ok := get(); ok {
		t.Fatal("attribute set before interval elapsed")
	}
}
