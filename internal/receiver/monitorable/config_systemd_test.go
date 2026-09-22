package monitorable

import (
	"testing"
	"time"
)

func TestSystemdConfigDefaults(t *testing.T) {
	cfg := &Config{}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if cfg.Systemd.Mode != SystemdModeAuto {
		t.Errorf("Systemd.Mode = %q, want %q", cfg.Systemd.Mode, SystemdModeAuto)
	}
	if cfg.Systemd.Interval != 5*time.Minute {
		t.Errorf("Systemd.Interval = %v, want 5m", cfg.Systemd.Interval)
	}
	if cfg.Systemd.Timeout != 10*time.Second {
		t.Errorf("Systemd.Timeout = %v, want 10s", cfg.Systemd.Timeout)
	}
}

func TestSystemdConfigInvalidMode(t *testing.T) {
	cfg := &Config{Systemd: SystemdConfig{Mode: "bogus"}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() = nil, want error for invalid systemd.mode")
	}
}
