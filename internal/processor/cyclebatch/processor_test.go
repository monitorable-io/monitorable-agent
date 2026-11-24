package cyclebatch

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/processor"
	"go.opentelemetry.io/collector/processor/processortest"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func newTestProcessorWith(t *testing.T, cfg *Config, set processor.Settings, next consumer.Metrics) processor.Metrics {
	t.Helper()
	p, err := NewFactory().CreateMetrics(context.Background(), set, cfg, next)
	if err != nil {
		t.Fatalf("CreateMetrics: %v", err)
	}
	if err := p.Start(context.Background(), nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	return p
}

func newTestProcessor(t *testing.T, deliveries int, maxWait time.Duration, next consumer.Metrics) processor.Metrics {
	t.Helper()
	return newTestProcessorWith(t, &Config{Deliveries: deliveries, MaxWait: maxWait},
		processortest.NewNopSettings(NewFactory().Type()), next)
}

func oneResource(name string) pmetric.Metrics {
	md := pmetric.NewMetrics()
	m := md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
	m.SetName(name)
	m.SetEmptyGauge().DataPoints().AppendEmpty().SetIntValue(1)
	return md
}

func consume(t *testing.T, p processor.Metrics, name string) {
	t.Helper()
	if err := p.ConsumeMetrics(context.Background(), oneResource(name)); err != nil {
		t.Fatalf("ConsumeMetrics(%s) = %v, want nil", name, err)
	}
}

// waitFor polls until the sink holds n Metrics or the deadline passes.
func waitFor(t *testing.T, sink *consumertest.MetricsSink, n int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if len(sink.AllMetrics()) >= n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("sink holds %d Metrics after %v, want %d", len(sink.AllMetrics()), within, n)
}

func resourceCounts(sink *consumertest.MetricsSink) []int {
	var out []int
	for _, md := range sink.AllMetrics() {
		out = append(out, md.ResourceMetrics().Len())
	}
	return out
}

func firstMetricName(rm pmetric.ResourceMetrics) string {
	return rm.ScopeMetrics().At(0).Metrics().At(0).Name()
}

func TestSendsWhenCountIsReached(t *testing.T) {
	sink := new(consumertest.MetricsSink)
	p := newTestProcessor(t, 2, time.Hour, sink)
	consume(t, p, "a")
	consume(t, p, "b")
	waitFor(t, sink, 1, time.Second)
	all := sink.AllMetrics()
	if len(all) != 1 || all[0].ResourceMetrics().Len() != 2 {
		t.Fatalf("sink = %v ResourceMetrics per Metrics, want [2]", resourceCounts(sink))
	}
	if a, b := firstMetricName(all[0].ResourceMetrics().At(0)), firstMetricName(all[0].ResourceMetrics().At(1)); a != "a" || b != "b" {
		t.Errorf("order = %s, %s; want a, b", a, b)
	}
}

func TestHoldsBelowCount(t *testing.T) {
	sink := new(consumertest.MetricsSink)
	p := newTestProcessor(t, 2, time.Hour, sink)
	consume(t, p, "a")
	time.Sleep(100 * time.Millisecond)
	if n := len(sink.AllMetrics()); n != 0 {
		t.Fatalf("sink holds %d Metrics, want 0", n)
	}
}

func TestSendsAfterMaxWait(t *testing.T) {
	sink := new(consumertest.MetricsSink)
	p := newTestProcessor(t, 2, 50*time.Millisecond, sink)
	consume(t, p, "a")
	if n := len(sink.AllMetrics()); n != 0 {
		t.Fatalf("sink holds %d Metrics right after the call, want 0", n)
	}
	waitFor(t, sink, 1, time.Second)
	if got := resourceCounts(sink); len(got) != 1 || got[0] != 1 {
		t.Fatalf("sink = %v, want [1]", got)
	}
}

func TestLateDeliveryStartsItsOwnGroup(t *testing.T) {
	sink := new(consumertest.MetricsSink)
	p := newTestProcessor(t, 2, 50*time.Millisecond, sink)
	consume(t, p, "early")
	waitFor(t, sink, 1, time.Second)
	consume(t, p, "late")
	waitFor(t, sink, 2, time.Second)
	if got := resourceCounts(sink); len(got) != 2 || got[0] != 1 || got[1] != 1 {
		t.Fatalf("sink = %v, want [1 1]", got)
	}
	// The next cycle groups normally again.
	consume(t, p, "next-a")
	consume(t, p, "next-b")
	waitFor(t, sink, 3, time.Second)
	if got := resourceCounts(sink); len(got) != 3 || got[2] != 2 {
		t.Fatalf("sink = %v, want [1 1 2]", got)
	}
}

func TestSilentPartnerDoesNotHoldData(t *testing.T) {
	sink := new(consumertest.MetricsSink)
	p := newTestProcessor(t, 2, 50*time.Millisecond, sink)
	for i := 0; i < 5; i++ {
		consume(t, p, "alone")
		time.Sleep(120 * time.Millisecond)
	}
	waitFor(t, sink, 5, time.Second)
	got := resourceCounts(sink)
	if len(got) != 5 {
		t.Fatalf("sink = %v, want five single deliveries", got)
	}
	for i, n := range got {
		if n != 1 {
			t.Errorf("Metrics %d has %d ResourceMetrics, want 1", i, n)
		}
	}
}

func TestGroupsDoNotMix(t *testing.T) {
	sink := new(consumertest.MetricsSink)
	p := newTestProcessor(t, 2, time.Hour, sink)
	for _, n := range []string{"a", "b", "c", "d"} {
		consume(t, p, n)
	}
	waitFor(t, sink, 2, time.Second)
	if got := resourceCounts(sink); len(got) != 2 || got[0] != 2 || got[1] != 2 {
		t.Fatalf("sink = %v, want [2 2]", got)
	}
}

func TestDefaultPassesEachDeliveryThrough(t *testing.T) {
	cfg := NewFactory().CreateDefaultConfig().(*Config)
	if cfg.Deliveries != 1 || cfg.MaxWait != 10*time.Second {
		t.Fatalf("default = %+v, want deliveries 1, max_wait 10s", cfg)
	}
	sink := new(consumertest.MetricsSink)
	p := newTestProcessorWith(t, cfg, processortest.NewNopSettings(NewFactory().Type()), sink)
	consume(t, p, "a")
	consume(t, p, "b")
	waitFor(t, sink, 2, time.Second)
	if got := resourceCounts(sink); len(got) != 2 {
		t.Fatalf("sink = %v, want two Metrics", got)
	}
}

func TestShutdownHandsOverPendingGroup(t *testing.T) {
	sink := new(consumertest.MetricsSink)
	p, err := NewFactory().CreateMetrics(context.Background(),
		processortest.NewNopSettings(NewFactory().Type()), &Config{Deliveries: 2, MaxWait: time.Hour}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Start(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	consume(t, p, "pending")
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if n := len(sink.AllMetrics()); n != 1 {
		t.Fatalf("sink holds %d Metrics when Shutdown returns, want 1", n)
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(sink.AllMetrics()); n != 1 {
		t.Fatalf("sink holds %d Metrics 100ms after Shutdown, want still 1", n)
	}
}

func TestNothingSentWhenIdle(t *testing.T) {
	sink := new(consumertest.MetricsSink)
	newTestProcessor(t, 2, 20*time.Millisecond, sink)
	time.Sleep(100 * time.Millisecond)
	if n := len(sink.AllMetrics()); n != 0 {
		t.Fatalf("sink holds %d Metrics, want 0", n)
	}
}

func TestDownstreamErrorIsLoggedNotReturned(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	set := processortest.NewNopSettings(NewFactory().Type())
	set.Logger = zap.New(core)
	p := newTestProcessorWith(t, &Config{Deliveries: 1, MaxWait: time.Second}, set,
		consumertest.NewErr(errors.New("boom")))
	if err := p.ConsumeMetrics(context.Background(), oneResource("a")); err != nil {
		t.Fatalf("ConsumeMetrics = %v, want nil", err)
	}
	if n := logs.Len(); n != 1 {
		t.Fatalf("log entries = %d, want 1: %v", n, logs.All())
	}
	if lvl := logs.All()[0].Level; lvl != zapcore.WarnLevel {
		t.Errorf("level = %v, want warn", lvl)
	}
}

func TestConcurrentDeliveriesLoseNothing(t *testing.T) {
	sink := new(consumertest.MetricsSink)
	p, err := NewFactory().CreateMetrics(context.Background(),
		processortest.NewNopSettings(NewFactory().Type()), &Config{Deliveries: 2, MaxWait: 5 * time.Millisecond}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Start(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				if err := p.ConsumeMetrics(context.Background(), oneResource("x")); err != nil {
					t.Errorf("ConsumeMetrics = %v", err)
				}
			}
		}()
	}
	wg.Wait()
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, n := range resourceCounts(sink) {
		total += n
	}
	if total != 800 {
		t.Fatalf("ResourceMetrics in sink = %d, want 800", total)
	}
}

func TestConfigValidate(t *testing.T) {
	for _, bad := range []Config{
		{Deliveries: 0, MaxWait: 10 * time.Second},
		{Deliveries: 2, MaxWait: 0},
		{Deliveries: -1, MaxWait: 10 * time.Second},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("Validate(%+v) = nil, want an error", bad)
		}
	}
	good := Config{Deliveries: 2, MaxWait: 10 * time.Second}
	if err := good.Validate(); err != nil {
		t.Errorf("Validate(%+v) = %v, want nil", good, err)
	}
}

func TestCapabilitiesMutatesData(t *testing.T) {
	p := newTestProcessor(t, 2, time.Hour, consumertest.NewNop())
	if !p.Capabilities().MutatesData {
		t.Error("Capabilities().MutatesData = false, want true")
	}
}

var _ component.Config = (*Config)(nil)
