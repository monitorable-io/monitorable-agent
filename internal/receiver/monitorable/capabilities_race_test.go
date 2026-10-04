package monitorable

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.uber.org/zap"

	"github.com/monitorable-io/monitorable-agent/internal/capabilities"
	"github.com/monitorable-io/monitorable-agent/internal/collectors"
	"github.com/monitorable-io/monitorable-agent/internal/sensors"
)

// TestCapabilitiesRaceRedetectVsCollect pins b2: performCapabilityRedetection writes
// r.capabilities from the redetectCapabilities goroutine while collectMetrics's cycle
// (performUnifiedCollection) reads it every interval. Before the fix these were
// unsynchronized; `go test -race` must be clean running the two concurrently.
// This test discriminates only because performCapabilityRedetection writes
// r.capabilities unconditionally on every call — if it is ever changed back to
// "write only on change" (skip the store when DetectAll() returns an equal result),
// this test would pass silently without ever exercising the write path, and must be
// re-armed with a seam (e.g. a detector stub that always returns a changed value).
func TestCapabilitiesRaceRedetectVsCollect(t *testing.T) {
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := &metricsReceiver{
		cfg:          &Config{Docker: DockerConfig{Timeout: time.Second}, Smart: SmartConfig{Timeout: time.Second}, Systemd: SystemdConfig{Timeout: time.Second}},
		consumer:     consumertest.NewNop(),
		logger:       zap.NewNop(),
		capDetector:  capabilities.NewDetector(),
		capabilities: &capabilities.SystemCapabilities{CanReadTemperatures: false},
		collectors: &collectorSet{
			systemInfo: collectors.NewSystemInfoCollector(discard),
			hwSpec:     collectors.NewHardwareSpecCollector(),
			cloudInfo:  collectors.NewCloudInfoCollector(""),
			cpuTime:    collectors.NewCPUTimeCollector(),
		},
		tempCollector: sensors.NewTemperatureCollector("", "", nil, discard),
		dockerPrev:    collectors.DockerStatus(-1),
		systemdPrev:   collectors.SystemdStatus(-1),
		smartPrev:     collectors.SmartStatus(-1),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 200 {
			r.performUnifiedCollection(ctx)
		}
	}()
	go func() {
		defer wg.Done()
		for range 200 {
			r.performCapabilityRedetection()
		}
	}()
	wg.Wait()
}
