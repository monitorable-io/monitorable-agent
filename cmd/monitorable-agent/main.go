package main

import (
	"log"

	"github.com/open-telemetry/opentelemetry-collector-contrib/extension/storage/filestorage"
	"github.com/open-telemetry/opentelemetry-collector-contrib/processor/resourceprocessor"
	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/hostmetricsreceiver"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/confmap"
	envprovider "go.opentelemetry.io/collector/confmap/provider/envprovider"
	fileprovider "go.opentelemetry.io/collector/confmap/provider/fileprovider"
	yamlprovider "go.opentelemetry.io/collector/confmap/provider/yamlprovider"
	"go.opentelemetry.io/collector/connector"
	"go.opentelemetry.io/collector/exporter"
	"go.opentelemetry.io/collector/exporter/debugexporter"
	"go.opentelemetry.io/collector/exporter/otlphttpexporter"
	"go.opentelemetry.io/collector/extension"
	"go.opentelemetry.io/collector/otelcol"
	"go.opentelemetry.io/collector/processor"
	"go.opentelemetry.io/collector/processor/batchprocessor"
	"go.opentelemetry.io/collector/receiver"
	"go.opentelemetry.io/collector/service/telemetry/otelconftelemetry"

	"github.com/monitorable-io/monitorable-agent/internal/receiver/monitorable"
	"github.com/monitorable-io/monitorable-agent/internal/version"
)

// newFactories is the single source of truth for the components compiled into
// this distribution (the binary is `go build ./cmd/monitorable-agent`).
func newFactories() otelcol.Factories {
	factories := otelcol.Factories{}

	// Set up receivers
	factories.Receivers = map[component.Type]receiver.Factory{
		component.MustNewType("host_metrics"): hostmetricsreceiver.NewFactory(),
		component.MustNewType("monitorable"):  monitorable.NewFactory(),
	}

	// Set up processors
	factories.Processors = map[component.Type]processor.Factory{
		component.MustNewType("batch"):    batchprocessor.NewFactory(),
		component.MustNewType("resource"): resourceprocessor.NewFactory(),
	}

	// Set up exporters
	factories.Exporters = map[component.Type]exporter.Factory{
		component.MustNewType("otlp_http"): otlphttpexporter.NewFactory(),
		// Kept for `--config` troubleshooting: an operator can add
		// `exporters: {debug: {verbosity: detailed}}` to see what is emitted. Not
		// referenced by the shipped distribution config.
		component.MustNewType("debug"): debugexporter.NewFactory(),
	}

	// Set up extensions
	factories.Extensions = map[component.Type]extension.Factory{
		// Persistent sending queue for otlp_http (distribution config: extensions.file_storage).
		component.MustNewType("file_storage"): filestorage.NewFactory(),
	}

	// Set up connectors (empty)
	factories.Connectors = map[component.Type]connector.Factory{}

	// Required by otelcol: telemetry factory must be set
	factories.Telemetry = otelconftelemetry.NewFactory()

	return factories
}

func main() {
	factories := newFactories()

	info := component.BuildInfo{
		Command:     "monitorable-agent",
		Description: "Monitorable agent (OpenTelemetry Collector distribution)",
		Version:     version.Version,
	}

	settings := otelcol.CollectorSettings{
		BuildInfo: info,
		Factories: func() (otelcol.Factories, error) { return factories, nil },
		ConfigProviderSettings: otelcol.ConfigProviderSettings{
			ResolverSettings: confmap.ResolverSettings{
				ProviderFactories: []confmap.ProviderFactory{
					envprovider.NewFactory(),
					fileprovider.NewFactory(),
					yamlprovider.NewFactory(),
				},
			},
		},
	}

	if err := run(settings); err != nil {
		log.Fatal(err)
	}
}

func run(settings otelcol.CollectorSettings) error {
	cmd := otelcol.NewCommand(settings)
	// otelcol.NewCommand sets SilenceUsage but leaves SilenceErrors at cobra's
	// default (false), so cmd.Execute() itself prints "Error: <err>" to stderr
	// before returning it here — main() would then log.Fatal(err) and print the
	// same failure a second time. Silencing cobra's own print keeps exactly one
	// copy of the error in the log.
	cmd.SilenceErrors = true
	return cmd.Execute()
}
