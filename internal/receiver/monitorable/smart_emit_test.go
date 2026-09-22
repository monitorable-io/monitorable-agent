package monitorable

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
	"go.uber.org/zap/zaptest/observer"

	"github.com/monitorable-io/monitorable-agent/internal/collectors"
)

func smartAttrMap() (pmetric.Metrics, func() (string, bool)) {
	md := pmetric.NewMetrics()
	attrs := md.ResourceMetrics().AppendEmpty().Resource().Attributes()
	return md, func() (string, bool) {
		v, ok := attrs.Get("smart.disks")
		if !ok {
			return "", false
		}
		return v.Str(), true
	}
}

func TestCollectSmartNoopWhenCollectorNil(t *testing.T) {
	r := &metricsReceiver{} // smartCollector nil (smart.mode=off)
	md, get := smartAttrMap()
	r.collectSmartDisks(context.Background(), md.ResourceMetrics().At(0).Resource().Attributes())
	if _, ok := get(); ok {
		t.Fatal("attribute set despite nil collector")
	}
}

func TestCollectSmartRespectsCadence(t *testing.T) {
	r := &metricsReceiver{
		smartCollector: collectors.NewSmartCollector(),
		smartInterval:  15 * time.Minute,
		lastSmartAt:    time.Now(), // sampled "just now" → not due
		cfg:            &Config{Smart: SmartConfig{Timeout: 10 * time.Second}},
	}
	md, get := smartAttrMap()
	r.collectSmartDisks(context.Background(), md.ResourceMetrics().At(0).Resource().Attributes())
	if _, ok := get(); ok {
		t.Fatal("attribute set before interval elapsed")
	}
}

func TestLogSmartTransitionOnlyOnChange(t *testing.T) {
	r := &metricsReceiver{logger: zaptest.NewLogger(t), smartMode: SmartModeAuto, smartPrev: collectors.SmartStatus(-1)}
	r.logSmartTransition(collectors.SmartPresent)
	if r.smartPrev != collectors.SmartPresent {
		t.Fatalf("smartPrev = %v, want SmartPresent", r.smartPrev)
	}
	// Same status again — still SmartPresent, no panic, no state change.
	r.logSmartTransition(collectors.SmartPresent)
	if r.smartPrev != collectors.SmartPresent {
		t.Fatalf("smartPrev changed unexpectedly")
	}
	r.logSmartTransition(collectors.SmartAbsent)
	if r.smartPrev != collectors.SmartAbsent {
		t.Fatalf("smartPrev = %v, want SmartAbsent", r.smartPrev)
	}
}

func TestLogSmartTransitionMessages(t *testing.T) {
	// mode=auto: NoPermission emits the actionable WARN; Absent stays quiet (no WARN).
	core, logs := observer.New(zap.DebugLevel)
	r := &metricsReceiver{logger: zap.New(core), smartMode: SmartModeAuto, smartPrev: collectors.SmartStatus(-1)}

	r.logSmartTransition(collectors.SmartNoPermission)
	if logs.FilterMessageSnippet("CAP_SYS_RAWIO").Len() != 1 {
		t.Errorf("NoPermission should emit an actionable WARN naming the missing capability")
	}

	r.logSmartTransition(collectors.SmartAbsent)
	if logs.FilterLevelExact(zap.WarnLevel).FilterMessageSnippet("no readable physical disk").Len() != 0 {
		t.Errorf("auto-mode Absent must not warn")
	}

	// mode=on: Absent escalates to a WARN (operator expected a readable disk).
	core2, logs2 := observer.New(zap.DebugLevel)
	r2 := &metricsReceiver{logger: zap.New(core2), smartMode: SmartModeOn, smartPrev: collectors.SmartStatus(-1)}
	r2.logSmartTransition(collectors.SmartAbsent)
	if logs2.FilterLevelExact(zap.WarnLevel).Len() != 1 {
		t.Errorf("mode=on Absent should emit a WARN")
	}
}
