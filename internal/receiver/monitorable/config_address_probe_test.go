package monitorable

import (
	"testing"
	"time"
)

func TestAddressProbeConfigDefaultsAndLimits(t *testing.T) {
	cfg := createDefaultConfig().(*Config)
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.AddressProbe.Interval != time.Hour {
		t.Fatalf("default interval = %v, want 1h", cfg.AddressProbe.Interval)
	}
	cfg.AddressProbe.Interval = 4 * time.Minute
	if err := cfg.Validate(); err == nil {
		t.Fatal("an interval under 5m must be rejected")
	}
	cfg.AddressProbe.Interval = 5 * time.Minute
	if err := cfg.Validate(); err != nil {
		t.Fatalf("5m must be accepted: %v", err)
	}
}
