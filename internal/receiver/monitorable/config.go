package monitorable

import (
	"fmt"
	"time"

	"go.opentelemetry.io/collector/component"
)

// Docker collection modes.
const (
	DockerModeAuto = "auto" // detect each cycle; quiet when absent
	DockerModeOn   = "on"   // detect each cycle; WARN when absent (operator signal)
	DockerModeOff  = "off"  // never probe or emit

	defaultDockerEndpoint = "unix:///var/run/docker.sock"
	defaultDockerTimeout  = 5 * time.Second
)

// Systemd collection modes (same semantics as Docker).
const (
	SystemdModeAuto = "auto" // detect each cycle; quiet when absent
	SystemdModeOn   = "on"   // detect each cycle; WARN when absent (operator signal)
	SystemdModeOff  = "off"  // never probe or emit

	defaultSystemdInterval = 5 * time.Minute
	defaultSystemdTimeout  = 10 * time.Second
)

// SMART collection modes.
const (
	SmartModeAuto = "auto" // detect each cycle; quiet when no physical disk
	SmartModeOn   = "on"   // detect each cycle; WARN when no disk readable
	SmartModeOff  = "off"  // never probe or emit

	defaultSmartInterval = 15 * time.Minute
	defaultSmartTimeout  = 10 * time.Second
)

// SystemdConfig controls in-receiver systemd service-snapshot collection.
type SystemdConfig struct {
	Mode     string        `mapstructure:"mode"`     // auto | on | off
	Interval time.Duration `mapstructure:"interval"` // D-Bus sample cadence
	Timeout  time.Duration `mapstructure:"timeout"`  // bounds D-Bus calls per sample
}

// SmartConfig controls in-receiver SMART disk-health snapshot collection.
type SmartConfig struct {
	Mode     string        `mapstructure:"mode"`     // auto | on | off
	Interval time.Duration `mapstructure:"interval"` // device sample cadence
	Timeout  time.Duration `mapstructure:"timeout"`  // bounds the per-sample read
}

// DockerConfig controls in-receiver Docker container-metric collection.
type DockerConfig struct {
	Mode           string        `mapstructure:"mode"`            // auto | on | off
	Endpoint       string        `mapstructure:"endpoint"`        // docker socket
	ExcludedImages []string      `mapstructure:"excluded_images"` // wildcard match on image ref (* matches any chars)
	Timeout        time.Duration `mapstructure:"timeout"`         // bounds Docker calls per cycle
}

// Config defines configuration for the monitorable receiver
type Config struct {
	// Collection interval - unified for all metrics and metadata
	// This should normally be controlled by the batch processor configuration
	CollectionInterval time.Duration `mapstructure:"collection_interval"`
	CapabilityInterval time.Duration `mapstructure:"capability_interval"`

	// Temperature sensor configuration
	PrimarySensor  string   `mapstructure:"primary_sensor"`   // Primary sensor for dashboard display
	SensorFilter   []string `mapstructure:"sensor_filter"`    // Sensor whitelist/blacklist (prefix with '-' for blacklist)
	SysSensorsPath string   `mapstructure:"sys_sensors_path"` // Override /sys path for sensors (useful for containers)

	// Docker container snapshot (self-detecting; no overlay/launcher needed)
	Docker DockerConfig `mapstructure:"docker"`

	// Systemd service snapshot (self-detecting; sampled on a slow cadence)
	Systemd SystemdConfig `mapstructure:"systemd"`

	// SMART disk-health snapshot (self-detecting; sampled on a slow cadence)
	Smart SmartConfig `mapstructure:"smart"`
}

var _ component.Config = (*Config)(nil)

// Validate checks the receiver configuration is valid
func (cfg *Config) Validate() error {
	// Collection interval defaults to 60s if not specified
	if cfg.CollectionInterval <= 0 {
		cfg.CollectionInterval = 60 * time.Second
	}
	// Capability interval defaults to 24h if not specified
	if cfg.CapabilityInterval <= 0 {
		cfg.CapabilityInterval = 24 * time.Hour
	}
	// Feature flags default to enabled with graceful degradation

	// Docker collection config
	switch cfg.Docker.Mode {
	case "":
		cfg.Docker.Mode = DockerModeAuto
	case DockerModeAuto, DockerModeOn, DockerModeOff:
		// ok
	default:
		return fmt.Errorf("monitorable: invalid docker.mode %q (want auto|on|off)", cfg.Docker.Mode)
	}
	if cfg.Docker.Endpoint == "" {
		cfg.Docker.Endpoint = defaultDockerEndpoint
	}
	if cfg.Docker.Timeout <= 0 {
		cfg.Docker.Timeout = defaultDockerTimeout
	}
	switch cfg.Systemd.Mode {
	case "":
		cfg.Systemd.Mode = SystemdModeAuto
	case SystemdModeAuto, SystemdModeOn, SystemdModeOff:
		// ok
	default:
		return fmt.Errorf("monitorable: invalid systemd.mode %q (want auto|on|off)", cfg.Systemd.Mode)
	}
	if cfg.Systemd.Interval <= 0 {
		cfg.Systemd.Interval = defaultSystemdInterval
	}
	if cfg.Systemd.Timeout <= 0 {
		cfg.Systemd.Timeout = defaultSystemdTimeout
	}
	switch cfg.Smart.Mode {
	case "":
		cfg.Smart.Mode = SmartModeAuto
	case SmartModeAuto, SmartModeOn, SmartModeOff:
		// ok
	default:
		return fmt.Errorf("monitorable: invalid smart.mode %q (want auto|on|off)", cfg.Smart.Mode)
	}
	if cfg.Smart.Interval <= 0 {
		cfg.Smart.Interval = defaultSmartInterval
	}
	if cfg.Smart.Timeout <= 0 {
		cfg.Smart.Timeout = defaultSmartTimeout
	}
	return nil
}
