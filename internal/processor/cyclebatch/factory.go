// Package cyclebatch implements the cycle_batch metrics processor: it gathers
// the deliveries of one collection cycle (one per receiver) and sends them on
// as one pmetric.Metrics as soon as the cycle is complete, or after max_wait.
// Its clock starts at the first delivery of each group, so a cycle leaves
// seconds after it is collected, from the first cycle on.
//
// Spec: docs/superpowers/specs/2026-11-24-lean-agent-part2-design.md, section 4.
package cyclebatch

import (
	"context"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/processor"
)

var componentType = component.MustNewType("cycle_batch")

const (
	defaultDeliveries = 1
	defaultMaxWait    = 10 * time.Second
)

// NewFactory returns the cycle_batch processor factory.
func NewFactory() processor.Factory {
	return processor.NewFactory(componentType, createDefaultConfig,
		processor.WithMetrics(createMetrics, component.StabilityLevelDevelopment))
}

func createDefaultConfig() component.Config {
	return &Config{Deliveries: defaultDeliveries, MaxWait: defaultMaxWait}
}

func createMetrics(_ context.Context, set processor.Settings, cfg component.Config, next consumer.Metrics) (processor.Metrics, error) {
	c := cfg.(*Config)
	return newCycleBatch(set.Logger, c.Deliveries, c.MaxWait, next), nil
}
