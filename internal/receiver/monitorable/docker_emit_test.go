package monitorable

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
	"go.uber.org/zap/zaptest/observer"

	"github.com/monitorable-io/monitorable-agent/internal/collectors"
)

func discardSlogLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func dockerAttrMap() (pmetric.Metrics, func() (string, bool)) {
	md := pmetric.NewMetrics()
	attrs := md.ResourceMetrics().AppendEmpty().Resource().Attributes()
	return md, func() (string, bool) {
		v, ok := attrs.Get("docker.containers")
		if !ok {
			return "", false
		}
		return v.Str(), true
	}
}

func TestCollectDockerNoopWhenCollectorNil(t *testing.T) {
	r := &metricsReceiver{} // dockerCollector nil (docker.mode=off)
	md, get := dockerAttrMap()
	r.collectDockerContainers(context.Background(), md.ResourceMetrics().At(0).Resource().Attributes())
	if _, ok := get(); ok {
		t.Fatal("attribute set despite nil collector")
	}
}

func TestCollectDockerNoAttrWhenAbsent(t *testing.T) {
	r := &metricsReceiver{
		dockerCollector: collectors.NewDockerCollector("unix:///nonexistent.sock", nil, discardSlogLogger()),
		dockerPrev:      collectors.DockerStatus(-1),
		cfg:             &Config{Docker: DockerConfig{Timeout: time.Second}},
		logger:          zap.NewNop(),
	}
	md, get := dockerAttrMap()
	r.collectDockerContainers(context.Background(), md.ResourceMetrics().At(0).Resource().Attributes())
	if _, ok := get(); ok {
		t.Fatal("attribute set despite absent docker daemon")
	}
}

func TestLogDockerTransitionOnlyOnChange(t *testing.T) {
	r := &metricsReceiver{logger: zaptest.NewLogger(t), dockerMode: DockerModeAuto, dockerPrev: collectors.DockerStatus(-1)}
	r.logDockerTransition(collectors.DockerPresent)
	if r.dockerPrev != collectors.DockerPresent {
		t.Fatalf("dockerPrev = %v, want DockerPresent", r.dockerPrev)
	}
	// Same status again — still DockerPresent, no panic, no state change.
	r.logDockerTransition(collectors.DockerPresent)
	if r.dockerPrev != collectors.DockerPresent {
		t.Fatalf("dockerPrev changed unexpectedly")
	}
	r.logDockerTransition(collectors.DockerAbsent)
	if r.dockerPrev != collectors.DockerAbsent {
		t.Fatalf("dockerPrev = %v, want DockerAbsent", r.dockerPrev)
	}
}

func TestLogDockerTransitionMessages(t *testing.T) {
	// mode=auto: NoPermission emits the actionable WARN; Absent stays quiet (debug).
	core, logs := observer.New(zap.DebugLevel)
	r := &metricsReceiver{logger: zap.New(core), dockerMode: DockerModeAuto, dockerPrev: collectors.DockerStatus(-1)}

	r.logDockerTransition(collectors.DockerNoPermission)
	if logs.FilterMessageSnippet("usermod -aG docker").Len() != 1 {
		t.Errorf("NoPermission should emit an actionable WARN with the remediation command")
	}

	r.logDockerTransition(collectors.DockerAbsent)
	if logs.FilterLevelExact(zap.WarnLevel).FilterMessageSnippet("docker.mode=on").Len() != 0 {
		t.Errorf("auto-mode Absent must not warn about mode=on")
	}

	// mode=on: Absent escalates to a WARN (operator expected Docker).
	core2, logs2 := observer.New(zap.DebugLevel)
	r2 := &metricsReceiver{logger: zap.New(core2), dockerMode: DockerModeOn, dockerPrev: collectors.DockerStatus(-1)}
	r2.logDockerTransition(collectors.DockerAbsent)
	if logs2.FilterLevelExact(zap.WarnLevel).Len() != 1 {
		t.Errorf("mode=on Absent should emit a WARN")
	}
}
