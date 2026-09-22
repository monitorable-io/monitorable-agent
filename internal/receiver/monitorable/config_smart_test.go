package monitorable

import (
	"testing"
	"time"
)

func TestValidate_SmartDefaults(t *testing.T) {
	cfg := &Config{}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if cfg.Smart.Mode != SmartModeAuto {
		t.Errorf("Smart.Mode = %q, want auto", cfg.Smart.Mode)
	}
	if cfg.Smart.Interval != 15*time.Minute {
		t.Errorf("Smart.Interval = %v, want 15m", cfg.Smart.Interval)
	}
	if cfg.Smart.Timeout != 10*time.Second {
		t.Errorf("Smart.Timeout = %v, want 10s", cfg.Smart.Timeout)
	}
}

func TestValidate_SmartInvalidMode(t *testing.T) {
	cfg := &Config{Smart: SmartConfig{Mode: "bogus"}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for invalid smart.mode")
	}
}
