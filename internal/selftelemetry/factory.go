// Package selftelemetry is the agent's telemetry factory: it builds the
// collector's console logger and leaves the meter and tracer providers as
// no-ops.
//
// The collector's stock factory (otelconftelemetry) is the only importer of
// go.opentelemetry.io/contrib/otelconf/x, which links the AWS SDK, the
// Kubernetes client, the GCP detectors and golang.org/x/oauth2 — about 15 MB
// of binary — while the shipped config turns its metrics and traces off and
// the agent uses only its logger. Spec:
// docs/superpowers/specs/2026-11-24-lean-agent-build-design.md (3.1).
package selftelemetry // import "github.com/monitorable-io/monitorable-agent/internal/selftelemetry"

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config/configtelemetry"
	"go.opentelemetry.io/collector/service/telemetry"
	"go.opentelemetry.io/otel"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Sampling of the stock logger: per message, per 10 s tick, the first 10
// entries and every 100th after that.
const (
	samplingTick       = 10 * time.Second
	samplingFirst      = 10
	samplingThereafter = 100
)

// Config is the service::telemetry section. Only logs::level, logs::encoding
// and metrics::level are read; every other key is accepted and kept aside so
// that a config written for the stock factory still loads.
type Config struct {
	Logs    LogsConfig     `mapstructure:"logs"`
	Metrics MetricsConfig  `mapstructure:"metrics"`
	Ignored map[string]any `mapstructure:",remain"`
}

// LogsConfig is service::telemetry::logs.
type LogsConfig struct {
	Level    zapcore.Level  `mapstructure:"level"`
	Encoding string         `mapstructure:"encoding"`
	Ignored  map[string]any `mapstructure:",remain"`
}

// MetricsConfig is service::telemetry::metrics. The level is read only to warn
// when it asks for internal metrics, which this build does not produce.
type MetricsConfig struct {
	Level   configtelemetry.Level `mapstructure:"level"`
	Ignored map[string]any        `mapstructure:",remain"`
}

var _ component.Config = (*Config)(nil)

// Validate accepts the two encodings zap ships.
func (c *Config) Validate() error {
	switch c.Logs.Encoding {
	case "console", "json":
		return nil
	}
	return fmt.Errorf("service::telemetry::logs::encoding %q: must be console or json", c.Logs.Encoding)
}

// IgnoredKeys lists the keys of the section that this build does not read,
// sorted: top-level keys bare, nested ones as "logs::<key>" or "metrics::<key>".
func (c *Config) IgnoredKeys() []string {
	var keys []string
	for k := range c.Ignored {
		keys = append(keys, k)
	}
	for k := range c.Logs.Ignored {
		keys = append(keys, "logs::"+k)
	}
	for k := range c.Metrics.Ignored {
		keys = append(keys, "metrics::"+k)
	}
	sort.Strings(keys)
	return keys
}

// NewFactory returns the agent's telemetry factory.
func NewFactory() telemetry.Factory {
	return telemetry.NewFactory(createDefaultConfig, telemetry.WithCreateLogger(createLogger))
}

func createDefaultConfig() component.Config {
	return &Config{
		Logs:    LogsConfig{Level: zapcore.InfoLevel, Encoding: "console"},
		Metrics: MetricsConfig{Level: configtelemetry.LevelNone},
	}
}

func createLogger(_ context.Context, set telemetry.LoggerSettings, componentConfig component.Config) (*zap.Logger, component.ShutdownFunc, error) {
	cfg, ok := componentConfig.(*Config)
	if !ok {
		return nil, nil, errors.New("selftelemetry: unexpected config type")
	}

	// The stock factory's zap config, minus the resource block it adds to
	// every line.
	ec := zap.NewProductionEncoderConfig()
	ec.EncodeTime = zapcore.ISO8601TimeEncoder
	zapCfg := zap.Config{
		Level:            zap.NewAtomicLevelAt(cfg.Logs.Level),
		Encoding:         cfg.Logs.Encoding,
		EncoderConfig:    ec,
		OutputPaths:      []string{"stderr"},
		ErrorOutputPaths: []string{"stderr"},
	}

	build := set.BuildZapLogger
	if build == nil {
		build = zap.Config.Build
	}
	logger, err := build(zapCfg)
	if err != nil {
		return nil, nil, err
	}

	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		logger.Error("OpenTelemetry internal telemetry error", zap.Error(err))
	}))

	// Warn before the sampler wraps the core, so these are never sampled away.
	if keys := cfg.IgnoredKeys(); len(keys) > 0 {
		logger.Warn("service::telemetry settings ignored: this build reads only logs::level, logs::encoding and metrics::level",
			zap.Strings("keys", keys))
	}
	if cfg.Metrics.Level != configtelemetry.LevelNone {
		logger.Warn("internal metrics are not available in this build; service::telemetry::metrics::level has no effect",
			zap.Stringer("level", cfg.Metrics.Level))
	}

	logger = logger.WithOptions(zap.WrapCore(func(core zapcore.Core) zapcore.Core {
		return zapcore.NewSamplerWithOptions(core, samplingTick, samplingFirst, samplingThereafter)
	}))
	return logger, nil, nil
}
