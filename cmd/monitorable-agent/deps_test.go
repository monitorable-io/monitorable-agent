package main

import (
	"os/exec"
	"strings"
	"testing"
)

// TestBinaryLinksNoStockTelemetry fails when the agent's import graph regains the
// collector's stock telemetry factory or anything only it pulled in (45 modules,
// 15 MB of binary in v1.7.0).
func TestBinaryLinksNoStockTelemetry(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	forbidden := []string{
		"go.opentelemetry.io/collector/service/telemetry/otelconftelemetry",
		"go.opentelemetry.io/contrib/otelconf/x",
		"github.com/aws/", "k8s.io/", "sigs.k8s.io/", "github.com/GoogleCloudPlatform/", "golang.org/x/oauth2",
	}
	for _, pkg := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		for _, f := range forbidden {
			if strings.HasPrefix(pkg, f) {
				t.Errorf("binary imports %s", pkg)
			}
		}
	}
}
