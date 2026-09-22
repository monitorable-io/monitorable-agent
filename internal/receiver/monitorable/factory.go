package monitorable

import (
	"context"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/receiver"
)

const (
	typeStr   = "monitorable"
	stability = component.StabilityLevelDevelopment
)

// NewFactory creates a new factory for the monitorable receiver
func NewFactory() receiver.Factory {
	return receiver.NewFactory(
		component.MustNewType(typeStr),
		createDefaultConfig,
		receiver.WithMetrics(createMetricsReceiver, stability),
	)
}

func createDefaultConfig() component.Config {
	return &Config{
		CollectionInterval: 60 * time.Second, // Default unified collection interval
		CapabilityInterval: 24 * time.Hour,   // Capability re-detection interval
		Docker: DockerConfig{
			Mode:     DockerModeAuto,
			Endpoint: defaultDockerEndpoint,
			Timeout:  defaultDockerTimeout,
		},
		Systemd: SystemdConfig{
			Mode:     SystemdModeAuto,
			Interval: defaultSystemdInterval,
			Timeout:  defaultSystemdTimeout,
		},
	}
}

func createMetricsReceiver(
	ctx context.Context,
	set receiver.Settings,
	cfg component.Config,
	consumer consumer.Metrics,
) (receiver.Metrics, error) {
	rCfg := cfg.(*Config)
	return newMetricsReceiver(set, rCfg, consumer)
}
