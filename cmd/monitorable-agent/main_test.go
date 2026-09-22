package main

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/otelcol"
)

// TestFactoriesRegisterShippedComponents pins the component set the
// distribution config relies on. The config-load test (config_test.go) is the
// end-to-end check; this one names the expectation explicitly.
func TestFactoriesRegisterShippedComponents(t *testing.T) {
	f := newFactories()

	for _, name := range []string{"host_metrics", "monitorable"} {
		if _, ok := f.Receivers[component.MustNewType(name)]; !ok {
			t.Errorf("receiver %q is not registered", name)
		}
	}
	for _, name := range []string{"batch", "resource"} {
		if _, ok := f.Processors[component.MustNewType(name)]; !ok {
			t.Errorf("processor %q is not registered", name)
		}
	}
	if _, ok := f.Exporters[component.MustNewType("otlp_http")]; !ok {
		t.Error("exporter \"otlp_http\" is not registered")
	}
	// Persistent sending queue for otlp_http (distribution config: extensions.file_storage).
	if _, ok := f.Extensions[component.MustNewType("file_storage")]; !ok {
		t.Error("extension \"file_storage\" is not registered; the shipped persistent queue would fail to start")
	}
	if _, ok := f.Extensions[component.MustNewType("health_check")]; ok {
		t.Error("extension \"health_check\" is registered; nothing uses it and it binds a port when configured")
	}
	if f.Telemetry == nil {
		t.Error("telemetry factory is nil; otelcol requires it")
	}
}

// TestRunDoesNotDoublePrintError pins b13: otelcol.NewCommand's cobra root leaves
// SilenceErrors at cobra's default (false), so cmd.Execute() would print
// "Error: <err>" to stderr itself before returning the error to main(), which then
// logs the same failure again via log.Fatal — an operator reading journalctl sees the
// startup failure twice. run() must silence cobra's own print so the caller is the
// only one that prints the error.
func TestRunDoesNotDoublePrintError(t *testing.T) {
	origArgs := os.Args
	t.Cleanup(func() { os.Args = origArgs })
	os.Args = []string{"monitorable-agent", "--this-flag-does-not-exist"}

	settings := otelcol.CollectorSettings{
		BuildInfo: component.BuildInfo{Command: "monitorable-agent"},
		Factories: func() (otelcol.Factories, error) { return newFactories(), nil },
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	origStderr := os.Stderr
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = origStderr })

	runErr := run(settings)

	w.Close()
	var buf bytes.Buffer
	io.Copy(&buf, r)

	if runErr == nil {
		t.Fatal("run() with an unknown flag returned nil error, want non-nil")
	}
	if strings.Contains(buf.String(), "Error:") {
		t.Errorf("run() printed its own \"Error:\" line to stderr (cobra's SilenceErrors not set); "+
			"main() would print the same error again via log.Fatal — got: %q", buf.String())
	}
}
