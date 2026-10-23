package monitorable

import (
	"testing"
	"time"
)

func TestValidatePortsDefaults(t *testing.T) {
	cfg := &Config{}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if cfg.Ports.Mode != PortsModeAuto {
		t.Errorf("Mode = %q, want %q", cfg.Ports.Mode, PortsModeAuto)
	}
	if cfg.Ports.SampleInterval != 10*time.Second {
		t.Errorf("SampleInterval = %v, want 10s", cfg.Ports.SampleInterval)
	}
	if cfg.Ports.MaxPorts != 30 {
		t.Errorf("MaxPorts = %d, want 30", cfg.Ports.MaxPorts)
	}
}

func TestCreateDefaultConfigPorts(t *testing.T) {
	cfg := createDefaultConfig().(*Config)
	if cfg.Ports.Mode != PortsModeAuto || cfg.Ports.SampleInterval != defaultPortsSampleInterval || cfg.Ports.MaxPorts != defaultPortsMaxPorts {
		t.Errorf("default ports config = %+v", cfg.Ports)
	}
}

func TestValidatePortsModeInvalid(t *testing.T) {
	cfg := &Config{Ports: PortsConfig{Mode: "bogus"}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() expected error for invalid ports.mode, got nil")
	}
}

func TestValidatePortsModesPreserved(t *testing.T) {
	for _, mode := range []string{PortsModeOn, PortsModeOff} {
		cfg := &Config{Ports: PortsConfig{Mode: mode}}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("mode %q: Validate() error = %v", mode, err)
		}
		if cfg.Ports.Mode != mode {
			t.Errorf("Mode = %q, want %q", cfg.Ports.Mode, mode)
		}
	}
}

func TestValidatePortsSampleIntervalBounds(t *testing.T) {
	for _, d := range []time.Duration{4 * time.Second, 61 * time.Second} {
		cfg := &Config{Ports: PortsConfig{SampleInterval: d}}
		if err := cfg.Validate(); err == nil {
			t.Errorf("sample_interval %v: expected error, got nil", d)
		}
	}
	for _, d := range []time.Duration{5 * time.Second, 60 * time.Second} {
		cfg := &Config{Ports: PortsConfig{SampleInterval: d}}
		if err := cfg.Validate(); err != nil {
			t.Errorf("sample_interval %v: unexpected error %v", d, err)
		}
		if cfg.Ports.SampleInterval != d {
			t.Errorf("SampleInterval = %v, want %v", cfg.Ports.SampleInterval, d)
		}
	}
}

func TestValidatePortsMaxPortsBounds(t *testing.T) {
	cfg := &Config{Ports: PortsConfig{MaxPorts: 0}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("max_ports 0: %v", err)
	}
	if cfg.Ports.MaxPorts != 30 {
		t.Errorf("MaxPorts = %d, want default 30", cfg.Ports.MaxPorts)
	}
	for _, n := range []int{101, -1} {
		cfg = &Config{Ports: PortsConfig{MaxPorts: n}}
		if err := cfg.Validate(); err == nil {
			t.Errorf("max_ports %d: expected error, got nil", n)
		}
	}
	cfg = &Config{Ports: PortsConfig{MaxPorts: 100}}
	if err := cfg.Validate(); err != nil {
		t.Errorf("max_ports 100: unexpected error %v", err)
	}
}
