package selftelemetry

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"go.opentelemetry.io/collector/config/configtelemetry"
	"go.opentelemetry.io/collector/confmap"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/service/telemetry"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestDefaultConfig(t *testing.T) {
	cfg := NewFactory().CreateDefaultConfig().(*Config)
	if cfg.Logs.Level != zapcore.InfoLevel || cfg.Logs.Encoding != "console" || cfg.Metrics.Level != configtelemetry.LevelNone {
		t.Fatalf("defaults = %+v", cfg)
	}
}

func TestUnknownKeysAreKeptAside(t *testing.T) {
	cfg := NewFactory().CreateDefaultConfig().(*Config)
	conf := confmap.NewFromStringMap(map[string]any{
		"logs":     map[string]any{"level": "debug", "sampling": map[string]any{"enabled": false}},
		"metrics":  map[string]any{"level": "detailed", "readers": []any{}},
		"traces":   map[string]any{"level": "none"},
		"resource": map[string]any{"service.name": "x"},
	})
	if err := conf.Unmarshal(cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Logs.Level != zapcore.DebugLevel || cfg.Metrics.Level != configtelemetry.LevelDetailed {
		t.Fatalf("known keys not read: %+v", cfg)
	}
	want := []string{"logs::sampling", "metrics::readers", "resource", "traces"}
	if got := cfg.IgnoredKeys(); !slices.Equal(got, want) {
		t.Fatalf("IgnoredKeys = %v, want %v", got, want)
	}
}

func TestValidateRejectsUnknownEncoding(t *testing.T) {
	cfg := NewFactory().CreateDefaultConfig().(*Config)
	cfg.Logs.Encoding = "xml"
	err := cfg.Validate()
	if err == nil {
		t.Fatal("encoding xml accepted")
	}
	// confmap prefixes the section path ("service::telemetry: ..."); the
	// message must not repeat it.
	if msg := err.Error(); !strings.HasPrefix(msg, `logs::encoding "xml"`) {
		t.Fatalf("error = %q, want it to start with the key relative to service::telemetry", msg)
	}
}

// observed builds the logger through the factory with a BuildZapLogger hook that
// records the zap.Config it is handed and returns a logger on an observer core.
func observed(t *testing.T, cfg *Config) (*zap.Logger, *observer.ObservedLogs, zap.Config) {
	t.Helper()
	core, logs := observer.New(zapcore.DebugLevel)
	var seen zap.Config
	res := pcommon.NewResource()
	res.Attributes().PutStr("service.name", "monitorable-agent")
	set := telemetry.LoggerSettings{
		Settings:       telemetry.Settings{Resource: &res},
		BuildZapLogger: func(zc zap.Config, _ ...zap.Option) (*zap.Logger, error) { seen = zc; return zap.New(core), nil },
	}
	logger, _, err := NewFactory().CreateLogger(context.Background(), set, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return logger, logs, seen
}

func TestLoggerSamplesAndCarriesNoResource(t *testing.T) {
	logger, logs, seen := observed(t, NewFactory().CreateDefaultConfig().(*Config))
	if seen.Encoding != "console" || !slices.Equal(seen.OutputPaths, []string{"stderr"}) ||
		!slices.Equal(seen.ErrorOutputPaths, []string{"stderr"}) || seen.Level.Level() != zapcore.InfoLevel {
		t.Fatalf("zap config = %+v", seen)
	}
	for i := 0; i < 200; i++ {
		logger.Info("same line")
	}
	if n := logs.Len(); n != 11 { // first 10, then the 110th
		t.Fatalf("200 identical lines produced %d entries, want 11", n)
	}
	for _, e := range logs.All() {
		if _, ok := e.ContextMap()["resource"]; ok {
			t.Fatal("log entry carries a resource field")
		}
	}
}

func TestWarnsOnceForIgnoredSettings(t *testing.T) {
	cfg := NewFactory().CreateDefaultConfig().(*Config)
	cfg.Ignored = map[string]any{"traces": map[string]any{}}
	cfg.Metrics.Level = configtelemetry.LevelDetailed
	_, logs, _ := observed(t, cfg)
	warns := logs.FilterLevelExact(zapcore.WarnLevel).All()
	if len(warns) != 2 {
		t.Fatalf("got %d warnings, want 2: %v", len(warns), warns)
	}
	if !strings.Contains(warns[0].Message+warns[1].Message, "internal metrics are not available in this build") {
		t.Fatalf("no metrics warning in %v", warns)
	}
	joined := fmt.Sprint(warns[0].ContextMap(), warns[1].ContextMap())
	if !strings.Contains(joined, "traces") {
		t.Fatalf("ignored key not named in %v", joined)
	}
}

func TestProvidersAreNoop(t *testing.T) {
	f, cfg := NewFactory(), NewFactory().CreateDefaultConfig()
	mp, err := f.CreateMeterProvider(context.Background(), telemetry.MeterSettings{}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := mp.Meter("x").(metricnoop.Meter); !ok {
		t.Fatalf("meter is %T, want the no-op meter", mp.Meter("x"))
	}
	tp, err := f.CreateTracerProvider(context.Background(), telemetry.TracerSettings{}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tp.Tracer("x").(tracenoop.Tracer); !ok {
		t.Fatalf("tracer is %T, want the no-op tracer", tp.Tracer("x"))
	}
}
